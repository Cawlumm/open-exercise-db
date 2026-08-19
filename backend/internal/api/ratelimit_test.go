package api

import (
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2/humatest"

	"github.com/Cawlumm/open-exercise-db/internal/export"
	"github.com/Cawlumm/open-exercise-db/internal/media"
	"github.com/Cawlumm/open-exercise-db/internal/ratelimit"
)

// goodUA is a conformant identifier per docs/API_POLICY.md's format —
// every test below that wants the operation's *normal* tier budget (not
// the strict Unidentified one) sends this.
const goodUA = "User-Agent: oedb-test-client/1.0 (test@example.com)"

// Every test below calls the real Register (unlike every other *_test.go
// in this package, which calls registerExerciseRoutes/registerMetaRoutes/
// registerExportRoutes directly and so never exercises
// rateLimitMiddleware at all) against a Store backed by a real disposable
// database (newTestStore, api_test.go), with a small, test-local
// ratelimit.Config so a burst can be exhausted in one or two requests
// instead of sleeping through a real per-minute window.
// internal/ratelimit/ratelimit_test.go already covers the token-bucket math
// in isolation; these confirm it's wired correctly into the actual HTTP
// path — status code, headers, tier isolation, and the User-Agent policy.

// newRegisteredTestAPI wires Register (middleware included) against a fresh
// disposable database and the given ratelimit.Config, returning the store
// (for seeding fixtures) and a ready-to-hit TestAPI.
func newRegisteredTestAPI(t *testing.T, cfg ratelimit.Config) humatest.TestAPI {
	t.Helper()
	s, _ := newTestStore(t)

	limiter := ratelimit.NewLimiter(cfg)
	t.Cleanup(limiter.Close)

	cache := export.NewCache(export.DefaultCacheTTL, "")
	mediaStore, err := media.NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatalf("init media store: %v", err)
	}
	_, hapi := humatest.New(t)
	Register(hapi, s, cache, limiter, mediaStore, "", AuthConfig{})

	return hapi.(humatest.TestAPI)
}

func TestRateLimit_ExceedingBurstReturns429WithHeaders(t *testing.T) {
	testAPI := newRegisteredTestAPI(t, ratelimit.Config{
		Search: ratelimit.Rule{RPM: 60, Burst: 2},
	})

	// list-exercises is TierSearch. First two calls (burst=2) succeed...
	for i := 0; i < 2; i++ {
		resp := testAPI.Get("/api/v1/exercises", goodUA)
		if resp.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200; body: %s", i+1, resp.Code, resp.Body.String())
		}
		if got := resp.Header().Get("RateLimit-Limit"); got != "60" {
			t.Errorf("request %d: RateLimit-Limit = %q, want %q", i+1, got, "60")
		}
	}

	// ...the third, within the same instant, must be denied.
	denied := testAPI.Get("/api/v1/exercises", goodUA)
	if denied.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd request status = %d, want %d; body: %s", denied.Code, http.StatusTooManyRequests, denied.Body.String())
	}
	if got := denied.Header().Get("RateLimit-Limit"); got != "60" {
		t.Errorf("RateLimit-Limit = %q, want %q", got, "60")
	}
	if got := denied.Header().Get("RateLimit-Remaining"); got != "0" {
		t.Errorf("RateLimit-Remaining = %q, want %q", got, "0")
	}
	reset := denied.Header().Get("RateLimit-Reset")
	if reset == "" || reset == "0" {
		t.Errorf("RateLimit-Reset = %q, want a positive number of seconds", reset)
	}
	retryAfter := denied.Header().Get("Retry-After")
	if retryAfter == "" || retryAfter == "0" {
		t.Errorf("Retry-After = %q, want a positive number of seconds", retryAfter)
	}
	if reset != retryAfter {
		t.Errorf("RateLimit-Reset (%q) and Retry-After (%q) differ on a denied response, want equal (see ratelimit.Decision's doc comment)", reset, retryAfter)
	}
}

func TestRateLimit_UnderLimitTrafficUnaffected(t *testing.T) {
	// Generous budget, well above what this test's request count could
	// ever exhaust — proves the middleware doesn't interfere with ordinary
	// traffic when nobody is near the configured limit.
	testAPI := newRegisteredTestAPI(t, ratelimit.Config{
		Search: ratelimit.Rule{RPM: 6000, Burst: 50},
	})

	for i := 0; i < 10; i++ {
		resp := testAPI.Get("/api/v1/exercises", goodUA)
		if resp.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200; body: %s", i+1, resp.Code, resp.Body.String())
		}
	}
}

