package main

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/Cawlumm/open-exercise-db/internal/media"
	"github.com/Cawlumm/open-exercise-db/internal/model"
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// fakeRows models exercise_images + exercise_image_variants closely enough
// to run the real selection predicate: an image needs work until it has a
// thumb row and a full row with a hash. Mirrors
// store.MediaStore.ListImagesNeedingThumb, whose SQL is pinned separately
// against Postgres in internal/store.
type fakeRows struct {
	images   []model.Image                                       // sorted by id
	variants map[string]map[string]store.UpsertImageVariantInput // image id -> name -> row
	upserts  int
}

func newFakeRows(imgs ...model.Image) *fakeRows {
	sort.Slice(imgs, func(i, j int) bool { return imgs[i].ID < imgs[j].ID })
	return &fakeRows{images: imgs, variants: map[string]map[string]store.UpsertImageVariantInput{}}
}

func (f *fakeRows) needsWork(id string) bool {
	_, hasThumb := f.variants[id][model.VariantThumb]
	full, hasFull := f.variants[id][model.VariantFull]
	return !hasThumb || !hasFull || full.ContentSHA256 == nil
}

func (f *fakeRows) ListImagesNeedingThumb(_ context.Context, afterID string, limit int) ([]model.Image, error) {
	var out []model.Image
	for _, img := range f.images {
		if img.ID <= afterID || !f.needsWork(img.ID) {
			continue
		}
		out = append(out, img)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *fakeRows) UpsertImageVariant(_ context.Context, in store.UpsertImageVariantInput) (string, error) {
	f.upserts++
	if f.variants[in.ImageID] == nil {
		f.variants[in.ImageID] = map[string]store.UpsertImageVariantInput{}
	}
	f.variants[in.ImageID][in.Name] = in
	return in.ImageID + "/" + in.Name, nil
}

// memStore is an in-memory media.Store that counts writes.
type memStore struct {
	objects map[string][]byte
	puts    int
}

func (m *memStore) Put(_ context.Context, key string, r io.Reader, _ string) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.puts++
	m.objects[key] = b
	return nil
}

