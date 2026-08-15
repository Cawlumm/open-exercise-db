package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"

	"github.com/Cawlumm/open-exercise-db/internal/auth"
	"github.com/Cawlumm/open-exercise-db/internal/export"
	"github.com/Cawlumm/open-exercise-db/internal/media"
	"github.com/Cawlumm/open-exercise-db/internal/ratelimit"
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// No write operation exists yet, so these tests register their own guarded
// route. That is the point of the metadata tag: the middleware's behaviour is
// independent of which operation opts in, and testing it against a real
// registered operation exercises the same path a future POST /submissions
// will take.
const testGuardedPath = "/api/v1/test-guarded"

type guardedOutput struct {
	Body struct {
		AppName string `json:"app_name"`
	}
}

// newAuthTestAPI wires Register plus one auth-required operation, returning
// the store so tests can register apps.
func newAuthTestAPI(t *testing.T) (*store.Store, humatest.TestAPI) {
	t.Helper()
	s, _ := newTestStore(t)

	// Generous limits: these tests make several requests and are not about
	// rate limiting.
	limiter := ratelimit.NewLimiter(ratelimit.Config{
		Read:         ratelimit.Rule{RPM: 600, Burst: 200},
		Search:       ratelimit.Rule{RPM: 600, Burst: 200},
		Export:       ratelimit.Rule{RPM: 600, Burst: 200},
		Unidentified: ratelimit.Rule{RPM: 600, Burst: 200},
	})
	t.Cleanup(limiter.Close)

	cache := export.NewCache(export.DefaultCacheTTL, "")
	mediaStore, err := media.NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatalf("init media store: %v", err)
	}

	_, hapi := humatest.New(t)
	Register(hapi, s, cache, limiter, mediaStore, "")

	huma.Register(hapi, huma.Operation{
		OperationID: "test-guarded",
		Method:      http.MethodGet,
		Path:        testGuardedPath,
		Summary:     "Test-only operation requiring an app credential",
		Metadata: map[string]any{
			rateLimitTierKey: ratelimit.TierRead,
			authRequiredKey:  true,
		},
	}, func(ctx context.Context, _ *struct{}) (*guardedOutput, error) {
		out := &guardedOutput{}
		// Reaching the handler at all proves the middleware let it through;
		// echoing the app name proves the identity survived into the handler
		// rather than merely being validated and dropped.
		if app, ok := AppFromContext(ctx); ok {
			out.Body.AppName = app.Name
		}
		return out, nil
	})

	return s, hapi.(humatest.TestAPI)
}

// registerTestApp creates an app and returns its usable token.
func registerTestApp(t *testing.T, s *store.Store, name string) string {
	t.Helper()

	keyID, token, hash, err := auth.GenerateAppCredential()
	if err != nil {
		t.Fatalf("generate credential: %v", err)
	}
	if _, err := s.Apps.CreateApp(context.Background(), store.CreateAppInput{
		Name:         name,
		ContactEmail: name + "@example.test",
		KeyID:        keyID,
		KeyHash:      hash,
	}); err != nil {
		t.Fatalf("create app %s: %v", name, err)
	}
	return token
}

const testUA = "oedb-auth-test/1.0 (tests@example.test)"

func TestAuth_PublicReadsNeedNoCredential(t *testing.T) {
	_, testAPI := newAuthTestAPI(t)

	// The whole read API stays open. A regression here would be the most
	// damaging possible outcome of adding auth.
	resp := testAPI.Get("/api/v1/meta/equipment", "User-Agent: "+testUA)
	if resp.Code != http.StatusOK {
		t.Fatalf("public read status = %d, want 200: %s", resp.Code, resp.Body.String())
	}
}

func TestAuth_GuardedRouteRejectsMissingCredential(t *testing.T) {
	_, testAPI := newAuthTestAPI(t)

	resp := testAPI.Get(testGuardedPath, "User-Agent: "+testUA)
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", resp.Code, resp.Body.String())
	}
}

func TestAuth_ValidCredentialReachesHandlerWithIdentity(t *testing.T) {
	s, testAPI := newAuthTestAPI(t)
	token := registerTestApp(t, s, "lyftr")

	resp := testAPI.Get(testGuardedPath,
		"User-Agent: "+testUA,
		"Authorization: Bearer "+token,
	)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body.String())
	}
	if body := resp.Body.String(); !strings.Contains(body, `"app_name":"lyftr"`) {
		t.Errorf("handler did not see the authenticated app: %s", body)
	}
}