func TestRateLimit_TiersAreIsolatedOverHTTP(t *testing.T) {
	// docs/PHASE1_BUILD_PLAN.md WO-7 acceptance criterion, exercised over
	// the real HTTP path this time (internal/ratelimit/ratelimit_test.go
	// already covers the same guarantee at the Limiter level directly):
	// hammering /export must not exhaust the /meta (read) budget.
	testAPI := newRegisteredTestAPI(t, ratelimit.Config{
		Export: ratelimit.Rule{RPM: 60, Burst: 1},
		Read:   ratelimit.Rule{RPM: 60, Burst: 5},
	})

	first := testAPI.Get("/api/v1/exercises/export", goodUA)
	if first.Code != http.StatusOK {
		t.Fatalf("first export request status = %d, want 200; body: %s", first.Code, first.Body.String())
	}
	exhausted := testAPI.Get("/api/v1/exercises/export", goodUA)
	if exhausted.Code != http.StatusTooManyRequests {
		t.Fatalf("second export request status = %d, want %d (export burst is 1)", exhausted.Code, http.StatusTooManyRequests)
	}

	// /meta/muscle-groups is TierRead — a separate, still-full budget.
	meta := testAPI.Get("/api/v1/meta/muscle-groups", goodUA)
	if meta.Code != http.StatusOK {
		t.Fatalf("meta request status = %d, want 200 — export exhaustion must not leak into the read tier; body: %s", meta.Code, meta.Body.String())
	}
}

func TestRateLimit_UserAgentPolicy(t *testing.T) {
	t.Run("missing User-Agent gets the strict Unidentified budget, not TierRead's", func(t *testing.T) {
		testAPI := newRegisteredTestAPI(t, ratelimit.Config{
			Read:         ratelimit.Rule{RPM: 60, Burst: 5},
			Unidentified: ratelimit.Rule{RPM: 60, Burst: 1},
		})

		// list-muscle-groups is TierRead, but that only applies to an
		// identified caller — a request with no User-Agent at all should
		// draw from Unidentified instead, regardless of which tier the
		// endpoint is normally priced at.
		path := "/api/v1/meta/muscle-groups"

		first := testAPI.Get(path) // no User-Agent header at all
		if first.Code != http.StatusOK {
			t.Fatalf("first no-UA request status = %d, want 200; body: %s", first.Code, first.Body.String())
		}

		second := testAPI.Get(path)
		if second.Code != http.StatusTooManyRequests {
			t.Fatalf("second no-UA request status = %d, want %d (Unidentified burst is 1)", second.Code, http.StatusTooManyRequests)
		}
		if got := second.Header().Get("RateLimit-Limit"); got != "60" {
			t.Errorf("RateLimit-Limit = %q, want the Unidentified tier's 60", got)
		}

		// A *properly identified* request for the same route must still
		// have TierRead's own, much larger, untouched budget — proves the
		// no-UA traffic above never touched it.
		identified := testAPI.Get(path, goodUA)
		if identified.Code != http.StatusOK {
			t.Fatalf("identified request status = %d, want 200 (TierRead budget must be untouched by unidentified traffic); body: %s", identified.Code, identified.Body.String())
		}
	})

	t.Run("generic library User-Agent is treated as unidentified", func(t *testing.T) {
		testAPI := newRegisteredTestAPI(t, ratelimit.Config{
			Read:         ratelimit.Rule{RPM: 60, Burst: 5},
			Unidentified: ratelimit.Rule{RPM: 60, Burst: 1},
		})
		path := "/api/v1/meta/equipment"

		first := testAPI.Get(path, "User-Agent: curl/8.4.0")
		if first.Code != http.StatusOK {
			t.Fatalf("first curl-UA request status = %d, want 200; body: %s", first.Code, first.Body.String())
		}
		second := testAPI.Get(path, "User-Agent: curl/8.4.0")
		if second.Code != http.StatusTooManyRequests {
			t.Fatalf("second curl-UA request status = %d, want %d — a generic library UA must consume the Unidentified budget, not TierRead's", second.Code, http.StatusTooManyRequests)
		}
	})

	t.Run("conformant User-Agent gets the endpoint's normal tier", func(t *testing.T) {
		// Unidentified burst is 1; TierRead burst is 3 — an identified
		// caller must not be quietly capped at Unidentified's smaller
		// number.
		testAPI := newRegisteredTestAPI(t, ratelimit.Config{
			Read:         ratelimit.Rule{RPM: 60, Burst: 3},
			Unidentified: ratelimit.Rule{RPM: 60, Burst: 1},
		})
		path := "/api/v1/meta/categories"

		for i := 0; i < 3; i++ {
			resp := testAPI.Get(path, goodUA)
			if resp.Code != http.StatusOK {
				t.Fatalf("identified request %d: status = %d, want 200 (TierRead burst is 3); body: %s", i+1, resp.Code, resp.Body.String())
			}
		}
	})
}

