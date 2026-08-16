package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Cawlumm/open-exercise-db/internal/auth"
	"github.com/Cawlumm/open-exercise-db/internal/export"
	"github.com/Cawlumm/open-exercise-db/internal/media"
	"github.com/Cawlumm/open-exercise-db/internal/ratelimit"
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

const signInSecret = "sign-in-test-secret"

// fakeGitHub serves the two endpoints the OAuth flow calls, so sign-in is
// exercised end to end rather than only from the callback inwards.
func fakeGitHub(t *testing.T, login string, id int64) auth.GitHubEndpoints {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"gho_fake"}`))
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":` + strconv.FormatInt(id, 10) + `,"login":"` + login + `","avatar_url":""}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return auth.GitHubEndpoints{
		AuthorizeURL: srv.URL + "/authorize",
		TokenURL:     srv.URL + "/token",
		UserURL:      srv.URL + "/user",
	}
}

// newSignInAPI wires Register with sign-in configured against a fake GitHub.
func newSignInAPI(t *testing.T, adminLogins, ghLogin string, ghID int64) (*store.Store, *pgxpool.Pool, humatest.TestAPI) {
	t.Helper()
	s, pool := newTestStore(t)

	limiter := ratelimit.NewLimiter(ratelimit.Config{
		Read: ratelimit.Rule{RPM: 600, Burst: 200}, Search: ratelimit.Rule{RPM: 600, Burst: 200},
		Export: ratelimit.Rule{RPM: 600, Burst: 200}, Unidentified: ratelimit.Rule{RPM: 600, Burst: 200},
	})
	t.Cleanup(limiter.Close)

	cache := export.NewCache(export.DefaultCacheTTL, "")
	mediaStore, err := media.NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatalf("media store: %v", err)
	}

	_, hapi := humatest.New(t)
	Register(hapi, s, cache, limiter, mediaStore, "", AuthConfig{
		JWTSecret:         signInSecret,
		AdminGitHubLogins: adminLogins,
		GitHubOAuth: auth.GitHubOAuth{
			ClientID:     "cid",
			ClientSecret: "csecret",
			RedirectURL:  "https://api.example.test/api/v1/auth/github/callback",
			Endpoints:    fakeGitHub(t, ghLogin, ghID),
		},
	})
	return s, pool, hapi.(humatest.TestAPI)
}

// startSignIn follows the real redirect and pulls the state out of it, so the
// tests exercise the same state this API issues rather than one a test hook
// manufactured. A test-only minting helper would have been the easy path and
// would have stopped covering the half of the flow that produces state at all.
func startSignIn(t *testing.T, api humatest.TestAPI) string {
	t.Helper()
	resp := api.Get("/api/v1/auth/github", "User-Agent: "+testUA)
	if resp.Code != http.StatusFound {
		t.Fatalf("start sign-in status = %d, want 302: %s", resp.Code, resp.Body.String())
	}
	loc := resp.Header().Get("Location")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse Location %q: %v", loc, err)
	}
	state := u.Query().Get("state")
	if state == "" {
		t.Fatalf("redirect carries no state: %s", loc)
	}
	return state
}

// signIn completes the OAuth round trip and returns the session token, the
// user id, and the role.
func signIn(t *testing.T, api humatest.TestAPI) (token, userID, role string) {
	t.Helper()
	state := startSignIn(t, api)

	resp := api.Get("/api/v1/auth/github/callback?code=abc&state="+url.QueryEscape(state), "User-Agent: "+testUA)
	if resp.Code != http.StatusOK {
		t.Fatalf("callback status = %d, want 200: %s", resp.Code, resp.Body.String())
	}
	var out struct {
		Token string `json:"token"`
		User  struct {
			ID   string `json:"id"`
			Role string `json:"role"`
		} `json:"user"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out.Token, out.User.ID, out.User.Role
}

