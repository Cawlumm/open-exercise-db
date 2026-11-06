package seed

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Cawlumm/open-exercise-db/internal/media"
)

// The tests here cover what happens when exercise_images rows and the
// objects they name disagree — the state a deployment lands in when its
// media storage is lost while Postgres persists. Before the repair path,
// processImages read the row as proof of the object and reported "reused"
// for images that no longer existed, so re-running the importer (the
// documented fix for a broken import) could not restore them and nothing in
// the output said so.

// countingStore wraps a media.Store and records what was asked of it, so a
// test can assert the importer skipped or performed real work rather than
// inferring it from side effects. It can also drop objects behind the
// importer's back (evict) and fail Exists (existsErr), the two conditions
// that separate reuse from repair.
type countingStore struct {
	inner     media.Store
	existsErr error

	puts      int
	existsHit int
}

func (c *countingStore) Put(ctx context.Context, key string, r io.Reader, contentType string) error {
	c.puts++
	return c.inner.Put(ctx, key, r, contentType)
}

func (c *countingStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return c.inner.Get(ctx, key)
}

func (c *countingStore) Exists(ctx context.Context, key string) (bool, error) {
	c.existsHit++
	if c.existsErr != nil {
		return false, c.existsErr
	}
	return c.inner.Exists(ctx, key)
}

// evictAll simulates storage losing every object while the database keeps
// its rows: a wiped container layer, an unmounted volume, a recreated
// bucket. It reaches through to a fresh FilesystemStore rooted somewhere
// else rather than deleting files, which is the same observable state and
// works regardless of how the inner store is implemented.
func (c *countingStore) evictAll(t *testing.T) {
	t.Helper()
	empty, err := media.NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilesystemStore: %v", err)
	}
	c.inner = empty
}

func newCountingStore(t *testing.T) *countingStore {
	t.Helper()
	return &countingStore{inner: newTestMediaStore(t)}
}

// imageIDsForRevision reads a revision's image_ids straight from the column,
// which is the thing repair must not disturb — reading it through the store
// would test the reader instead.
func imageIDsForRevision(t *testing.T, ctx context.Context, pool *pgxpool.Pool, revisionID string) []string {
	t.Helper()
	var ids []string
	if err := pool.QueryRow(ctx, `SELECT image_ids FROM exercise_revisions WHERE id = $1`, revisionID).Scan(&ids); err != nil {
		t.Fatalf("read image_ids for revision %s: %v", revisionID, err)
	}
	return ids
}

// countRowString is countRow for a text column.
func countRowString(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, dest *string, args ...any) {
	t.Helper()
	if err := pool.QueryRow(ctx, sql, args...).Scan(dest); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
}

