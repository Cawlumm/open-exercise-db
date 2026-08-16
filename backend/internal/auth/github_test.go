package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// stubGitHub stands in for GitHub's two endpoints, so the whole
// authorization-code flow runs in tests. Without it the flow's first real
// execution would be against production, which is where OAuth mistakes are
// most expensive to find.
func stubGitHub(t *testing.T, opts stubOpts) (GitHubEndpoints, *stubState) {
	t.Helper()
	st := &stubState{}

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		st.TokenCalls++
		_ = r.ParseForm()
		st.LastCode = r.FormValue("code")
		st.LastRedirect = r.FormValue("redirect_uri")
		st.SentSecret = r.FormValue("client_secret")

		w.Header().Set("Content-Type", "application/json")
		switch {
		case opts.tokenStatus != 0:
			w.WriteHeader(opts.tokenStatus)
			_, _ = w.Write([]byte(`{}`))
		case opts.tokenErrorBody:
			// GitHub reports a bad code as HTTP 200 with an error field.
			_, _ = w.Write([]byte(`{"error":"bad_verification_code"}`))
		default:
			_, _ = w.Write([]byte(`{"access_token":"gho_stub","token_type":"bearer"}`))
		}
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		st.UserCalls++
		st.UserAuthHeader = r.Header.Get("Authorization")

		w.Header().Set("Content-Type", "application/json")
		switch {
		case opts.userStatus != 0:
			w.WriteHeader(opts.userStatus)
			_, _ = w.Write([]byte(`{}`))
		case opts.userNoIdentity:
			_, _ = w.Write([]byte(`{"id":0,"login":""}`))
		default:
			_, _ = w.Write([]byte(`{"id":4242,"login":"Cawlumm","avatar_url":"https://example.test/a.png"}`))
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return GitHubEndpoints{
		AuthorizeURL: srv.URL + "/authorize",
		TokenURL:     srv.URL + "/token",
		UserURL:      srv.URL + "/user",
	}, st
}

type stubOpts struct {
	tokenStatus    int
	tokenErrorBody bool
	userStatus     int
	userNoIdentity bool
}

type stubState struct {
	TokenCalls     int
	UserCalls      int
	LastCode       string
	LastRedirect   string
	SentSecret     string
	UserAuthHeader string
}

func newTestOAuth(t *testing.T, opts stubOpts) (GitHubOAuth, *stubState) {
	t.Helper()
	eps, st := stubGitHub(t, opts)
	return GitHubOAuth{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		RedirectURL:  "https://api.example.test/api/v1/auth/github/callback",
		Endpoints:    eps,
	}, st
}

func TestGitHubOAuth_ExchangeReturnsIdentity(t *testing.T) {
	g, st := newTestOAuth(t, stubOpts{})

	user, err := g.Exchange(context.Background(), "the-code")
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if user.ID != 4242 || user.Login != "Cawlumm" {
		t.Errorf("user = %+v, want id 4242 / Cawlumm", user)
	}
	if st.LastCode != "the-code" {
		t.Errorf("code sent = %q, want the-code", st.LastCode)
	}
	if st.LastRedirect != g.RedirectURL {
		t.Errorf("redirect_uri sent = %q, want %q", st.LastRedirect, g.RedirectURL)
	}
	if st.UserAuthHeader != "Bearer gho_stub" {
		t.Errorf("user request auth = %q, want the exchanged token", st.UserAuthHeader)
	}
}

func TestGitHubOAuth_BadCodeIsRejected(t *testing.T) {
	// The case that matters: GitHub answers 200 with an error field, so a
	// handler checking only the status code would treat a rejected code as a
	// successful login with an empty token.
	g, _ := newTestOAuth(t, stubOpts{tokenErrorBody: true})

	if _, err := g.Exchange(context.Background(), "bad"); !errors.Is(err, ErrExchangeFailed) {
		t.Fatalf("err = %v, want ErrExchangeFailed", err)
	}
}

func TestGitHubOAuth_UpstreamFailures(t *testing.T) {
	for name, opts := range map[string]stubOpts{
		"token endpoint 500": {tokenStatus: http.StatusInternalServerError},
		"user endpoint 401":  {userStatus: http.StatusUnauthorized},
		"user without id":    {userNoIdentity: true},
	} {
		g, _ := newTestOAuth(t, opts)
		if _, err := g.Exchange(context.Background(), "code"); err == nil {
			t.Errorf("%s: exchange succeeded, want an error", name)
		}
	}
}

func TestGitHubOAuth_NotConfigured(t *testing.T) {
	var g GitHubOAuth
	if _, err := g.Exchange(context.Background(), "code"); !errors.Is(err, ErrOAuthNotConfigured) {
		t.Errorf("exchange err = %v, want ErrOAuthNotConfigured", err)
	}
	if _, err := g.AuthorizeURL(testSecret, testNow); !errors.Is(err, ErrOAuthNotConfigured) {
		t.Errorf("authorize err = %v, want ErrOAuthNotConfigured", err)
	}
}

func TestGitHubOAuth_AuthorizeURLCarriesStateAndRedirect(t *testing.T) {
	g, _ := newTestOAuth(t, stubOpts{})

	raw, err := g.AuthorizeURL(testSecret, testNow)
	if err != nil {
		t.Fatalf("authorize url: %v", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q := u.Query()
	if q.Get("client_id") != "client-id" {
		t.Errorf("client_id = %q", q.Get("client_id"))
	}
	if q.Get("redirect_uri") != g.RedirectURL {
		t.Errorf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	if q.Get("state") == "" {
		t.Fatal("no state parameter")
	}
	// The client secret must never appear in a URL a browser will follow.
	if strings.Contains(raw, "client-secret") {
		t.Error("authorize URL leaks the client secret")
	}
	if err := VerifyState(testSecret, q.Get("state"), testNow); err != nil {
		t.Errorf("own state does not verify: %v", err)
	}
}

func TestOAuthState_RejectsForgedAndStale(t *testing.T) {
	good, err := newState(testSecret, testNow)
	if err != nil {
		t.Fatalf("state: %v", err)
	}

	if err := VerifyState(testSecret, good, testNow); err != nil {
		t.Fatalf("fresh state rejected: %v", err)
	}

	// A signed state that never expires is a permanent CSRF token, which is
	// the thing state exists to prevent.
	if err := VerifyState(testSecret, good, testNow.Add(stateTTL+time.Minute)); !errors.Is(err, ErrInvalidState) {
		t.Errorf("stale state accepted (err = %v)", err)
	}

	for name, bad := range map[string]string{
		"empty":          "",
		"no mac":         "nonce.123",
		"wrong mac":      strings.Split(good, ".")[0] + "." + strings.Split(good, ".")[1] + ".AAAA",
		"other secret":   mustState(t, "a-different-secret"),
		"tampered stamp": tamperTimestamp(good),
	} {
		if err := VerifyState(testSecret, bad, testNow); !errors.Is(err, ErrInvalidState) {
			t.Errorf("%s state accepted (err = %v)", name, err)
		}
	}
}

func mustState(t *testing.T, secret string) string {
	t.Helper()
	s, err := newState(secret, testNow)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	return s
}

// tamperTimestamp rewrites the issue time while keeping the original MAC —
// the way an attacker would try to extend a captured state's life.
func tamperTimestamp(state string) string {
	parts := strings.Split(state, ".")
	if len(parts) != 3 {
		return state
	}
	return parts[0] + ".9999999999." + parts[2]
}
