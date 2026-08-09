package api

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/Cawlumm/open-exercise-db/internal/media"
	"github.com/Cawlumm/open-exercise-db/internal/model"
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// huma404Status reports err's HTTP status, or 0 when it is not a
// huma.StatusError — so an assertion reads as "wrong status" rather than
// panicking on a type assertion.
func huma404Status(err error) int {
	statusErr, ok := err.(huma.StatusError)
	if !ok {
		return 0
	}
	return statusErr.GetStatus()
}

// seedVariant records a rendition row and writes its bytes, so a test can
// build the multi-rendition state the dataset does not have yet — today
// every image has exactly "full", and the point of these tests is that the
// second one costs nothing when it arrives.
func seedVariant(t *testing.T, ctx context.Context, s *store.Store, mediaStore media.Store, imageID, name, storageKey string, body []byte, w, h int) {
	t.Helper()

	if err := mediaStore.Put(ctx, storageKey, bytes.NewReader(body), "image/jpeg"); err != nil {
		t.Fatalf("write variant bytes: %v", err)
	}
	if _, err := s.Media.UpsertImageVariant(ctx, store.UpsertImageVariantInput{
		ImageID:     imageID,
		Name:        name,
		StorageKey:  storageKey,
		ContentType: "image/jpeg",
		Width:       &w,
		Height:      &h,
	}); err != nil {
		t.Fatalf("seed variant %s: %v", name, err)
	}
}

// TestGetImageVariant_ServesTheNamedRendition is the core of the feature:
// distinct names return distinct bytes. Without this the route could return
// the full image for every name and every other test here would still pass.
func TestGetImageVariant_ServesTheNamedRendition(t *testing.T) {
	s, _ := newTestStore(t)
	mediaStore := newTestMediaStore(t)
	ctx := context.Background()

	fullBytes := []byte("full-resolution-bytes")
	thumbBytes := []byte("thumb")

	imageID := seedImage(t, ctx, s, mediaStore, "exercises/Variant_Test/0.jpg", fullBytes)
	seedVariant(t, ctx, s, mediaStore, imageID, model.VariantFull, "exercises/Variant_Test/0.jpg", fullBytes, 800, 600)
	seedVariant(t, ctx, s, mediaStore, imageID, "thumb", "exercises/Variant_Test/0/thumb.jpg", thumbBytes, 200, 150)

	t.Run("full", func(t *testing.T) {
		out, err := GetImageVariant(ctx, s, mediaStore, &GetImageVariantInput{ID: imageID, Variant: model.VariantFull})
		if err != nil {
			t.Fatalf("GetImageVariant(full): %v", err)
		}
		if !bytes.Equal(out.Body, fullBytes) {
			t.Errorf("full body = %q, want %q", out.Body, fullBytes)
		}
	})

	t.Run("thumb", func(t *testing.T) {
		out, err := GetImageVariant(ctx, s, mediaStore, &GetImageVariantInput{ID: imageID, Variant: "thumb"})
		if err != nil {
			t.Fatalf("GetImageVariant(thumb): %v", err)
		}
		if !bytes.Equal(out.Body, thumbBytes) {
			t.Errorf("thumb body = %q, want %q — a rendition name must not resolve to the full image", out.Body, thumbBytes)
		}
		if out.ContentType != "image/jpeg" {
			t.Errorf("content type = %q, want image/jpeg", out.ContentType)
		}
	})

	// The failure this guards is silent and expensive: a client asking for a
	// thumbnail and receiving the full image sees a successful request while
	// downloading many times the bytes it budgeted for.
	t.Run("unknown name is 404, never a fallback", func(t *testing.T) {
		_, err := GetImageVariant(ctx, s, mediaStore, &GetImageVariantInput{ID: imageID, Variant: "medium"})
		if err == nil {
			t.Fatal("GetImageVariant on an unknown rendition returned no error — it must not fall back to full")
		}
		if got := huma404Status(err); got != http.StatusNotFound {
			t.Errorf("status = %d, want 404", got)
		}
	})

	t.Run("unknown image is 404", func(t *testing.T) {
		_, err := GetImageVariant(ctx, s, mediaStore, &GetImageVariantInput{
			ID: "00000000-0000-0000-0000-000000000000", Variant: model.VariantFull,
		})
		if err == nil {
			t.Fatal("GetImageVariant on an unknown image returned no error")
		}
		if got := huma404Status(err); got != http.StatusNotFound {
			t.Errorf("status = %d, want 404", got)
		}
	})
}