func TestSignIn_IssuesUsableSessionToken(t *testing.T) {
	_, _, api := newSignInAPI(t, "", "someone", 100)

	token, _, role := signIn(t, api)
	if token == "" {
		t.Fatal("no session token issued")
	}
	if role != "contributor" {
		t.Errorf("role = %q, want contributor for a login not on the admin list", role)
	}

	resp := api.Get("/api/v1/me", "User-Agent: "+testUA, "Authorization: Bearer "+token)
	if resp.Code != http.StatusOK {
		t.Fatalf("/me status = %d, want 200: %s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	if !strings.Contains(body, `"username":"someone"`) {
		t.Errorf("/me does not describe the signed-in person: %s", body)
	}
	// GitHub's account id identifies the person on another service and
	// nothing here needs it.
	if strings.Contains(body, "github_id") {
		t.Errorf("/me leaks github_id: %s", body)
	}
}

func TestSignIn_PromotesConfiguredAdmin(t *testing.T) {
	// Deliberately different case: GitHub usernames are case-insensitive, and
	// an operator who typed the wrong case would otherwise get a silent
	// non-promotion with nothing to indicate why.
	_, _, api := newSignInAPI(t, "Cawlumm, someone-else", "cawlumm", 200)

	if _, _, role := signIn(t, api); role != "admin" {
		t.Fatalf("role = %q, want admin", role)
	}
}

func TestSignIn_DoesNotDemoteOnSubsequentLogin(t *testing.T) {
	s, pool, api := newSignInAPI(t, "", "someone", 250)

	_, userID, _ := signIn(t, api)
	if _, err := pool.Exec(context.Background(),
		`UPDATE users SET role = 'moderator' WHERE id = $1`, userID); err != nil {
		t.Fatalf("promote: %v", err)
	}

	// Signing in again must not reset the role. Otherwise an empty or
	// mistyped ADMIN_GITHUB_LOGINS silently strips everyone's role the next
	// time they log in — precisely when nobody is watching.
	signIn(t, api)

	user, err := s.Users.GetByID(context.Background(), userID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if user.Role != "moderator" {
		t.Errorf("role after re-login = %q, want moderator (login must not demote)", user.Role)
	}
}

func TestSignIn_RejectsForgedOrMissingState(t *testing.T) {
	_, _, api := newSignInAPI(t, "", "someone", 300)

	// CSRF: a callback no sign-in of ours started.
	for name, state := range map[string]string{
		"absent": "",
		"forged": "aaaa.111.bbbb",
		"junk":   "not-a-state",
	} {
		resp := api.Get("/api/v1/auth/github/callback?code=abc&state="+url.QueryEscape(state), "User-Agent: "+testUA)
		if resp.Code != http.StatusBadRequest {
			t.Errorf("%s state: status = %d, want 400: %s", name, resp.Code, resp.Body.String())
		}
	}
}

func TestSignIn_UnconfiguredReports503(t *testing.T) {
	s, _ := newTestStore(t)
	limiter := ratelimit.NewLimiter(ratelimit.Config{
		Read: ratelimit.Rule{RPM: 600, Burst: 200}, Export: ratelimit.Rule{RPM: 600, Burst: 200},
	})
	t.Cleanup(limiter.Close)
	cache := export.NewCache(export.DefaultCacheTTL, "")
	mediaStore, err := media.NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatalf("media store: %v", err)
	}
	_, hapi := humatest.New(t)
	Register(hapi, s, cache, limiter, mediaStore, "", AuthConfig{})

	// A self-hoster who wants no sign-in gets a working read API and a clear
	// "this feature is off here", not a 500.
	resp := hapi.(humatest.TestAPI).Get("/api/v1/auth/github", "User-Agent: "+testUA)
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", resp.Code, resp.Body.String())
	}
}

func TestMe_RejectsAppCredentialWith403(t *testing.T) {
	s, _, api := newSignInAPI(t, "", "someone", 400)
	appToken := registerTestApp(t, s, "an-app")

	// An app authenticates fine and is not a person. 401 would be wrong —
	// their credential works — so this is a distinct 403.
	resp := api.Get("/api/v1/me", "User-Agent: "+testUA, "Authorization: Bearer "+appToken)
	if resp.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", resp.Code, resp.Body.String())
	}
}

func TestSession_DeletedUserIsImmediatelyRevoked(t *testing.T) {
	_, pool, api := newSignInAPI(t, "", "someone", 500)
	token, userID, _ := signIn(t, api)

	if resp := api.Get("/api/v1/me", "User-Agent: "+testUA, "Authorization: Bearer "+token); resp.Code != http.StatusOK {
		t.Fatalf("pre-delete /me = %d", resp.Code)
	}

	// The session must die with the account rather than outliving it until
	// the token expires. This is the whole reason the middleware loads the
	// user per request instead of trusting the token's contents.
	if _, err := pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	resp := api.Get("/api/v1/me", "User-Agent: "+testUA, "Authorization: Bearer "+token)
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("post-delete /me = %d, want 401: %s", resp.Code, resp.Body.String())
	}
}

func TestAuth_UserAndAppFailuresAreIndistinguishable(t *testing.T) {
	_, _, api := newSignInAPI(t, "", "someone", 600)

	// A bad app credential and a bad session token must produce the same 401,
	// or a caller can learn which kind of secret it just guessed at.
	bodies := map[string]string{}
	for name, tok := range map[string]string{
		"bad app token":  "oedb_nope.nope",
		"bad user token": "aaa.bbb.ccc",
		"garbage":        "not-a-token-at-all",
	} {
		resp := api.Get("/api/v1/me", "User-Agent: "+testUA, "Authorization: Bearer "+tok)
		if resp.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401: %s", name, resp.Code, resp.Body.String())
		}
		bodies[name] = resp.Body.String()
	}
	var first string
	for name, b := range bodies {
		if first == "" {
			first = b
			continue
		}
		if b != first {
			t.Errorf("%s produced a distinguishable body:\n got: %s\nwant: %s", name, b, first)
		}
	}
}
