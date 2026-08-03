package api

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Cawlumm/open-exercise-db/internal/media"
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// newTestMediaStore returns a media.Store rooted at a fresh temp directory —
// this package's equivalent of newTestStore (api_test.go) for the object
// storage side of an image test, per docs/PHASE1_BUILD_PLAN.md WO-3's "do
// not mock the store" spirit: a real FilesystemStore round-trips real bytes
// through a real filesystem, the same as production's Store.
func newTestMediaStore(t *testing.T) media.Store {
	t.Helper()
	ms, err := media.NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatalf("init media store: %v", err)
	}
	return ms
}

// seedImage inserts an exercise_images row (so content_type/width/height
// round-trip through Postgres like production data) and writes fakeBytes to
// mediaStore at storageKey, returning the new image's id.
func seedImage(t *testing.T, ctx context.Context, s *store.Store, mediaStore media.Store, storageKey string, fakeBytes []byte) string {
	t.Helper()

	width, height := 800, 600
	license := "Unlicense"
	attribution := "free-exercise-db contributors"
	id, err := s.Media.InsertImage(ctx, store.InsertImageInput{
		StorageKey:      storageKey,
		ContentType:     "image/jpeg",
		Width:           &width,
		Height:          &height,
		License:         &license,
		AttributionText: &attribution,
		Status:          "approved",
	})
	if err != nil {
		t.Fatalf("seed image: %v", err)
	}
	if err := mediaStore.Put(ctx, storageKey, bytes.NewReader(fakeBytes), "image/jpeg"); err != nil {
		t.Fatalf("write image bytes: %v", err)
	}
	return id
}

// attachImage points revisionID's image_ids[] at imageID, mirroring what
// the importer does after re-hosting an image (internal/seed/freeexercisedb.go
// attaches image ids as part of the revision content it writes). Done here
// via a raw SQL update rather than store.ExerciseStore.UpsertImport, since
// UpsertImport always writes an entire revision's content atomically and has
// no standalone "attach an image to an already-seeded revision" entry point
// — exactly the kind of test-only shortcut api_test.go's seedExercise/
// seedTranslation already take for the same reason.
func attachImage(t *testing.T, ctx context.Context, pool *pgxpool.Pool, revisionID, imageID string) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`UPDATE exercise_revisions SET image_ids = $1 WHERE id = $2`,
		[]string{imageID}, revisionID,
	); err != nil {
		t.Fatalf("attach image %s to revision %s: %v", imageID, revisionID, err)
	}
}

// TestGetImage_ReturnsBytesWithContentType is the core defect regression:
// before this endpoint existed there was no route that served an image at
// all, so a consumer holding an image_ids UUID had no way to display it.
// Asserts on the actual bytes and Content-Type, not just a 200 — a 200 with
// an empty or wrong-typed body would have been just as broken.
func TestGetImage_ReturnsBytesWithContentType(t *testing.T) {
	s, _ := newTestStore(t)
	mediaStore := newTestMediaStore(t)
	ctx := context.Background()

	want := []byte("\xff\xd8\xff\xe0fake-but-distinctive-jpeg-bytes")
	id := seedImage(t, ctx, s, mediaStore, "exercises/bench-press/0.jpg", want)

	out, err := GetImage(ctx, s, mediaStore, &GetImageInput{ID: id})
	if err != nil {
		t.Fatalf("GetImage: %v", err)
	}
	if !bytes.Equal(out.Body, want) {
		t.Errorf("Body = %q, want %q", out.Body, want)
	}
	if out.ContentType != "image/jpeg" {
		t.Errorf("ContentType = %q, want image/jpeg (the stored value, not guessed from an extension)", out.ContentType)
	}
	if out.CacheControl != imageCacheControl {
		t.Errorf("CacheControl = %q, want %q", out.CacheControl, imageCacheControl)
	}
	if out.ETag == "" {
		t.Error("ETag header not set")
	}
}

func TestGetImage_NotFound(t *testing.T) {
	s, _ := newTestStore(t)
	mediaStore := newTestMediaStore(t)
	ctx := context.Background()

	_, err := GetImage(ctx, s, mediaStore, &GetImageInput{ID: "00000000-0000-0000-0000-000000000000"})
	if err == nil {
		t.Fatal("GetImage(nonexistent): expected error, got nil")
	}
	statusErr, ok := err.(huma.StatusError)
	if !ok || statusErr.GetStatus() != http.StatusNotFound {
		t.Fatalf("GetImage(nonexistent) error = %v, want a 404 huma.StatusError", err)
	}
}

// TestGetImage_MalformedID_Returns422 needs a real HTTP round trip — the
// `format:"uuid"` validation on GetImageInput.ID (the same tag
// GetExerciseInput.ID uses, see api.go's comment on registration order for
// why it's trusted to 422 a non-UUID path segment) only runs in huma's
// request pipeline, not when calling GetImage directly.
func TestGetImage_MalformedID_Returns422(t *testing.T) {
	s, _ := newTestStore(t)
	mediaStore := newTestMediaStore(t)

	_, testAPI := humatest.New(t)
	registerImageRoutes(testAPI, s, mediaStore)

	resp := testAPI.Get("/api/v1/images/not-a-uuid")
	if resp.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d; body: %s", resp.Code, http.StatusUnprocessableEntity, resp.Body.String())
	}
}