// TestGetImageVariant_MissingObjectIs404 mirrors GetImage's own rule. It
// matters more for variants than for images: a partially-completed
// re-encode leaves rows for renditions whose bytes were never written, and
// as 5xx those are uncacheable and indistinguishable from the API being
// broken.
func TestGetImageVariant_MissingObjectIs404(t *testing.T) {
	s, _ := newTestStore(t)
	mediaStore := newTestMediaStore(t)
	ctx := context.Background()

	imageID := seedImage(t, ctx, s, mediaStore, "exercises/Missing_Variant/0.jpg", []byte("full"))

	// A row whose object was never written.
	if _, err := s.Media.UpsertImageVariant(ctx, store.UpsertImageVariantInput{
		ImageID:     imageID,
		Name:        "thumb",
		StorageKey:  "exercises/Missing_Variant/0/thumb.jpg",
		ContentType: "image/jpeg",
	}); err != nil {
		t.Fatalf("seed variant row: %v", err)
	}

	_, err := GetImageVariant(ctx, s, mediaStore, &GetImageVariantInput{ID: imageID, Variant: "thumb"})
	if err == nil {
		t.Fatal("expected an error for a variant whose object is missing")
	}
	if got := huma404Status(err); got != http.StatusNotFound {
		t.Errorf("status = %d, want 404 — a missing object is not a server error", got)
	}
}

// TestImageVariants_AreAdditiveInTheContract is the compatibility
// assertion. lyftr and every other existing consumer read url/width/height
// and must keep getting exactly what they got before this change; variants
// is new information beside them, not a replacement.
func TestImageVariants_AreAdditiveInTheContract(t *testing.T) {
	s, _ := newTestStore(t)
	mediaStore := newTestMediaStore(t)
	ctx := context.Background()

	imageID := seedImage(t, ctx, s, mediaStore, "exercises/Additive_Test/0.jpg", []byte("full"))
	seedVariant(t, ctx, s, mediaStore, imageID, model.VariantFull, "exercises/Additive_Test/0.jpg", []byte("full"), 800, 600)
	seedVariant(t, ctx, s, mediaStore, imageID, "thumb", "exercises/Additive_Test/0/thumb.jpg", []byte("thumb"), 200, 150)

	images, err := s.Media.ListImagesByIDs(ctx, []string{imageID})
	if err != nil {
		t.Fatalf("ListImagesByIDs: %v", err)
	}
	built := media.BuildImages(images, "")
	if len(built) != 1 {
		t.Fatalf("built %d images, want 1", len(built))
	}
	img := built[0]

	if want := "/api/v1/images/" + imageID; img.URL != want {
		t.Errorf("url = %q, want %q — the top-level url must not change shape", img.URL, want)
	}
	if img.Width == nil || *img.Width != 800 {
		t.Errorf("width = %v, want 800 (the full rendition's)", img.Width)
	}

	if len(img.Variants) != 2 {
		t.Fatalf("variants = %d, want 2", len(img.Variants))
	}

	// Full first, so a client reading variants[0] as the primary gets the
	// same image the top-level url points at.
	if img.Variants[0].Name != model.VariantFull {
		t.Errorf("variants[0].name = %q, want %q", img.Variants[0].Name, model.VariantFull)
	}
	// Same bytes must not be advertised at two URLs: that splits caches and
	// makes the ETags incomparable.
	if img.Variants[0].URL != img.URL {
		t.Errorf("full variant url = %q, want it identical to image url %q", img.Variants[0].URL, img.URL)
	}
	if img.Variants[1].Name != "thumb" {
		t.Errorf("variants[1].name = %q, want thumb", img.Variants[1].Name)
	}
	if want := "/api/v1/images/" + imageID + "/thumb"; img.Variants[1].URL != want {
		t.Errorf("thumb url = %q, want %q", img.Variants[1].URL, want)
	}
	if img.Variants[1].Width == nil || *img.Variants[1].Width != 200 {
		t.Errorf("thumb width = %v, want 200 — a variant must report its own dimensions", img.Variants[1].Width)
	}
}