func TestAuth_SchemeIsCaseInsensitive(t *testing.T) {
	s, testAPI := newAuthTestAPI(t)
	token := registerTestApp(t, s, "lowercase-scheme")

	// RFC 9110 §11.1 specifies auth schemes as case-insensitive; a client
	// sending `bearer` is following the spec.
	resp := testAPI.Get(testGuardedPath,
		"User-Agent: "+testUA,
		"Authorization: bearer "+token,
	)
	if resp.Code != http.StatusOK {
		t.Fatalf("lowercase scheme status = %d, want 200: %s", resp.Code, resp.Body.String())
	}
}

func TestAuth_BadCredentialsAllLookAlike(t *testing.T) {
	s, testAPI := newAuthTestAPI(t)
	token := registerTestApp(t, s, "victim")

	// A wrong secret against a REAL key id, an entirely unknown key id, and
	// a malformed token must be indistinguishable. Anything else tells an
	// attacker which half of a guess landed.
	keyID, secret, err := auth.ParseToken(token)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	wrongSecret := auth.FormatToken(keyID, secret[:len(secret)-2]+"zz")
	unknownKey := auth.FormatToken("not-a-real-key-id", secret)

	bodies := map[string]string{}
	for name, tok := range map[string]string{
		"wrong secret": wrongSecret,
		"unknown key":  unknownKey,
		"malformed":    "not-even-close",
		"empty-ish":    "oedb_",
	} {
		resp := testAPI.Get(testGuardedPath,
			"User-Agent: "+testUA,
			"Authorization: Bearer "+tok,
		)
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

func TestAuth_SuspendedAppIsForbiddenNotUnauthorized(t *testing.T) {
	s, testAPI := newAuthTestAPI(t)
	token := registerTestApp(t, s, "suspended")
	suspend(t, s, token)

	// 403, not 401: the credential is real, and reporting it invalid would
	// send the operator hunting for a key problem that does not exist.
	resp := testAPI.Get(testGuardedPath,
		"User-Agent: "+testUA,
		"Authorization: Bearer "+token,
	)
	if resp.Code != http.StatusForbidden {
		t.Fatalf("suspended app status = %d, want 403: %s", resp.Code, resp.Body.String())
	}
}

func TestAuth_UntaggedWriteOperationStillRequiresCredential(t *testing.T) {
	s, _ := newTestStore(t)
	limiter := ratelimit.NewLimiter(ratelimit.Config{
		Read: ratelimit.Rule{RPM: 600, Burst: 200}, Search: ratelimit.Rule{RPM: 600, Burst: 200},
		Export: ratelimit.Rule{RPM: 600, Burst: 200}, Unidentified: ratelimit.Rule{RPM: 600, Burst: 200},
	})
	t.Cleanup(limiter.Close)
	cache := export.NewCache(export.DefaultCacheTTL, "")
	mediaStore, err := media.NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatalf("init media store: %v", err)
	}
	_, hapi := humatest.New(t)
	Register(hapi, s, cache, limiter, mediaStore, "")

	// Deliberately registered WITHOUT authRequiredKey — the mistake a future
	// POST /submissions could make. The default must not be "public", or that
	// mistake ships a world-writable endpoint with no log, no test failure,
	// and nothing in review to catch it. The method is what makes it safe,
	// because nobody can forget to set the method.
	huma.Register(hapi, huma.Operation{
		OperationID: "test-untagged-write",
		Method:      http.MethodPost,
		Path:        "/api/v1/test-untagged-write",
		Summary:     "Test-only write operation that forgot its auth tag",
		Metadata:    map[string]any{rateLimitTierKey: ratelimit.TierRead},
	}, func(_ context.Context, _ *struct{}) (*guardedOutput, error) {
		return &guardedOutput{}, nil
	})

	resp := hapi.(humatest.TestAPI).Post("/api/v1/test-untagged-write", "User-Agent: "+testUA)
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("untagged POST status = %d, want 401 — an untagged write must fail closed: %s",
			resp.Code, resp.Body.String())
	}
}

