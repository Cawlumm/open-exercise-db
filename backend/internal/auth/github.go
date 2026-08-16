package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// GitHub's authorization-code endpoints. Overridable so tests can point at a
// stub rather than the internet — the alternative is an OAuth flow nothing
// exercises until it is deployed, which is where OAuth bugs are most expensive
// to find.
type GitHubEndpoints struct {
	AuthorizeURL string
	TokenURL     string
	UserURL      string
}

// DefaultGitHubEndpoints are the real ones.
func DefaultGitHubEndpoints() GitHubEndpoints {
	return GitHubEndpoints{
		AuthorizeURL: "https://github.com/login/oauth/authorize",
		TokenURL:     "https://github.com/login/oauth/access_token",
		UserURL:      "https://api.github.com/user",
	}
}

// GitHubOAuth performs the authorization-code flow against GitHub.
type GitHubOAuth struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Endpoints    GitHubEndpoints
	HTTPClient   *http.Client
}

// GitHubUser is the subset of GitHub's user payload this project stores.
// Nothing else is read: an email would be personal data with no use here, and
// the less of it that exists the less there is to leak or to justify holding.
type GitHubUser struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	AvatarURL string `json:"avatar_url"`
}

var (
	// ErrOAuthNotConfigured means the client id/secret are absent — a
	// deployment choice, not a user error, so sign-in reports it as
	// unavailable rather than as a failed login.
	ErrOAuthNotConfigured = errors.New("github oauth is not configured")

	// ErrInvalidState is a callback whose state parameter does not verify:
	// either a CSRF attempt or a stale sign-in.
	ErrInvalidState = errors.New("invalid oauth state")

	ErrExchangeFailed = errors.New("github rejected the authorization code")
)

// Configured reports whether sign-in is available.
func (g GitHubOAuth) Configured() bool {
	return g.ClientID != "" && g.ClientSecret != "" && g.RedirectURL != ""
}

// AuthorizeURL builds the URL to send a browser to, carrying a signed state.
//
// State is signed rather than stored. The usual approach — generate a random
// value, keep it in a session, compare on callback — needs server-side session
// storage this service does not have, and would not survive Cloud Run scaling
// to a second instance mid-sign-in. An HMAC of a nonce plus an issue time,
// keyed on the same secret that signs session tokens, is verifiable by any
// instance without shared state, which is the property that actually matters
// here.
func (g GitHubOAuth) AuthorizeURL(secret string, now time.Time) (string, error) {
	if !g.Configured() {
		return "", ErrOAuthNotConfigured
	}
	state, err := newState(secret, now)
	if err != nil {
		return "", err
	}

	q := url.Values{}
	q.Set("client_id", g.ClientID)
	q.Set("redirect_uri", g.RedirectURL)
	// No scopes requested. The default grant already exposes the public
	// profile this project stores, and asking for more would present the user
	// with a consent screen listing permissions we would never use.
	q.Set("scope", "")
	q.Set("state", state)
	q.Set("allow_signup", "false")

	return g.Endpoints.AuthorizeURL + "?" + q.Encode(), nil
}

// Exchange trades an authorization code for the GitHub user behind it.
func (g GitHubOAuth) Exchange(ctx context.Context, code string) (GitHubUser, error) {
	if !g.Configured() {
		return GitHubUser{}, ErrOAuthNotConfigured
	}

	token, err := g.exchangeCode(ctx, code)
	if err != nil {
		return GitHubUser{}, err
	}
	return g.fetchUser(ctx, token)
}

func (g GitHubOAuth) client() *http.Client {
	if g.HTTPClient != nil {
		return g.HTTPClient
	}
	// A bounded client rather than http.DefaultClient: this call is made
	// while a user waits on a redirect, and DefaultClient has no timeout at
	// all, so a hung GitHub would hold the request until Cloud Run's own
	// timeout rather than failing in seconds.
	return &http.Client{Timeout: 10 * time.Second}
}

func (g GitHubOAuth) exchangeCode(ctx context.Context, code string) (string, error) {
	form := url.Values{}
	form.Set("client_id", g.ClientID)
	form.Set("client_secret", g.ClientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", g.RedirectURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.Endpoints.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := g.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("exchange code: %w", err)
	}
	defer resp.Body.Close()

	// Bounded read: this is a third-party response, and an unbounded
	// io.ReadAll on one is a memory exhaustion vector we do not control.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: status %d", ErrExchangeFailed, resp.StatusCode)
	}

	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	// GitHub reports a bad code as HTTP 200 with an error field, so status
	// alone is not enough to tell success from failure here.
	if out.Error != "" || out.AccessToken == "" {
		return "", ErrExchangeFailed
	}
	return out.AccessToken, nil
}

func (g GitHubOAuth) fetchUser(ctx context.Context, accessToken string) (GitHubUser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.Endpoints.UserURL, nil)
	if err != nil {
		return GitHubUser{}, fmt.Errorf("build user request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := g.client().Do(req)
	if err != nil {
		return GitHubUser{}, fmt.Errorf("fetch github user: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return GitHubUser{}, fmt.Errorf("read github user: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return GitHubUser{}, fmt.Errorf("%w: user status %d", ErrExchangeFailed, resp.StatusCode)
	}

	var u GitHubUser
	if err := json.Unmarshal(body, &u); err != nil {
		return GitHubUser{}, fmt.Errorf("decode github user: %w", err)
	}
	if u.ID == 0 || u.Login == "" {
		return GitHubUser{}, fmt.Errorf("%w: github returned no identity", ErrExchangeFailed)
	}
	return u, nil
}

// stateTTL bounds how long an in-flight sign-in stays valid. Long enough to
// read a consent screen, short enough that a state captured from a browser's
// history or a referrer log is useless later.
const stateTTL = 10 * time.Minute

// newState builds an HMAC-signed "<nonce>.<unix>.<mac>".
func newState(secret string, now time.Time) (string, error) {
	if secret == "" {
		return "", ErrNoSecret
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate oauth state: %w", err)
	}
	body := b64(nonce) + "." + strconv.FormatInt(now.Unix(), 10)
	return body + "." + b64(stateMAC(secret, body)), nil
}

// VerifyState checks a callback's state parameter.
//
// Constant-time, and expiry-checked. A state that verifies but is older than
// stateTTL is rejected: without that, a signed state is a permanent CSRF
// token, which is the thing state exists to prevent.
func VerifyState(secret, state string, now time.Time) error {
	if secret == "" {
		return ErrNoSecret
	}
	idx := strings.LastIndex(state, ".")
	if idx < 0 {
		return ErrInvalidState
	}
	body, macPart := state[:idx], state[idx+1:]

	got, err := base64.RawURLEncoding.DecodeString(macPart)
	if err != nil || !hmac.Equal(stateMAC(secret, body), got) {
		return ErrInvalidState
	}

	_, tsPart, ok := strings.Cut(body, ".")
	if !ok {
		return ErrInvalidState
	}
	ts, err := strconv.ParseInt(tsPart, 10, 64)
	if err != nil {
		return ErrInvalidState
	}
	issued := time.Unix(ts, 0)
	if now.Before(issued.Add(-time.Minute)) || now.After(issued.Add(stateTTL)) {
		return ErrInvalidState
	}
	return nil
}

func stateMAC(secret, body string) []byte {
	mac := hmac.New(sha256.New, []byte("oauth-state:"+secret))
	mac.Write([]byte(body))
	return mac.Sum(nil)
}