// TestGetImage_ConditionalRequestReturns304 confirms GetImage routes
// through the same checkNotModified helper (and therefore the same
// trimListOWS workaround) every other handler in this package does, even
// though its ETag is computed from raw bytes (etagValueBytes) rather than a
// JSON body (etagValue).
func TestGetImage_ConditionalRequestReturns304(t *testing.T) {
	s, _ := newTestStore(t)
	mediaStore := newTestMediaStore(t)
	ctx := context.Background()

	id := seedImage(t, ctx, s, mediaStore, "exercises/squat/0.jpg", []byte("squat-image-bytes"))

	_, testAPI := humatest.New(t)
	registerImageRoutes(testAPI, s, mediaStore)

	first := testAPI.Get("/api/v1/images/" + id)
	if first.Code != http.StatusOK {
		t.Fatalf("initial request status = %d, want 200; body: %s", first.Code, first.Body.String())
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("initial 200 response had no ETag header")
	}

	resp := testAPI.Get("/api/v1/images/"+id, "If-None-Match: "+etag)
	if resp.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want %d; body: %s", resp.Code, http.StatusNotModified, resp.Body.String())
	}
	if resp.Body.Len() != 0 {
		t.Errorf("304 response had a %d-byte body, want none: %s", resp.Body.Len(), resp.Body.String())
	}
	if got := resp.Header().Get("Cache-Control"); got != imageCacheControl {
		t.Errorf("304 Cache-Control = %q, want %q", got, imageCacheControl)
	}
}

// TestGetExercise_ImagesURL_LocalModeFallsBackToImagesRoute and
// TestGetExercise_ImagesURL_BaseURLModePointsAtExternalHost cover the two
// modes internal/media.ImageURL supports — the whole point of making
// ImageBaseURL a config value rather than a hardcoded route: production can
// point at an external CDN/object-store host without a code change, while
// local dev/self-host (ImageBaseURL unset, the default) still gets a
// directly usable URL via this API's own route.
func TestGetExercise_ImagesURL_LocalModeFallsBackToImagesRoute(t *testing.T) {
	s, pool := newTestStore(t)
	mediaStore := newTestMediaStore(t)
	ctx := context.Background()

	exerciseID, revisionID := seedExercise(t, ctx, pool, exerciseFixture{
		slug: "bench-press", name: "Bench Press",
		muscleGroup: "chest", equipment: "barbell", category: "strength",
	})
	storageKey := "exercises/bench-press/0.jpg"
	imageID := seedImage(t, ctx, s, mediaStore, storageKey, []byte("bytes"))
	attachImage(t, ctx, pool, revisionID, imageID)

	out, err := GetExercise(ctx, s, "", &GetExerciseInput{ID: exerciseID})
	if err != nil {
		t.Fatalf("GetExercise: %v", err)
	}
	if len(out.Body.Images) != 1 {
		t.Fatalf("len(Images) = %d, want 1", len(out.Body.Images))
	}
	img := out.Body.Images[0]
	if img.ID != imageID {
		t.Errorf("Images[0].ID = %q, want %q", img.ID, imageID)
	}
	wantURL := "/api/v1/images/" + imageID
	if img.URL != wantURL {
		t.Errorf("Images[0].URL = %q, want %q (local route, ImageBaseURL unset)", img.URL, wantURL)
	}
	if img.ContentType != "image/jpeg" {
		t.Errorf("Images[0].ContentType = %q, want image/jpeg", img.ContentType)
	}
}

func TestGetExercise_ImagesURL_BaseURLModePointsAtExternalHost(t *testing.T) {
	s, pool := newTestStore(t)
	mediaStore := newTestMediaStore(t)
	ctx := context.Background()

	exerciseID, revisionID := seedExercise(t, ctx, pool, exerciseFixture{
		slug: "squat", name: "Squat",
		muscleGroup: "legs", equipment: "barbell", category: "strength",
	})
	storageKey := "exercises/squat/0.jpg"
	imageID := seedImage(t, ctx, s, mediaStore, storageKey, []byte("bytes"))
	attachImage(t, ctx, pool, revisionID, imageID)

	const base = "https://images.example.com"
	out, err := GetExercise(ctx, s, base, &GetExerciseInput{ID: exerciseID})
	if err != nil {
		t.Fatalf("GetExercise: %v", err)
	}
	if len(out.Body.Images) != 1 {
		t.Fatalf("len(Images) = %d, want 1", len(out.Body.Images))
	}
	wantURL := base + "/" + storageKey
	if got := out.Body.Images[0].URL; got != wantURL {
		t.Errorf("Images[0].URL = %q, want %q (points at the external base, not the local route)", got, wantURL)
	}
}

// TestListExercises_ImagesURL confirms the list endpoint resolves the same
// way GetExercise does — both go through resolveExercise, but this is the
// only test in this file that exercises the list path's own plumbing of
// imageBaseURL end to end.
func TestListExercises_ImagesURL(t *testing.T) {
	s, pool := newTestStore(t)
	mediaStore := newTestMediaStore(t)
	ctx := context.Background()

	_, revisionID := seedExercise(t, ctx, pool, exerciseFixture{
		slug: "deadlift", name: "Deadlift",
		muscleGroup: "back", equipment: "barbell", category: "strength",
	})
	storageKey := "exercises/deadlift/0.jpg"
	imageID := seedImage(t, ctx, s, mediaStore, storageKey, []byte("bytes"))
	attachImage(t, ctx, pool, revisionID, imageID)

	const base = "https://images.example.com"
	out, err := ListExercises(ctx, s, base, &ListExercisesInput{})
	if err != nil {
		t.Fatalf("ListExercises: %v", err)
	}
	if len(out.Body.Exercises) != 1 || len(out.Body.Exercises[0].Images) != 1 {
		t.Fatalf("Exercises/Images = %d/%d, want 1/1", len(out.Body.Exercises), len(out.Body.Exercises[0].Images))
	}
	wantURL := base + "/" + storageKey
	if got := out.Body.Exercises[0].Images[0].URL; got != wantURL {
		t.Errorf("Images[0].URL = %q, want %q", got, wantURL)
	}
}