// deadImageServer serves the dataset but 404s every image, unlike
// fixtureServer's has404 flag which only 404s one hardcoded path belonging
// to a fixture this file does not use. Needed for the case where a repair
// is attempted and upstream can no longer supply the bytes.
func deadImageServer(t *testing.T, items []fedExercise) *httptest.Server {
	t.Helper()

	datasetJSON, err := json.Marshal(items)
	if err != nil {
		t.Fatalf("marshal fixture dataset: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/dataset.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(datasetJSON)
	})
	mux.HandleFunc("/images/", http.NotFound)

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// TestImport_ObjectMissingButRowPresent_RepairsAndKeepsImageIDs is the
// regression test for the defect: with the row still in Postgres and the
// object gone, a re-run must re-download and re-store the bytes rather than
// counting them as reused, and must do it under the existing image id so
// every revision referencing it stays valid.
func TestImport_ObjectMissingButRowPresent_RepairsAndKeepsImageIDs(t *testing.T) {
	st, pool := newTestStore(t)
	ctx := context.Background()

	items := []fedExercise{benchPressFixture()}
	server := fixtureServer(t, items, testGoldenJPEG(t), false)
	mediaStore := newCountingStore(t)

	first, err := Import(ctx, st, testOptions(server, mediaStore))
	if err != nil {
		t.Fatalf("first Import: %v", err)
	}
	if first.ImagesUploaded != 2 {
		t.Fatalf("first run: ImagesUploaded = %d, want 2", first.ImagesUploaded)
	}

	firstEx, err := st.Exercises.GetBySourceID(ctx, "Bench_Press")
	if err != nil {
		t.Fatalf("GetBySourceID after first run: %v", err)
	}
	imageIDsBefore := imageIDsForRevision(t, ctx, pool, *firstEx.CurrentRevisionID)
	if len(imageIDsBefore) != 2 {
		t.Fatalf("image_ids before = %d, want 2", len(imageIDsBefore))
	}

	// Storage loses everything; Postgres keeps every row.
	mediaStore.evictAll(t)
	mediaStore.puts = 0

	second, err := Import(ctx, st, testOptions(server, mediaStore))
	if err != nil {
		t.Fatalf("second Import: %v", err)
	}

	if second.ImagesRepaired != 2 {
		t.Errorf("ImagesRepaired = %d, want 2", second.ImagesRepaired)
	}
	if second.ImagesReused != 0 {
		t.Errorf("ImagesReused = %d, want 0 — a missing object is not a reuse", second.ImagesReused)
	}
	if second.ImagesUploaded != 0 {
		t.Errorf("ImagesUploaded = %d, want 0 — repair reuses the row, it does not insert a new one", second.ImagesUploaded)
	}
	// Two full images and the thumb rebuilt from each.
	if mediaStore.puts != 4 {
		t.Errorf("Put calls = %d, want 4 — the bytes (and their thumbs) must actually be rewritten", mediaStore.puts)
	}

	// The whole reason repair keeps the existing id: re-inserting would
	// change image_ids, which store.revisionUnchanged compares like any
	// other field, so every exercise in the dataset would gain a spurious
	// revision on the first run after a storage loss.
	if second.Updated != 0 {
		t.Errorf("Updated = %d, want 0 — repairing bytes is not a content change", second.Updated)
	}

	secondEx, err := st.Exercises.GetBySourceID(ctx, "Bench_Press")
	if err != nil {
		t.Fatalf("GetBySourceID after second run: %v", err)
	}
	if *secondEx.CurrentRevisionID != *firstEx.CurrentRevisionID {
		t.Errorf("current_revision_id changed: %s vs %s — repair must not create a revision",
			*secondEx.CurrentRevisionID, *firstEx.CurrentRevisionID)
	}

	imageIDsAfter := imageIDsForRevision(t, ctx, pool, *secondEx.CurrentRevisionID)
	if len(imageIDsAfter) != len(imageIDsBefore) {
		t.Fatalf("image_ids count changed: %d vs %d", len(imageIDsAfter), len(imageIDsBefore))
	}
	for i := range imageIDsBefore {
		if imageIDsAfter[i] != imageIDsBefore[i] {
			t.Errorf("image_ids[%d] changed: %s vs %s", i, imageIDsAfter[i], imageIDsBefore[i])
		}
	}

	var imageCount int
	countRow(t, ctx, pool, `SELECT count(*) FROM exercise_images`, &imageCount)
	if imageCount != 2 {
		t.Errorf("exercise_images row count = %d, want 2 — repair must not duplicate rows", imageCount)
	}

	// The point of the whole exercise: the bytes are fetchable again.
	for _, key := range []string{"exercises/Bench_Press/0.jpg", "exercises/Bench_Press/1.jpg"} {
		rc, err := mediaStore.Get(ctx, key)
		if err != nil {
			t.Errorf("Get(%s) after repair: %v", key, err)
			continue
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Errorf("read %s after repair: %v", key, err)
			continue
		}
		if len(data) == 0 {
			t.Errorf("%s is empty after repair", key)
		}
	}
}

// TestImport_ObjectPresent_IsReusedNotRepaired pins the ordinary path: when
// the object really is there, the existence check must not turn a reuse
// into a re-download. Without this, the fix above would "work" by making
// every run re-upload the entire dataset.
func TestImport_ObjectPresent_IsReusedNotRepaired(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	items := []fedExercise{benchPressFixture()}
	server := fixtureServer(t, items, testGoldenJPEG(t), false)
	mediaStore := newCountingStore(t)

	if _, err := Import(ctx, st, testOptions(server, mediaStore)); err != nil {
		t.Fatalf("first Import: %v", err)
	}
	mediaStore.puts = 0

	second, err := Import(ctx, st, testOptions(server, mediaStore))
	if err != nil {
		t.Fatalf("second Import: %v", err)
	}

	if second.ImagesReused != 2 {
		t.Errorf("ImagesReused = %d, want 2", second.ImagesReused)
	}
	if second.ImagesRepaired != 0 {
		t.Errorf("ImagesRepaired = %d, want 0 — nothing was missing", second.ImagesRepaired)
	}
	if mediaStore.puts != 0 {
		t.Errorf("Put calls = %d, want 0 — a present object must not be rewritten", mediaStore.puts)
	}
	if mediaStore.existsHit != 2 {
		t.Errorf("Exists calls = %d, want 2 — one per image, and no more", mediaStore.existsHit)
	}
}

// TestImport_ExistsFails_TreatsObjectAsPresent covers the deliberate
// asymmetry in Store.Exists' contract: an error is "could not answer", not
// "absent". Reading a failure as absence would re-upload every object in
// the bucket the first time a token lost its read scope or the network
// blipped — a far more expensive wrong answer than deferring the repair to
// the next run.
func TestImport_ExistsFails_TreatsObjectAsPresent(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	items := []fedExercise{benchPressFixture()}
	server := fixtureServer(t, items, testGoldenJPEG(t), false)
	mediaStore := newCountingStore(t)

	if _, err := Import(ctx, st, testOptions(server, mediaStore)); err != nil {
		t.Fatalf("first Import: %v", err)
	}

	// Objects are genuinely gone AND the store cannot say so. The gone-ness
	// must not be inferred from the failure.
	mediaStore.evictAll(t)
	mediaStore.puts = 0
	mediaStore.existsErr = errors.New("credentials rejected")

	second, err := Import(ctx, st, testOptions(server, mediaStore))
	if err != nil {
		t.Fatalf("second Import: %v", err)
	}

	if second.ImagesRepaired != 0 {
		t.Errorf("ImagesRepaired = %d, want 0 — an unanswerable check must not trigger a rewrite", second.ImagesRepaired)
	}
	if second.ImagesReused != 2 {
		t.Errorf("ImagesReused = %d, want 2", second.ImagesReused)
	}
	if mediaStore.puts != 0 {
		t.Errorf("Put calls = %d, want 0", mediaStore.puts)
	}
	if second.ImagesFailed != 0 {
		t.Errorf("ImagesFailed = %d, want 0 — this is a deferral, not a failure", second.ImagesFailed)
	}
}

// TestImport_RepairSourceGone_SkipsWithoutCorruptingRow covers the case
// where the object is missing and upstream can no longer supply it. The
// repair must fail cleanly: no partial write, no row mutation, and the
// image dropped from this revision's image_ids rather than left pointing at
// bytes that do not exist.
func TestImport_RepairSourceGone_SkipsWithoutCorruptingRow(t *testing.T) {
	st, pool := newTestStore(t)
	ctx := context.Background()

	items := []fedExercise{benchPressFixture()}
	mediaStore := newCountingStore(t)

	server := fixtureServer(t, items, testGoldenJPEG(t), false)
	if _, err := Import(ctx, st, testOptions(server, mediaStore)); err != nil {
		t.Fatalf("first Import: %v", err)
	}

	var contentTypeBefore string
	countRowString(t, ctx, pool,
		`SELECT content_type FROM exercise_images WHERE storage_key = 'exercises/Bench_Press/0.jpg'`,
		&contentTypeBefore)

	mediaStore.evictAll(t)

	// Same dataset, but every image URL now 404s.
	brokenServer := deadImageServer(t, items)
	second, err := Import(ctx, st, testOptions(brokenServer, mediaStore))
	if err != nil {
		t.Fatalf("second Import: %v", err)
	}

	if second.ImagesFailed != 2 {
		t.Errorf("ImagesFailed = %d, want 2", second.ImagesFailed)
	}
	if second.ImagesRepaired != 0 {
		t.Errorf("ImagesRepaired = %d, want 0 — nothing was actually restored", second.ImagesRepaired)
	}

	// The row must be untouched: a failed repair leaves the pre-existing
	// (accurate-for-the-lost-object) metadata rather than a half-updated one.
	var contentTypeAfter string
	countRowString(t, ctx, pool,
		`SELECT content_type FROM exercise_images WHERE storage_key = 'exercises/Bench_Press/0.jpg'`,
		&contentTypeAfter)
	if contentTypeAfter != contentTypeBefore {
		t.Errorf("content_type mutated by a failed repair: %q vs %q", contentTypeAfter, contentTypeBefore)
	}

	var imageCount int
	countRow(t, ctx, pool, `SELECT count(*) FROM exercise_images`, &imageCount)
	if imageCount != 2 {
		t.Errorf("exercise_images row count = %d, want 2 — a failed repair must not add or drop rows", imageCount)
	}
}