// TestImageVariants_ExternalHostUsesStorageKeys pins that variant URLs
// follow IMAGE_BASE_URL the same way image URLs do. A variant that stayed
// on this API's own route when everything else moved to a CDN would quietly
// route thumbnail traffic — the highest-volume image requests, one per grid
// cell — back through the origin.
func TestImageVariants_ExternalHostUsesStorageKeys(t *testing.T) {
	s, _ := newTestStore(t)
	mediaStore := newTestMediaStore(t)
	ctx := context.Background()

	imageID := seedImage(t, ctx, s, mediaStore, "exercises/CDN_Test/0.jpg", []byte("full"))
	seedVariant(t, ctx, s, mediaStore, imageID, model.VariantFull, "exercises/CDN_Test/0.jpg", []byte("full"), 800, 600)
	seedVariant(t, ctx, s, mediaStore, imageID, "thumb", "exercises/CDN_Test/0/thumb.jpg", []byte("thumb"), 200, 150)

	images, err := s.Media.ListImagesByIDs(ctx, []string{imageID})
	if err != nil {
		t.Fatalf("ListImagesByIDs: %v", err)
	}
	built := media.BuildImages(images, "https://images.example.org")
	img := built[0]

	if want := "https://images.example.org/exercises/CDN_Test/0.jpg"; img.URL != want {
		t.Errorf("url = %q, want %q", img.URL, want)
	}
	if want := "https://images.example.org/exercises/CDN_Test/0/thumb.jpg"; img.Variants[1].URL != want {
		t.Errorf("thumb url = %q, want %q — variants must follow IMAGE_BASE_URL too", img.Variants[1].URL, want)
	}
}

// TestImageVariants_MissingRowsStillRenderFull covers the defensive path in
// media.buildVariants. It should be unreachable — migration 0002 backfilled
// every image and every writer since creates a row — but publishing
// "variants": [] beside a working url would tell a variant-aware client the
// image has no renditions and invite it to render nothing.
func TestImageVariants_MissingRowsStillRenderFull(t *testing.T) {
	s, _ := newTestStore(t)
	mediaStore := newTestMediaStore(t)
	ctx := context.Background()

	// Seeded through InsertImage only — deliberately no variant row.
	imageID := seedImage(t, ctx, s, mediaStore, "exercises/No_Variant_Row/0.jpg", []byte("full"))

	images, err := s.Media.ListImagesByIDs(ctx, []string{imageID})
	if err != nil {
		t.Fatalf("ListImagesByIDs: %v", err)
	}
	img := media.BuildImages(images, "")[0]

	if len(img.Variants) != 1 {
		t.Fatalf("variants = %d, want 1 synthesized entry", len(img.Variants))
	}
	if img.Variants[0].Name != model.VariantFull {
		t.Errorf("synthesized variant name = %q, want %q", img.Variants[0].Name, model.VariantFull)
	}
	if img.Variants[0].URL != img.URL {
		t.Errorf("synthesized variant url = %q, want it identical to image url %q", img.Variants[0].URL, img.URL)
	}
}

// TestUpsertImageVariant_ReEncodeReplacesRatherThanDuplicates pins the
// (image_id, name) uniqueness the importer's repair path relies on: a
// re-encode must re-point the rendition, not accumulate a second row that
// makes the response list the same name twice.
func TestUpsertImageVariant_ReEncodeReplacesRatherThanDuplicates(t *testing.T) {
	s, pool := newTestStore(t)
	mediaStore := newTestMediaStore(t)
	ctx := context.Background()

	imageID := seedImage(t, ctx, s, mediaStore, "exercises/Reencode_Test/0.jpg", []byte("full"))
	seedVariant(t, ctx, s, mediaStore, imageID, "thumb", "exercises/Reencode_Test/0/thumb.jpg", []byte("v1"), 200, 150)
	seedVariant(t, ctx, s, mediaStore, imageID, "thumb", "exercises/Reencode_Test/0/thumb.jpg", []byte("v2"), 240, 180)

	variant, err := s.Media.GetImageVariant(ctx, imageID, "thumb")
	if err != nil {
		t.Fatalf("GetImageVariant: %v", err)
	}
	if variant.Width == nil || *variant.Width != 240 {
		t.Errorf("width = %v, want 240 — a re-encode must update the row", variant.Width)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM exercise_image_variants WHERE image_id = $1 AND name = 'thumb'`,
		imageID,
	).Scan(&count); err != nil {
		t.Fatalf("count variants: %v", err)
	}
	if count != 1 {
		t.Errorf("thumb rows = %d, want 1", count)
	}
}