// TestRateLimit_429IsDocumentedInOpenAPISpec asserts every rate-limited GET
// operation this package registers declares a 429 response in its OpenAPI
// operation (huma.Operation.Errors, set alongside each route's Metadata tier
// tag in exercises.go/meta.go/export.go) — docs/PHASE1_BUILD_PLAN.md WO-7's
// "return a typed huma error so it lands in the OpenAPI spec" requirement.
// Inspects hapi.OpenAPI() directly rather than round-tripping through
// GET /openapi.json: humatest.New's zero-value huma.Config leaves
// OpenAPIPath empty, which means no spec route is registered at all (see
// huma's api.go — a route is only added `if config.OpenAPIPath != ""`), so
// an HTTP round trip here would 404 regardless of whether 429 is
// documented; the in-memory spec object is what actually gets served (with
// a real OpenAPIPath) by cmd/api-server/main.go in production.
func TestRateLimit_429IsDocumentedInOpenAPISpec(t *testing.T) {
	testAPI := newRegisteredTestAPI(t, ratelimit.DefaultConfig())
	spec := testAPI.OpenAPI()

	paths := []string{
		"/api/v1/exercises",
		"/api/v1/exercises/{id}",
		"/api/v1/exercises/{id}/revisions",
		"/api/v1/exercises/{id}/translations",
		"/api/v1/exercises/export",
		"/api/v1/meta/muscle-groups",
		"/api/v1/meta/equipment",
		"/api/v1/meta/categories",
		"/api/v1/meta/locales",
	}
	for _, p := range paths {
		item, ok := spec.Paths[p]
		if !ok || item.Get == nil {
			t.Errorf("%s: no GET operation found in the generated spec", p)
			continue
		}
		if _, ok := item.Get.Responses["429"]; !ok {
			t.Errorf("%s: GET operation has no 429 response documented", p)
		}
	}
}

// A browser is the one consumer that cannot comply with the User-Agent policy:
// the Fetch spec makes User-Agent a forbidden header name, so script cannot set
// it and the browser's own "Mozilla/..." string is sent instead — which
// genericUAPrefixes correctly classifies as unidentified. Before X-OEDB-Client
// existed, a web app was pinned to the Unidentified tier permanently with no
// action available to its author, which made this API unusable from a page.
func TestRateLimit_BrowserCanIdentifyWithoutUserAgent(t *testing.T) {
	const browserUA = "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"
	const clientID = "X-OEDB-Client: lyftr-web/1.0 (contact@example.com)"

	t.Run("browser UA alone is still unidentified", func(t *testing.T) {
		testAPI := newRegisteredTestAPI(t, ratelimit.Config{
			Read:         ratelimit.Rule{RPM: 60, Burst: 5},
			Unidentified: ratelimit.Rule{RPM: 60, Burst: 1},
		})
		path := "/api/v1/meta/categories"

		if first := testAPI.Get(path, browserUA); first.Code != http.StatusOK {
			t.Fatalf("first browser request = %d, want 200", first.Code)
		}
		if second := testAPI.Get(path, browserUA); second.Code != http.StatusTooManyRequests {
			t.Fatalf("second browser request = %d, want 429 — a bare browser UA must not buy the read tier", second.Code)
		}
	})

	t.Run("X-OEDB-Client earns the endpoint's normal tier despite the browser UA", func(t *testing.T) {
		testAPI := newRegisteredTestAPI(t, ratelimit.Config{
			Read:         ratelimit.Rule{RPM: 60, Burst: 5},
			Unidentified: ratelimit.Rule{RPM: 60, Burst: 1},
		})
		path := "/api/v1/meta/categories"

		// Both headers are sent together, which is the only shape a real
		// browser can produce: it cannot suppress its own User-Agent, so a
		// rule requiring UA to be absent would make the header useless.
		for i := range 4 {
			resp := testAPI.Get(path, browserUA, clientID)
			if resp.Code != http.StatusOK {
				t.Fatalf("request %d with X-OEDB-Client = %d, want 200 (read burst is 5); body: %s", i+1, resp.Code, resp.Body.String())
			}
		}
	})

	t.Run("a junk X-OEDB-Client does not buy the normal tier", func(t *testing.T) {
		testAPI := newRegisteredTestAPI(t, ratelimit.Config{
			Read:         ratelimit.Rule{RPM: 60, Burst: 5},
			Unidentified: ratelimit.Rule{RPM: 60, Burst: 1},
		})
		path := "/api/v1/meta/equipment"

		// The header is an identity claim in a defined format, not a magic
		// opt-out — anything that would fail the policy as a User-Agent must
		// fail here too, or the policy is decorative.
		const junk = "X-OEDB-Client: Mozilla/5.0"
		if first := testAPI.Get(path, browserUA, junk); first.Code != http.StatusOK {
			t.Fatalf("first junk-header request = %d, want 200", first.Code)
		}
		if second := testAPI.Get(path, browserUA, junk); second.Code != http.StatusTooManyRequests {
			t.Errorf("second junk-header request = %d, want 429 — malformed identification must stay unidentified", second.Code)
		}
	})
}
