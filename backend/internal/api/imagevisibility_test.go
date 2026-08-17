package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Cawlumm/open-exercise-db/internal/auth"
	"github.com/Cawlumm/open-exercise-db/internal/export"
	"github.com/Cawlumm/open-exercise-db/internal/media"
	"github.com/Cawlumm/open-exercise-db/internal/ratelimit"
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// Unapproved images are the whole reason this gate exists. Once POST /images
// lands, a pending row is arbitrary bytes a stranger uploaded — if those are
// fetchable the moment they arrive, the moderation queue is decoration and the
// first anyone learns of a problem is when somebody else finds it.
//
// This file carries its own harness because it needs the media store: the
// approved and moderator cases must return real bytes, so asserting 200 proves
// the gate opened rather than that something else happened to fail.

func newImageGateAPI(t *testing.T) (*store.Store, *pgxpool.Pool, media.Store, humatest.TestAPI, string) {
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
	Register(hapi, s, cache, limiter, mediaStore, "", AuthConfig{JWTSecret: signInSecret})

	// A moderator, minted directly: this file is about image visibility, not
	// about the sign-in flow, which has its own tests.
	u, err := s.Users.UpsertGitHubUser(context.Background(), store.UpsertGitHubUserInput{
		GitHubID: "image-gate-admin", Username: "moderator", PromoteToAdmin: true,
	})
	if err != nil {
		t.Fatalf("create moderator: %v", err)
	}
	tok, err := auth.MintUserToken(signInSecret, u.ID, time.Now())
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return s, pool, mediaStore, hapi.(humatest.TestAPI), tok
}

// seedImageWithStatus writes an image row plus its bytes and returns the id.
func seedImageWithStatus(t *testing.T, s *store.Store, mediaStore media.Store, pool *pgxpool.Pool, key, status string) string {
	t.Helper()
	ctx := context.Background()

	id := seedImage(t, ctx, s, mediaStore, key, []byte("pretend-jpeg-bytes"))
	if _, err := pool.Exec(ctx, `UPDATE exercise_images SET status = $2 WHERE id = $1`, id, status); err != nil {
		t.Fatalf("set status %s: %v", status, err)
	}
	return id
}

func TestImageVisibility_PendingIsNotPubliclyServed(t *testing.T) {
	s, pool, ms, api, _ := newImageGateAPI(t)
	id := seedImageWithStatus(t, s, ms, pool, "exercises/Pending_Test/0.jpg", "pending")

	// Anonymous — the case that matters.
	if resp := api.Get("/api/v1/images/"+id, "User-Agent: "+testUA); resp.Code != http.StatusNotFound {
		t.Fatalf("anonymous fetch of a pending image = %d, want 404: %s", resp.Code, resp.Body.String())
	}

	// An app credential authenticates but is not a moderator. An app that
	// could read raw queue uploads could mirror unmoderated content back out.
	appToken := registerTestApp(t, s, "peeking-app")
	if resp := api.Get("/api/v1/images/"+id, "User-Agent: "+testUA, "Authorization: Bearer "+appToken); resp.Code != http.StatusNotFound {
		t.Errorf("app fetch of a pending image = %d, want 404", resp.Code)
	}
}

func TestImageVisibility_ModeratorCanSeePending(t *testing.T) {
	s, pool, ms, api, modToken := newImageGateAPI(t)
	id := seedImageWithStatus(t, s, ms, pool, "exercises/Pending_Mod/0.jpg", "pending")

	// Judging an image requires looking at it, so the same endpoint serves it
	// to someone who can moderate. A separate admin route would be a second
	// place to get the check wrong, and the second one is always the one that
	// gets forgotten.
	resp := api.Get("/api/v1/images/"+id, "User-Agent: "+testUA, "Authorization: Bearer "+modToken)
	if resp.Code != http.StatusOK {
		t.Fatalf("moderator fetch of a pending image = %d, want 200: %s", resp.Code, resp.Body.String())
	}
}

func TestImageVisibility_RejectedStaysHidden(t *testing.T) {
	s, pool, ms, api, _ := newImageGateAPI(t)
	id := seedImageWithStatus(t, s, ms, pool, "exercises/Rejected_Test/0.jpg", "rejected")

	// Rejected is content someone decided not to publish. It must not become
	// reachable merely by ceasing to be pending.
	if resp := api.Get("/api/v1/images/"+id, "User-Agent: "+testUA); resp.Code != http.StatusNotFound {
		t.Errorf("rejected image = %d, want 404", resp.Code)
	}
}

func TestImageVisibility_ApprovedStillPublic(t *testing.T) {
	s, pool, ms, api, _ := newImageGateAPI(t)
	id := seedImageWithStatus(t, s, ms, pool, "exercises/Approved_Test/0.jpg", "approved")

	// The guard against over-tightening: 1,746 seeded images are approved and
	// the public read API must keep serving them anonymously. Every test above
	// is worthless if this one fails.
	resp := api.Get("/api/v1/images/"+id, "User-Agent: "+testUA)
	if resp.Code != http.StatusOK {
		t.Fatalf("approved image = %d, want 200: %s", resp.Code, resp.Body.String())
	}
}

func TestImageVisibility_VariantFollowsItsParent(t *testing.T) {
	s, pool, ms, api, modToken := newImageGateAPI(t)
	ctx := context.Background()

	id := seedImageWithStatus(t, s, ms, pool, "exercises/Variant_Gate/0.jpg", "pending")
	if _, err := pool.Exec(ctx, `
		INSERT INTO exercise_image_variants (image_id, name, storage_key, content_type, width, height)
		VALUES ($1, 'full', 'exercises/Variant_Gate/0.jpg', 'image/jpeg', 800, 600)`, id); err != nil {
		t.Fatalf("seed variant: %v", err)
	}

	// The gap this closes: an unapproved upload unreachable at /images/{id}
	// and served happily at /images/{id}/full. Two routes, one rule.
	if resp := api.Get("/api/v1/images/"+id+"/full", "User-Agent: "+testUA); resp.Code != http.StatusNotFound {
		t.Fatalf("variant of a pending image = %d, want 404: %s", resp.Code, resp.Body.String())
	}
	// And it opens for a moderator, same as the parent.
	if resp := api.Get("/api/v1/images/"+id+"/full", "User-Agent: "+testUA, "Authorization: Bearer "+modToken); resp.Code != http.StatusOK {
		t.Errorf("moderator variant fetch = %d, want 200", resp.Code)
	}
}
