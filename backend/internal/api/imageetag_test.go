package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/danielgtaylor/huma/v2/humatest"

	"github.com/Cawlumm/open-exercise-db/internal/media"
	"github.com/Cawlumm/open-exercise-db/internal/model"
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// countingStore wraps a real media.Store and counts Get calls: the storage
// read a revalidation must not make once the hash is recorded.
type countingStore struct {
	media.Store
	gets atomic.Int64
}

func (c *countingStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	c.gets.Add(1)
	return c.Store.Get(ctx, key)
}

// seedHashedVariant writes bytes and a variant row that records their hash,
// the state every writer leaves behind after migration 0005.
func seedHashedVariant(t *testing.T, ctx context.Context, s *store.Store, ms media.Store, imageID, name, key string, body []byte) {
	t.Helper()
	if err := ms.Put(ctx, key, bytes.NewReader(body), "image/jpeg"); err != nil {
		t.Fatalf("write %s: %v", key, err)
	}
	hash := media.ContentSHA256(body)
	w, h := 10, 10
	if _, err := s.Media.UpsertImageVariant(ctx, store.UpsertImageVariantInput{
		ImageID: imageID, Name: name, StorageKey: key, ContentType: "image/jpeg",
		Width: &w, Height: &h, ContentSHA256: &hash,
	}); err != nil {
		t.Fatalf("seed variant %s: %v", name, err)
	}
}

// TestImageRoutes_304DoesNotReadTheObject is the point of migration 0005: a
// revalidation of an image whose hash is recorded is answered from the
// database row alone, on both routes.
func TestImageRoutes_304DoesNotReadTheObject(t *testing.T) {
	s, _ := newTestStore(t)
	ms := &countingStore{Store: newTestMediaStore(t)}
	ctx := context.Background()

	fullBytes := []byte("full-bytes-with-a-recorded-hash")
	thumbBytes := []byte("thumb-bytes")
	id := seedImage(t, ctx, s, ms, "exercises/Cheap_304/0.jpg", fullBytes)
	seedHashedVariant(t, ctx, s, ms, id, model.VariantFull, "exercises/Cheap_304/0.jpg", fullBytes)
	seedHashedVariant(t, ctx, s, ms, id, model.VariantThumb, "exercises/Cheap_304/0.thumb.jpg", thumbBytes)

	_, testAPI := humatest.New(t)
	registerImageRoutes(testAPI, s, ms)

	for _, tc := range []struct {
		path string
		body []byte
	}{
		{"/api/v1/images/" + id, fullBytes},
		{"/api/v1/images/" + id + "/thumb", thumbBytes},
	} {
		t.Run(tc.path, func(t *testing.T) {
			first := testAPI.Get(tc.path)
			if first.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", first.Code)
			}
			// The recorded hash must produce the same ETag the bytes always
			// did, or every client cache invalidates at deploy.
			if got, want := first.Header().Get("ETag"), etagHeader(etagValueBytes(tc.body)); got != want {
				t.Errorf("ETag = %s, want %s (the byte-derived tag)", got, want)
			}

			before := ms.gets.Load()
			resp := testAPI.Get(tc.path, "If-None-Match: "+first.Header().Get("ETag"))
			if resp.Code != http.StatusNotModified {
				t.Fatalf("revalidation status = %d, want 304", resp.Code)
			}
			if n := ms.gets.Load() - before; n != 0 {
				t.Errorf("304 read the media store %d time(s), want 0", n)
			}
			if got := resp.Header().Get("Cache-Control"); got != imageCacheControl {
				t.Errorf("304 Cache-Control = %q, want %q", got, imageCacheControl)
			}

			before = ms.gets.Load()
			miss := testAPI.Get(tc.path, `If-None-Match: "0000"`)
			if miss.Code != http.StatusOK || !bytes.Equal(miss.Body.Bytes(), tc.body) {
				t.Errorf("non-matching revalidation = %d %q, want 200 with the bytes", miss.Code, miss.Body.String())
			}
			if n := ms.gets.Load() - before; n != 1 {
				t.Errorf("200 read the media store %d time(s), want 1", n)
			}
		})
	}
}

// TestGetImage_UnrecordedHashFallsBackToBytes pins the NULL case: images
// written before migration 0005 (and any writer that does not state a hash)
// still revalidate correctly, by reading and hashing the object as before.
func TestGetImage_UnrecordedHashFallsBackToBytes(t *testing.T) {
	s, _ := newTestStore(t)
	ms := &countingStore{Store: newTestMediaStore(t)}
	ctx := context.Background()

	body := []byte("no-hash-recorded")
	id := seedImage(t, ctx, s, ms, "exercises/No_Hash/0.jpg", body)

	_, testAPI := humatest.New(t)
	registerImageRoutes(testAPI, s, ms)

	resp := testAPI.Get("/api/v1/images/"+id, "If-None-Match: "+etagHeader(etagValueBytes(body)))
	if resp.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", resp.Code)
	}
	if ms.gets.Load() != 1 {
		t.Errorf("media store reads = %d, want 1 (no hash recorded, so the bytes are the only source)", ms.gets.Load())
	}
}

