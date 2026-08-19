package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2/humatest"

	"github.com/Cawlumm/open-exercise-db/internal/auth"
	"github.com/Cawlumm/open-exercise-db/internal/export"
	"github.com/Cawlumm/open-exercise-db/internal/media"
	"github.com/Cawlumm/open-exercise-db/internal/ratelimit"
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// readPath is a TierRead operation that needs no seeded data.
const readPath = "/api/v1/meta/muscle-groups"

// identifyingUA satisfies the User-Agent policy, so an anonymous caller here
// lands in TierRead rather than the Unidentified tier. Without it these tests
// would prove only that unidentified callers are throttled, which was already
// true.
const identifyingUA = "oedb-test/1.0 (tests@example.test)"

// newTierTestAPI wires the real middleware stack with deliberately lopsided
// limits: an anonymous caller gets three requests, a registered app gets fifty.
// Every assertion below is "which of those two budgets did this request spend",
// which only reads cleanly when the two are far apart.
func newTierTestAPI(t *testing.T) (*store.Store, humatest.TestAPI) {
	t.Helper()
	s, _ := newTestStore(t)

	limiter := ratelimit.NewLimiter(ratelimit.Config{
		Flood:        ratelimit.Rule{RPM: 6000, Burst: 6000},
		App:          ratelimit.Rule{RPM: 50, Burst: 50},
		Read:         ratelimit.Rule{RPM: 3, Burst: 3},
		Search:       ratelimit.Rule{RPM: 3, Burst: 3},
		Export:       ratelimit.Rule{RPM: 3, Burst: 3},
		Unidentified: ratelimit.Rule{RPM: 3, Burst: 3},
	})
	t.Cleanup(limiter.Close)

	cache := export.NewCache(export.DefaultCacheTTL, "")
	mediaStore, err := media.NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatalf("init media store: %v", err)
	}

	_, hapi := humatest.New(t)
	Register(hapi, s, cache, limiter, mediaStore, "", AuthConfig{})
	return s, hapi
}

// registerApp creates an app and returns its usable credential.
func registerApp(t *testing.T, s *store.Store, name string) (token, keyID string) {
	t.Helper()
	keyID, token, hash, err := auth.GenerateAppCredential()
	if err != nil {
		t.Fatalf("generate credential: %v", err)
	}
	if _, err := s.Apps.CreateApp(context.Background(), store.CreateAppInput{
		Name: name, ContactEmail: name + "@example.test", KeyID: keyID, KeyHash: hash,
	}); err != nil {
		t.Fatalf("create app %s: %v", name, err)
	}
	return token, keyID
}

// getN issues n requests and returns how many were allowed.
func getN(t *testing.T, api humatest.TestAPI, n int, credential string) int {
	t.Helper()
	allowed := 0
	for range n {
		headers := []any{"User-Agent: " + identifyingUA}
		if credential != "" {
			headers = append(headers, "Authorization: Bearer "+credential)
		}
		resp := api.Get(readPath, headers...)
		switch resp.Code {
		case http.StatusOK:
			allowed++
		case http.StatusTooManyRequests:
		default:
			t.Fatalf("unexpected status %d: %s", resp.Code, resp.Body)
		}
	}
	return allowed
}

// Registering an app has to buy something. Before this tier existed a named,
// revocable consumer got exactly the same budget as an anonymous caller with a
// polite User-Agent.
func TestRateLimit_AuthenticatedAppGetsItsOwnTier(t *testing.T) {
	s, api := newTierTestAPI(t)
	token, _ := registerApp(t, s, "lyftr")

	if allowed := getN(t, api, 20, token); allowed != 20 {
		t.Errorf("authenticated app allowed %d of 20; it should be on the app tier, not the anonymous one", allowed)
	}
}

func TestRateLimit_AnonymousKeepsTheAnonymousTier(t *testing.T) {
	_, api := newTierTestAPI(t)

	if allowed := getN(t, api, 20, ""); allowed != 3 {
		t.Errorf("anonymous caller allowed %d of 20, want 3", allowed)
	}
}

// The security property this whole design rests on: the tier is granted from a
// credential the database verified, never from one the caller merely presented.
// Reading the key id out of an unverified credential would let anyone mint a
// fresh bucket at the app tier just by claiming an id — the same class of hole
// as trusting a client-supplied CF-Connecting-IP header.
func TestRateLimit_ForgedCredentialCannotBuyTheAppTier(t *testing.T) {
	s, api := newTierTestAPI(t)
	_, realKeyID := registerApp(t, s, "victim")

	forged := []struct{ name, credential string }{
		// The real app's key id with a secret that is not its own — the
		// strongest forgery available, since the id half is genuine.
		{"real key id, wrong secret", fmt.Sprintf("oedb_%s.%s", realKeyID, "not-the-secret")},
		{"invented key id", "oedb_ZZZZZZZZZZZZZZZZZZZZZZ.also-not-a-secret"},
		{"not a credential at all", "totally-made-up"},
		{"empty after the scheme", " "},
	}

	for _, tc := range forged {
		t.Run(tc.name, func(t *testing.T) {
			_, api := newTierTestAPI(t) // a fresh bucket per case
			if allowed := getN(t, api, 20, tc.credential); allowed != 3 {
				t.Errorf("forged credential (%s) allowed %d of 20, want 3 — it bought a larger budget than an anonymous caller", tc.name, allowed)
			}
		})
	}
	_ = api
}

// Apps must not share a bucket. Billing by app id rather than by address is
// what fixes the shared-IP problem for registered consumers, and it only works
// if one app spending its budget leaves another's alone.
func TestRateLimit_AppsDoNotShareABucket(t *testing.T) {
	s, api := newTierTestAPI(t)
	first, _ := registerApp(t, s, "first")
	second, _ := registerApp(t, s, "second")

	if allowed := getN(t, api, 50, first); allowed != 50 {
		t.Fatalf("first app allowed %d of 50", allowed)
	}
	// The first app has now spent its entire budget. The second must be
	// untouched — they arrive from the same address in this harness, which is
	// exactly the shared-address case.
	if allowed := getN(t, api, 10, second); allowed != 10 {
		t.Errorf("second app allowed %d of 10; the two apps are sharing a bucket", allowed)
	}
}

// An app's larger budget must not extend to the export, which is the heaviest
// request in the API and is meant to be mirrored on a schedule. Registering an
// app is not a reason to let it be polled.
func TestRateLimit_AppDoesNotGetABiggerExportBudget(t *testing.T) {
	s, api := newTierTestAPI(t)
	token, _ := registerApp(t, s, "exporter")

	allowed := 0
	for range 10 {
		resp := api.Get("/api/v1/exercises/export",
			"User-Agent: "+identifyingUA,
			"Authorization: Bearer "+token,
		)
		switch resp.Code {
		case http.StatusOK:
			allowed++
		case http.StatusTooManyRequests:
		default:
			t.Fatalf("unexpected status %d: %s", resp.Code, resp.Body)
		}
	}
	if allowed != 3 {
		t.Errorf("app allowed %d of 10 exports, want 3 — the export tier must still apply to apps", allowed)
	}
}