func TestAuth_CredentialOnPublicRouteStillAttributes(t *testing.T) {
	s, testAPI := newAuthTestAPI(t)
	token := registerTestApp(t, s, "reader")

	// Authentication is opportunistic on public routes: a valid credential
	// is resolved even where none is required, so a future per-app read
	// budget has an identity to key on. It must not turn a public read into
	// an error.
	resp := testAPI.Get("/api/v1/meta/equipment",
		"User-Agent: "+testUA,
		"Authorization: Bearer "+token,
	)
	if resp.Code != http.StatusOK {
		t.Fatalf("authenticated public read status = %d, want 200: %s", resp.Code, resp.Body.String())
	}
}

func TestAuth_InvalidCredentialDoesNotBreakPublicReads(t *testing.T) {
	_, testAPI := newAuthTestAPI(t)

	// The read API has been open to anonymous traffic since Phase 1, and a
	// client can always reach it by dropping the header. So an unusable
	// Authorization header — an API gateway injecting its own JWT, an app
	// forwarding an end-user session token by mistake — must not take reads
	// away. There is no attribution at stake on a GET; the credential only
	// has to be valid where it is actually consumed.
	for _, header := range []string{
		"Authorization: Bearer oedb_nope.nope",
		"Authorization: Bearer some.other.jwt",
		"Authorization: Bearer " + strings.Repeat("x", 200),
	} {
		resp := testAPI.Get("/api/v1/meta/equipment", "User-Agent: "+testUA, header)
		if resp.Code != http.StatusOK {
			t.Errorf("%s -> status %d, want 200: %s", header, resp.Code, resp.Body.String())
		}
	}

	// The same credential on an operation that requires one is still a 401.
	resp := testAPI.Get(testGuardedPath,
		"User-Agent: "+testUA,
		"Authorization: Bearer oedb_nope.nope",
	)
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("guarded route status = %d, want 401: %s", resp.Code, resp.Body.String())
	}
}

func TestAuth_SuspensionDoesNotBreakPublicReads(t *testing.T) {
	s, testAPI := newAuthTestAPI(t)
	token := registerTestApp(t, s, "suspended-reader")
	suspend(t, s, token)

	// Suspension is a write-side lever (model.App, migration 0003). Enforcing
	// it on reads would turn one moderation decision into a total blackout for
	// every user of that app — while the identical request still succeeds by
	// dropping the header, which makes it useless as a control and harmful as
	// an outage.
	resp := testAPI.Get("/api/v1/meta/equipment",
		"User-Agent: "+testUA,
		"Authorization: Bearer "+token,
	)
	if resp.Code != http.StatusOK {
		t.Fatalf("suspended app read status = %d, want 200: %s", resp.Code, resp.Body.String())
	}
}

func TestAuth_ErrorsCarryProblemTypeURIs(t *testing.T) {
	s, testAPI := newAuthTestAPI(t)

	// `detail` is prose and will be reworded; `type` is the stable thing a
	// client branches on. These two errors demand opposite responses —
	// rotate the credential versus contact the maintainer — so a client that
	// cannot tell them apart either rotates a working key or emails about a
	// typo.
	unauth := testAPI.Get(testGuardedPath, "User-Agent: "+testUA)
	if !strings.Contains(unauth.Body.String(), ErrTypeUnauthorized) {
		t.Errorf("401 body has no %s type URI: %s", ErrTypeUnauthorized, unauth.Body.String())
	}

	token := registerTestApp(t, s, "suspended-type")
	suspend(t, s, token)
	forbidden := testAPI.Get(testGuardedPath, "User-Agent: "+testUA, "Authorization: Bearer "+token)
	if !strings.Contains(forbidden.Body.String(), ErrTypeForbidden) {
		t.Errorf("403 body has no %s type URI: %s", ErrTypeForbidden, forbidden.Body.String())
	}
}

// suspend puts the app owning token into the suspended state.
func suspend(t *testing.T, s *store.Store, token string) {
	t.Helper()

	keyID, _, err := auth.ParseToken(token)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	app, err := s.Apps.GetByKeyID(context.Background(), keyID)
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	if err := s.Apps.SetStatus(context.Background(), app.ID, "suspended"); err != nil {
		t.Fatalf("suspend: %v", err)
	}
}