// TestGetImage_StaleHashServesTheTrueETag covers bytes replaced under an id
// without the row being updated. A 200 must describe the bytes it carries,
// or the client caches new bytes under the old tag and revalidates wrongly
// forever after.
func TestGetImage_StaleHashServesTheTrueETag(t *testing.T) {
	s, _ := newTestStore(t)
	ms := newTestMediaStore(t)
	ctx := context.Background()

	key := "exercises/Stale_Hash/0.jpg"
	id := seedImage(t, ctx, s, ms, key, []byte("old"))
	seedHashedVariant(t, ctx, s, ms, id, model.VariantFull, key, []byte("old"))
	// Out of band: new bytes, row untouched.
	if err := ms.Put(ctx, key, bytes.NewReader([]byte("new")), "image/jpeg"); err != nil {
		t.Fatal(err)
	}

	_, testAPI := humatest.New(t)
	registerImageRoutes(testAPI, s, ms)

	resp := testAPI.Get("/api/v1/images/" + id)
	if resp.Code != http.StatusOK || resp.Body.String() != "new" {
		t.Fatalf("got %d %q, want 200 \"new\"", resp.Code, resp.Body.String())
	}
	if got, want := resp.Header().Get("ETag"), etagHeader(etagValueBytes([]byte("new"))); got != want {
		t.Errorf("ETag = %s, want %s (the bytes actually sent)", got, want)
	}
}

// TestGetImageVariant_MissingThumbServesFull is the fallback contract: a
// client asking for a thumb that does not exist yet gets the image, not a
// broken one, and the stand-in is not cached as if it were the thumb.
func TestGetImageVariant_MissingThumbServesFull(t *testing.T) {
	s, _ := newTestStore(t)
	ms := newTestMediaStore(t)
	ctx := context.Background()

	fullBytes := []byte("full-only")
	id := seedImage(t, ctx, s, ms, "exercises/No_Thumb/0.jpg", fullBytes)
	seedHashedVariant(t, ctx, s, ms, id, model.VariantFull, "exercises/No_Thumb/0.jpg", fullBytes)

	_, testAPI := humatest.New(t)
	registerImageRoutes(testAPI, s, ms)

	resp := testAPI.Get("/api/v1/images/" + id + "/thumb")
	if resp.Code != http.StatusOK || !bytes.Equal(resp.Body.Bytes(), fullBytes) {
		t.Fatalf("got %d %q, want 200 with the full bytes", resp.Code, resp.Body.String())
	}
	// Immutable here would pin the full image to the thumb URL for a year
	// after the backfill makes the real thumb.
	if got := resp.Header().Get("Cache-Control"); got != cacheControl {
		t.Errorf("stand-in Cache-Control = %q, want the short %q", got, cacheControl)
	}

	// Once the thumb exists, the old tag no longer matches and the client
	// gets the small bytes on its next revalidation.
	staleTag := resp.Header().Get("ETag")
	thumbBytes := []byte("real-thumb")
	seedHashedVariant(t, ctx, s, ms, id, model.VariantThumb, "exercises/No_Thumb/0.thumb.jpg", thumbBytes)
	after := testAPI.Get("/api/v1/images/"+id+"/thumb", "If-None-Match: "+staleTag)
	if after.Code != http.StatusOK || !bytes.Equal(after.Body.Bytes(), thumbBytes) {
		t.Fatalf("after backfill got %d %q, want 200 with the thumb", after.Code, after.Body.String())
	}
	if got := after.Header().Get("Cache-Control"); got != imageCacheControl {
		t.Errorf("real thumb Cache-Control = %q, want %q", got, imageCacheControl)
	}

	// The fallback is for renditions this server makes, not for any name.
	if r := testAPI.Get("/api/v1/images/" + id + "/medium"); r.Code != http.StatusNotFound {
		t.Errorf("unknown rendition status = %d, want 404", r.Code)
	}
}

// TestGetImageVariant_MissingThumbRespectsVisibility: the fallback goes
// through the same approval gate, so a pending upload is not reachable at
// /thumb just because it has no thumb row.
func TestGetImageVariant_MissingThumbRespectsVisibility(t *testing.T) {
	s, pool := newTestStore(t)
	ms := newTestMediaStore(t)
	ctx := context.Background()

	id := seedImage(t, ctx, s, ms, "exercises/Pending_No_Thumb/0.jpg", []byte("pending"))
	if _, err := pool.Exec(ctx, `UPDATE exercise_images SET status = 'pending' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}

	_, err := GetImageVariant(ctx, s, ms, &GetImageVariantInput{ID: id, Variant: model.VariantThumb})
	if got := huma404Status(err); got != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a pending image", got)
	}
}