func (m *memStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	b, ok := m.objects[key]
	if !ok {
		return nil, media.ErrObjectNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (m *memStore) Exists(_ context.Context, key string) (bool, error) {
	_, ok := m.objects[key]
	return ok, nil
}

func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 90, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// dataset builds n images with their full bytes stored and, like
// production before this change, a full variant row with no hash.
func dataset(t *testing.T, n int) (*fakeRows, *memStore) {
	t.Helper()
	ms := &memStore{objects: map[string][]byte{}}
	var imgs []model.Image
	full := jpegBytes(t, 800, 600)
	for i := 0; i < n; i++ {
		id := "00000000-0000-0000-0000-00000000000" + string(rune('a'+i))
		key := "exercises/Ex_" + string(rune('A'+i)) + "/0.jpg"
		ms.objects[key] = full
		w, h := 800, 600
		imgs = append(imgs, model.Image{ID: id, StorageKey: key, ContentType: "image/jpeg", Width: &w, Height: &h})
	}
	rows := newFakeRows(imgs...)
	for _, img := range imgs {
		rows.variants[img.ID] = map[string]store.UpsertImageVariantInput{
			model.VariantFull: {ImageID: img.ID, Name: model.VariantFull, StorageKey: img.StorageKey},
		}
	}
	return rows, ms
}

func TestRun_BackfillsThumbAndFullHash(t *testing.T) {
	rows, ms := dataset(t, 3)
	ctx := context.Background()

	res, err := run(ctx, rows, ms, options{batch: 2})
	if err != nil {
		t.Fatal(err)
	}
	if res.done != 3 || res.failed != 0 {
		t.Fatalf("result = %+v, want 3 done, 0 failed", res)
	}

	for _, img := range rows.images {
		full := rows.variants[img.ID][model.VariantFull]
		if full.ContentSHA256 == nil || *full.ContentSHA256 != media.ContentSHA256(ms.objects[img.StorageKey]) {
			t.Errorf("%s: full hash not recorded from the stored bytes", img.ID)
		}
		thumb, ok := rows.variants[img.ID][model.VariantThumb]
		if !ok {
			t.Fatalf("%s: no thumb row", img.ID)
		}
		if thumb.StorageKey != media.ThumbStorageKey(img.StorageKey) {
			t.Errorf("%s: thumb key = %q", img.ID, thumb.StorageKey)
		}
		stored, ok := ms.objects[thumb.StorageKey]
		if !ok {
			t.Fatalf("%s: thumb row names an object that was not written", img.ID)
		}
		if *thumb.ContentSHA256 != media.ContentSHA256(stored) {
			t.Errorf("%s: thumb hash does not describe the stored thumb", img.ID)
		}
		if *thumb.Width != 128 || *thumb.Height != 96 {
			t.Errorf("%s: thumb %dx%d, want 128x96", img.ID, *thumb.Width, *thumb.Height)
		}
	}
}

// A second run must find nothing and write nothing: this is what makes it
// safe for the owner to re-run after a partial or uncertain first run.
func TestRun_SecondRunIsANoOp(t *testing.T) {
	rows, ms := dataset(t, 3)
	ctx := context.Background()
	if _, err := run(ctx, rows, ms, options{batch: 10}); err != nil {
		t.Fatal(err)
	}
	puts, upserts := ms.puts, rows.upserts

	res, err := run(ctx, rows, ms, options{batch: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.seen != 0 || ms.puts != puts || rows.upserts != upserts {
		t.Errorf("second run saw %d and wrote %d objects / %d rows, want nothing", res.seen, ms.puts-puts, rows.upserts-upserts)
	}
}

func TestRun_DryRunWritesNothing(t *testing.T) {
	rows, _ := dataset(t, 3)
	var out bytes.Buffer

	// No media store at all: a dry run must not need one, which is also
	// how main proves it reads nothing.
	res, err := run(context.Background(), rows, nil, options{dryRun: true, batch: 2, out: &out})
	if err != nil {
		t.Fatal(err)
	}
	if res.seen != 3 || res.done != 0 {
		t.Errorf("result = %+v, want 3 seen, 0 done", res)
	}
	if rows.upserts != 0 {
		t.Errorf("dry run wrote %d rows", rows.upserts)
	}
	if lines := strings.Count(out.String(), "\n"); lines != 3 {
		t.Errorf("dry run listed %d images, want 3:\n%s", lines, out.String())
	}
}

// -limit bounds one run and a later run resumes where it stopped.
func TestRun_LimitThenResume(t *testing.T) {
	rows, ms := dataset(t, 5)
	ctx := context.Background()

	res, err := run(ctx, rows, ms, options{limit: 2, batch: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.done != 2 {
		t.Fatalf("limited run did %d, want 2", res.done)
	}

	res, err = run(ctx, rows, ms, options{batch: 2})
	if err != nil {
		t.Fatal(err)
	}
	if res.done != 3 {
		t.Errorf("resumed run did %d, want the remaining 3", res.done)
	}
	for _, img := range rows.images {
		if rows.needsWork(img.ID) {
			t.Errorf("%s still needs work after resume", img.ID)
		}
	}
}

// A missing object fails that image without stopping the run or looping on
// it, and the next run (after the object is restored) picks it up.
func TestRun_FailureIsSkippedAndRetriedNextRun(t *testing.T) {
	rows, ms := dataset(t, 3)
	ctx := context.Background()
	broken := rows.images[1]
	saved := ms.objects[broken.StorageKey]
	delete(ms.objects, broken.StorageKey)

	res, err := run(ctx, rows, ms, options{batch: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.done != 2 || res.failed != 1 {
		t.Fatalf("result = %+v, want 2 done, 1 failed", res)
	}
	if _, ok := rows.variants[broken.ID][model.VariantThumb]; ok {
		t.Error("a thumb row was written for an image whose full bytes could not be read")
	}

	ms.objects[broken.StorageKey] = saved
	res, err = run(ctx, rows, ms, options{batch: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.done != 1 || res.failed != 0 {
		t.Errorf("retry result = %+v, want 1 done", res)
	}
}

// An image with no full variant row at all (an app upload from before
// uploads wrote one) gains one, described from exercise_images.
func TestRun_CreatesMissingFullRow(t *testing.T) {
	rows, ms := dataset(t, 1)
	id := rows.images[0].ID
	delete(rows.variants, id)

	if _, err := run(context.Background(), rows, ms, options{batch: 1}); err != nil {
		t.Fatal(err)
	}
	full, ok := rows.variants[id][model.VariantFull]
	if !ok || full.StorageKey != rows.images[0].StorageKey || full.ContentSHA256 == nil {
		t.Errorf("full row = %+v (present %v), want one naming the image's key with a hash", full, ok)
	}
}

func TestRun_CancelledContextStopsAsInterrupted(t *testing.T) {
	rows, ms := dataset(t, 2)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := run(ctx, rows, ms, options{batch: 1})
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if !res.interrupted || res.done != 0 {
		t.Errorf("result = %+v, want interrupted with nothing done", res)
	}
}
