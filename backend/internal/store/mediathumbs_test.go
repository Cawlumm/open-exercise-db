package store

import (
	"context"
	"strings"
	"testing"

	"github.com/Cawlumm/open-exercise-db/internal/model"
)

func insertTestImage(t *testing.T, s *Store, key string) string {
	t.Helper()
	w, h := 800, 600
	id, err := s.Media.InsertImage(context.Background(), InsertImageInput{
		StorageKey: key, ContentType: "image/jpeg", Width: &w, Height: &h, Status: model.ImageStatusApproved,
	})
	if err != nil {
		t.Fatalf("InsertImage: %v", err)
	}
	return id
}

func upsertTestVariant(t *testing.T, s *Store, imageID, name, key string, hash *string) {
	t.Helper()
	if _, err := s.Media.UpsertImageVariant(context.Background(), UpsertImageVariantInput{
		ImageID: imageID, Name: name, StorageKey: key, ContentType: "image/jpeg", ContentSHA256: hash,
	}); err != nil {
		t.Fatalf("UpsertImageVariant(%s): %v", name, err)
	}
}

// TestListImagesNeedingThumb pins the predicate cmd/backfill-thumbs resumes
// by: an image is selected until it has a thumb row and a hashed full row,
// and the id cursor pages forward.
func TestListImagesNeedingThumb(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	hash := strings.Repeat("a", 64)

	noRows := insertTestImage(t, s, "exercises/A/0.jpg")
	unhashed := insertTestImage(t, s, "exercises/B/0.jpg")
	upsertTestVariant(t, s, unhashed, model.VariantFull, "exercises/B/0.jpg", nil)
	upsertTestVariant(t, s, unhashed, model.VariantThumb, "exercises/B/0.thumb.jpg", &hash)
	noThumb := insertTestImage(t, s, "exercises/C/0.jpg")
	upsertTestVariant(t, s, noThumb, model.VariantFull, "exercises/C/0.jpg", &hash)
	done := insertTestImage(t, s, "exercises/D/0.jpg")
	upsertTestVariant(t, s, done, model.VariantFull, "exercises/D/0.jpg", &hash)
	upsertTestVariant(t, s, done, model.VariantThumb, "exercises/D/0.thumb.jpg", &hash)

	var got []string
	cursor := ""
	for {
		page, err := s.Media.ListImagesNeedingThumb(ctx, cursor, 1)
		if err != nil {
			t.Fatalf("ListImagesNeedingThumb: %v", err)
		}
		if len(page) == 0 {
			break
		}
		got = append(got, page[0].ID)
		cursor = page[0].ID
	}

	want := map[string]bool{noRows: true, unhashed: true, noThumb: true}
	if len(got) != len(want) {
		t.Fatalf("selected %d images, want %d", len(got), len(want))
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("selected %s, which already has a thumb and a hashed full row", id)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Errorf("not in id order: %v", got)
		}
	}
}

// TestContentSHA256_RoundTripsAndClears pins the read paths that serve the
// cheap 304, and that a re-upsert without a hash clears it rather than
// leaving one that may describe replaced bytes.
func TestContentSHA256_RoundTripsAndClears(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	hash := strings.Repeat("b", 64)

	id := insertTestImage(t, s, "exercises/E/0.jpg")
	upsertTestVariant(t, s, id, model.VariantFull, "exercises/E/0.jpg", &hash)

	img, err := s.Media.GetImageByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if img.ContentSHA256 == nil || *img.ContentSHA256 != hash {
		t.Errorf("GetImageByID hash = %v, want %s", img.ContentSHA256, hash)
	}
	v, _, err := s.Media.GetImageVariant(ctx, id, model.VariantFull)
	if err != nil {
		t.Fatal(err)
	}
	if v.ContentSHA256 == nil || *v.ContentSHA256 != hash {
		t.Errorf("GetImageVariant hash = %v, want %s", v.ContentSHA256, hash)
	}

	upsertTestVariant(t, s, id, model.VariantFull, "exercises/E/0.jpg", nil)
	img, err = s.Media.GetImageByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if img.ContentSHA256 != nil {
		t.Errorf("hash survived a re-upsert that did not state one: %s", *img.ContentSHA256)
	}

	// An image with no full row still resolves, with no hash.
	bare := insertTestImage(t, s, "exercises/F/0.jpg")
	img, err = s.Media.GetImageByID(ctx, bare)
	if err != nil {
		t.Fatalf("GetImageByID without a variant row: %v", err)
	}
	if img.ContentSHA256 != nil {
		t.Error("hash reported for an image with no full variant row")
	}
}
