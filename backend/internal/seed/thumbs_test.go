package seed

import (
	"context"
	"testing"

	"github.com/Cawlumm/open-exercise-db/internal/model"
)

// TestImport_WritesThumbAndHashes: a newly imported image arrives with a
// thumb and recorded hashes, so it needs nothing from cmd/backfill-thumbs
// and its 304s never read the bucket.
func TestImport_WritesThumbAndHashes(t *testing.T) {
	st, pool := newTestStore(t)
	ctx := context.Background()

	server := fixtureServer(t, []fedExercise{benchPressFixture()}, testGoldenJPEG(t), false)
	mediaStore := newCountingStore(t)
	if _, err := Import(ctx, st, testOptions(server, mediaStore)); err != nil {
		t.Fatalf("Import: %v", err)
	}

	ex, err := st.Exercises.GetBySourceID(ctx, "Bench_Press")
	if err != nil {
		t.Fatal(err)
	}
	ids := imageIDsForRevision(t, ctx, pool, *ex.CurrentRevisionID)
	imgs, err := st.Media.ListImagesByIDs(ctx, ids)
	if err != nil || len(imgs) == 0 {
		t.Fatalf("ListImagesByIDs = %d images, %v", len(imgs), err)
	}
	for _, img := range imgs {
		if len(img.Variants) != 2 || img.Variants[0].Name != model.VariantFull || img.Variants[1].Name != model.VariantThumb {
			t.Fatalf("image %s variants = %+v, want full then thumb", img.ID, img.Variants)
		}
		for _, v := range img.Variants {
			if v.ContentSHA256 == nil {
				t.Errorf("image %s %s: no content hash recorded", img.ID, v.Name)
			}
			if ok, err := mediaStore.Exists(ctx, v.StorageKey); err != nil || !ok {
				t.Errorf("image %s %s: object %s not stored (%v)", img.ID, v.Name, v.StorageKey, err)
			}
		}
	}

	// Nothing left for the backfill to do.
	pending, err := st.Media.ListImagesNeedingThumb(ctx, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("%d imported image(s) still need a thumb", len(pending))
	}
}
