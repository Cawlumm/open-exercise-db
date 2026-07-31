package seed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Cawlumm/open-exercise-db/internal/database"
	"github.com/Cawlumm/open-exercise-db/internal/media"
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// This file mirrors internal/store/store_test.go's disposable-database
// pattern (docs/CONVENTIONS.md section 6: tests run against real Postgres,
// never a mock). Duplicated rather than imported because the store
// package's helpers are unexported — same reasoning store_test.go itself
// gives for duplicating internal/database's version.

func baseDSN() string {
	if v := os.Getenv("TEST_DATABASE_URL"); v != "" {
		return v
	}
	if v := os.Getenv("DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://openexercisedb:openexercisedb@localhost:5432/openexercisedb?sslmode=disable"
}

// requirePostgresEnv, when set to any non-empty value, turns a missing
// Postgres from a quiet skip into a hard test failure. Left unset (the
// default for a local contributor), `go test ./...` degrades gracefully
// without Docker. CI sets it (.github/workflows/ci.yml) specifically so this
// package can't report green by skipping every real query if the service
// container ever fails to come up — see docs/CONVENTIONS.md section 6.
const requirePostgresEnv = "OEDB_REQUIRE_POSTGRES"

// skipOrFail is requirePostgres's escape hatch: t.Skipf everywhere except
// when requirePostgresEnv says the caller (CI) requires a real database, in
// which case the same condition is a t.Fatalf instead.
func skipOrFail(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv(requirePostgresEnv) != "" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

func requirePostgres(t *testing.T) string {
	t.Helper()

	dsn := baseDSN()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		skipOrFail(t, "postgres not reachable at %s (start it with `docker compose up -d postgres`): %v", dsn, err)
		return ""
	}
	defer conn.Close(context.Background())

	if err := conn.Ping(ctx); err != nil {
		skipOrFail(t, "postgres not reachable at %s (start it with `docker compose up -d postgres`): %v", dsn, err)
		return ""
	}

	return dsn
}

func withDatabase(dsn, dbName string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse dsn: %w", err)
	}
	u.Path = "/" + dbName
	return u.String(), nil
}

// newTestStore spins up a disposable, migrated database and returns a Store
// wired against it, plus the raw pool for test-only row-count assertions
// (mirroring internal/store/store_test.go's newTestStore).
func newTestStore(t *testing.T) (*store.Store, *pgxpool.Pool) {
	t.Helper()

	adminDSN := requirePostgres(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect to admin database: %v", err)
	}
	defer admin.Close(context.Background())

	dbName := fmt.Sprintf("oedb_seed_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{dbName}.Sanitize()); err != nil {
		t.Fatalf("create test database %s: %v", dbName, err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()

		admin, err := pgx.Connect(cleanupCtx, adminDSN)
		if err != nil {
			t.Logf("cleanup: connect to admin database: %v", err)
			return
		}
		defer admin.Close(context.Background())

		if _, err := admin.Exec(cleanupCtx,
			`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`,
			dbName,
		); err != nil {
			t.Logf("cleanup: terminate backends for %s: %v", dbName, err)
		}
		if _, err := admin.Exec(cleanupCtx, "DROP DATABASE IF EXISTS "+pgx.Identifier{dbName}.Sanitize()); err != nil {
			t.Logf("cleanup: drop database %s: %v", dbName, err)
		}
	})

	testDSN, err := withDatabase(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build test dsn: %v", err)
	}

	pool, err := database.Connect(ctx, testDSN)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}

	return store.New(pool), pool
}

func strPtr(s string) *string { return &s }

// testGoldenJPEG is a minimal valid JPEG served by the fixture image
// server, generated once per test run rather than hand-embedded as a
// binary fixture (docs/PHASE1_BUILD_PLAN.md WO-4: hermetic tests, no real
// network access).
func testGoldenJPEG(t *testing.T) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, 40, 30))
	for y := 0; y < 30; y++ {
		for x := 0; x < 40; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 5), G: uint8(y * 5), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode test jpeg: %v", err)
	}
	return buf.Bytes()
}

// fixtureServer starts an httptest server that serves datasetJSON at "/" and
// each of images at "/<path>" (200 with a real JPEG body); any other path
// serves 404, which is what backs the "image URL 404s" test case.
func fixtureServer(t *testing.T, items []fedExercise, jpegBody []byte, has404 bool) *httptest.Server {
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
	mux.HandleFunc("/images/", func(w http.ResponseWriter, r *http.Request) {
		if has404 && r.URL.Path == "/images/Missing_Exercise/0.jpg" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(jpegBody)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func testOptions(server *httptest.Server, mediaStore media.Store) Options {
	var logger *log.Logger
	if testing.Verbose() {
		logger = log.Default()
	} else {
		logger = log.New(io.Discard, "", 0)
	}
	return Options{
		SourceURL:    server.URL + "/dataset.json",
		ImageBaseURL: server.URL + "/images/",
		HTTPClient:   server.Client(),
		MediaStore:   mediaStore,
		Logger:       logger,
	}
}

func newTestMediaStore(t *testing.T) media.Store {
	t.Helper()
	fs, err := media.NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilesystemStore: %v", err)
	}
	return fs
}

func benchPressFixture() fedExercise {
	return fedExercise{
		ID:               "Bench_Press",
		Name:             "Bench Press",
		Force:            strPtr("push"),
		Level:            "beginner",
		Mechanic:         strPtr("compound"),
		Equipment:        strPtr("barbell"),
		PrimaryMuscles:   []string{"chest"},
		SecondaryMuscles: []string{"triceps", "shoulders"},
		Instructions:     []string{"Lie on the bench.", "Press the bar up."},
		Category:         "strength",
		Images:           []string{"Bench_Press/0.jpg", "Bench_Press/1.jpg"},
	}
}

func TestImport_EmptyDatabase_CreatesExercisesAndRevisions(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	server := fixtureServer(t, []fedExercise{benchPressFixture()}, testGoldenJPEG(t), false)
	opts := testOptions(server, newTestMediaStore(t))

	result, err := Import(ctx, st, opts)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	if result.Fetched != 1 {
		t.Errorf("Fetched = %d, want 1", result.Fetched)
	}
	if result.Created != 1 {
		t.Errorf("Created = %d, want 1", result.Created)
	}
	if result.Updated != 0 || result.Unchanged != 0 {
		t.Errorf("Updated/Unchanged = %d/%d, want 0/0", result.Updated, result.Unchanged)
	}
	if result.ImagesUploaded != 2 {
		t.Errorf("ImagesUploaded = %d, want 2", result.ImagesUploaded)
	}

	ex, err := st.Exercises.GetBySourceID(ctx, "Bench_Press")
	if err != nil {
		t.Fatalf("GetBySourceID: %v", err)
	}
	if ex.Slug != "bench-press" {
		t.Errorf("Slug = %q, want bench-press", ex.Slug)
	}
	if ex.CurrentRevisionID == nil {
		t.Fatal("CurrentRevisionID is nil, want set")
	}
	if ex.CurrentRevision.Status != "approved" {
		t.Errorf("revision status = %q, want approved", ex.CurrentRevision.Status)
	}
	license := "Unlicense"
	if ex.CurrentRevision.License == nil || *ex.CurrentRevision.License != license {
		t.Errorf("license = %v, want %s", ex.CurrentRevision.License, license)
	}
	if ex.CurrentRevision.SourceURL == nil || *ex.CurrentRevision.SourceURL != attributionSourceURL {
		t.Errorf("source_url = %v, want %s", ex.CurrentRevision.SourceURL, attributionSourceURL)
	}
	if len(ex.CurrentRevision.ImageIDs) != 2 {
		t.Errorf("image_ids = %v, want 2 entries", ex.CurrentRevision.ImageIDs)
	}
	if ex.CurrentRevision.MuscleGroup != "chest" {
		t.Errorf("muscle_group = %q, want chest", ex.CurrentRevision.MuscleGroup)
	}
	wantSecondary := []string{"triceps", "shoulders"}
	if len(ex.CurrentRevision.SecondaryMuscles) != len(wantSecondary) {
		t.Errorf("secondary_muscles = %v, want %v", ex.CurrentRevision.SecondaryMuscles, wantSecondary)
	}

	// Instructions is a JSONB array of strings, not a concatenated string —
	// docs/PHASE1_BUILD_PLAN.md WO-4's deliberate difference from lyftr.
	if len(ex.CurrentRevision.Instructions) != 2 {
		t.Errorf("instructions = %v, want 2 separate steps", ex.CurrentRevision.Instructions)
	}

	terms, err := st.Taxonomy.ListTerms(ctx, "muscle_group", "en")
	if err != nil {
		t.Fatalf("ListTerms: %v", err)
	}
	if len(terms) == 0 {
		t.Error("expected muscle_group taxonomy terms to be populated")
	}
}

// TestImport_RunTwice_YieldsIdenticalCountsAndNoNewRevisions is the single
// most important acceptance criterion for WO-4 (docs/PHASE1_BUILD_PLAN.md):
// re-running the importer against unchanged upstream data must not
// duplicate rows or create new revisions.
func TestImport_RunTwice_YieldsIdenticalCountsAndNoNewRevisions(t *testing.T) {
	st, pool := newTestStore(t)
	ctx := context.Background()

	items := []fedExercise{benchPressFixture()}
	server := fixtureServer(t, items, testGoldenJPEG(t), false)
	mediaStore := newTestMediaStore(t)

	first, err := Import(ctx, st, testOptions(server, mediaStore))
	if err != nil {
		t.Fatalf("first Import: %v", err)
	}
	if first.Created != 1 {
		t.Fatalf("first run: Created = %d, want 1", first.Created)
	}

	firstEx, err := st.Exercises.GetBySourceID(ctx, "Bench_Press")
	if err != nil {
		t.Fatalf("GetBySourceID after first run: %v", err)
	}

	second, err := Import(ctx, st, testOptions(server, mediaStore))
	if err != nil {
		t.Fatalf("second Import: %v", err)
	}

	if second.Created != 0 {
		t.Errorf("second run: Created = %d, want 0", second.Created)
	}
	if second.Updated != 0 {
		t.Errorf("second run: Updated = %d, want 0 (nothing changed)", second.Updated)
	}
	if second.Unchanged != 1 {
		t.Errorf("second run: Unchanged = %d, want 1", second.Unchanged)
	}
	// Images are recognized via their deterministic storage key and reused,
	// never re-uploaded — this is what keeps image_ids (and therefore the
	// whole revision) stable across runs.
	if second.ImagesUploaded != 0 {
		t.Errorf("second run: ImagesUploaded = %d, want 0", second.ImagesUploaded)
	}
	if second.ImagesReused != 2 {
		t.Errorf("second run: ImagesReused = %d, want 2", second.ImagesReused)
	}

	secondEx, err := st.Exercises.GetBySourceID(ctx, "Bench_Press")
	if err != nil {
		t.Fatalf("GetBySourceID after second run: %v", err)
	}
	if secondEx.ID != firstEx.ID {
		t.Errorf("exercise id changed across runs: %s vs %s", secondEx.ID, firstEx.ID)
	}
	if *secondEx.CurrentRevisionID != *firstEx.CurrentRevisionID {
		t.Errorf("current_revision_id changed across runs: %s vs %s (no new revision expected)",
			*secondEx.CurrentRevisionID, *firstEx.CurrentRevisionID)
	}

	var exerciseCount, revisionCount int
	countRow(t, ctx, pool, `SELECT count(*) FROM exercises`, &exerciseCount)
	countRow(t, ctx, pool, `SELECT count(*) FROM exercise_revisions`, &revisionCount)
	if exerciseCount != 1 {
		t.Errorf("exercises row count = %d, want 1", exerciseCount)
	}
	if revisionCount != 1 {
		t.Errorf("exercise_revisions row count = %d, want 1 (no new revision)", revisionCount)
	}
}

// TestImport_ChangedUpstreamRecord_CreatesNewRevisionAndRepoints is the
// flip side: a genuine upstream content change must still produce a new
// revision, not be swallowed by the idempotency check.
func TestImport_ChangedUpstreamRecord_CreatesNewRevisionAndRepoints(t *testing.T) {
	st, pool := newTestStore(t)
	ctx := context.Background()

	first := benchPressFixture()
	server1 := fixtureServer(t, []fedExercise{first}, testGoldenJPEG(t), false)
	mediaStore := newTestMediaStore(t)

	if _, err := Import(ctx, st, testOptions(server1, mediaStore)); err != nil {
		t.Fatalf("first Import: %v", err)
	}
	before, err := st.Exercises.GetBySourceID(ctx, "Bench_Press")
	if err != nil {
		t.Fatalf("GetBySourceID: %v", err)
	}

	changed := benchPressFixture()
	changed.Instructions = append(changed.Instructions, "Lower with control.")
	server2 := fixtureServer(t, []fedExercise{changed}, testGoldenJPEG(t), false)

	result, err := Import(ctx, st, testOptions(server2, mediaStore))
	if err != nil {
		t.Fatalf("second Import: %v", err)
	}
	if result.Updated != 1 {
		t.Errorf("Updated = %d, want 1", result.Updated)
	}
	if result.Created != 0 {
		t.Errorf("Created = %d, want 0", result.Created)
	}

	after, err := st.Exercises.GetBySourceID(ctx, "Bench_Press")
	if err != nil {
		t.Fatalf("GetBySourceID: %v", err)
	}
	if *after.CurrentRevisionID == *before.CurrentRevisionID {
		t.Error("current_revision_id did not change after an actual content change")
	}
	if len(after.CurrentRevision.Instructions) != 3 {
		t.Errorf("instructions = %v, want 3 steps", after.CurrentRevision.Instructions)
	}

	var revisionCount int
	countRow(t, ctx, pool, `SELECT count(*) FROM exercise_revisions WHERE exercise_id = $1`, &revisionCount, before.ID)
	if revisionCount != 2 {
		t.Errorf("revision count = %d, want 2", revisionCount)
	}
}

func TestImport_DryRun_WritesNothing(t *testing.T) {
	st, pool := newTestStore(t)
	ctx := context.Background()

	server := fixtureServer(t, []fedExercise{benchPressFixture()}, testGoldenJPEG(t), false)
	opts := testOptions(server, nil) // no media store needed: dry run must never touch it
	opts.DryRun = true

	result, err := Import(ctx, st, opts)
	if err != nil {
		t.Fatalf("Import (dry run): %v", err)
	}
	if result.Created != 1 {
		t.Errorf("dry-run preview Created = %d, want 1 (reports what WOULD happen)", result.Created)
	}
	if result.ImagesUploaded != 0 || result.ImagesReused != 0 {
		t.Errorf("dry run touched images: uploaded=%d reused=%d, want 0/0", result.ImagesUploaded, result.ImagesReused)
	}

	var exerciseCount, taxonomyCount int
	countRow(t, ctx, pool, `SELECT count(*) FROM exercises`, &exerciseCount)
	countRow(t, ctx, pool, `SELECT count(*) FROM taxonomy_terms`, &taxonomyCount)
	if exerciseCount != 0 {
		t.Errorf("exercises row count after dry run = %d, want 0", exerciseCount)
	}
	if taxonomyCount != 0 {
		t.Errorf("taxonomy_terms row count after dry run = %d, want 0", taxonomyCount)
	}
}

// TestImport_SlugCollision_DedupesWithNumericSuffix covers
// docs/PHASE1_BUILD_PLAN.md WO-4's "slug generated from name, deduped with
// a numeric suffix on collision" requirement, using two distinct upstream
// items whose names slugify to the same base.
func TestImport_SlugCollision_DedupesWithNumericSuffix(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	first := fedExercise{
		ID: "Push_Up", Name: "Push Up", Level: "beginner",
		PrimaryMuscles: []string{"chest"}, Category: "strength",
		Equipment: strPtr("body only"), Instructions: []string{"Step one."},
	}
	// A different upstream item whose name slugifies to the exact same
	// base ("push-up") — not a real free-exercise-db collision (verified
	// there are none in the live dataset), but the code path must still
	// handle it correctly since a future upstream edit or another source
	// could introduce one.
	second := fedExercise{
		ID: "Push-Up_Variant", Name: "Push-Up", Level: "intermediate",
		PrimaryMuscles: []string{"chest"}, Category: "strength",
		Equipment: strPtr("body only"), Instructions: []string{"Step one, variant."},
	}

	server := fixtureServer(t, []fedExercise{first, second}, testGoldenJPEG(t), false)
	result, err := Import(ctx, st, testOptions(server, newTestMediaStore(t)))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if result.Created != 2 {
		t.Fatalf("Created = %d, want 2", result.Created)
	}

	a, err := st.Exercises.GetBySourceID(ctx, "Push_Up")
	if err != nil {
		t.Fatalf("GetBySourceID(Push_Up): %v", err)
	}
	b, err := st.Exercises.GetBySourceID(ctx, "Push-Up_Variant")
	if err != nil {
		t.Fatalf("GetBySourceID(Push-Up_Variant): %v", err)
	}

	if a.Slug == b.Slug {
		t.Fatalf("both items got the same slug %q, want a deduped suffix", a.Slug)
	}
	slugs := map[string]bool{a.Slug: true, b.Slug: true}
	if !slugs["push-up"] || !slugs["push-up-2"] {
		t.Errorf("slugs = %q, %q, want push-up and push-up-2", a.Slug, b.Slug)
	}
}

// TestImport_ImageNotFound_SkipsImageButImportsExercise covers
// docs/PHASE1_BUILD_PLAN.md WO-4's fault-tolerance requirement: one bad
// image asset must not abort the run.
func TestImport_ImageNotFound_SkipsImageButImportsExercise(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	item := fedExercise{
		ID: "Missing_Exercise", Name: "Missing Image Exercise", Level: "beginner",
		PrimaryMuscles: []string{"chest"}, Category: "strength",
		Equipment:    strPtr("body only"),
		Instructions: []string{"Step one."},
		Images:       []string{"Missing_Exercise/0.jpg"}, // fixtureServer 404s this exact path
	}

	server := fixtureServer(t, []fedExercise{item}, testGoldenJPEG(t), true)
	result, err := Import(ctx, st, testOptions(server, newTestMediaStore(t)))
	if err != nil {
		t.Fatalf("Import: expected no fatal error from a 404'd image, got: %v", err)
	}
	if result.Created != 1 {
		t.Errorf("Created = %d, want 1 (the exercise itself must still import)", result.Created)
	}
	if result.ImagesFailed != 1 {
		t.Errorf("ImagesFailed = %d, want 1", result.ImagesFailed)
	}

	ex, err := st.Exercises.GetBySourceID(ctx, "Missing_Exercise")
	if err != nil {
		t.Fatalf("GetBySourceID: %v", err)
	}
	if len(ex.CurrentRevision.ImageIDs) != 0 {
		t.Errorf("image_ids = %v, want empty (the only image failed)", ex.CurrentRevision.ImageIDs)
	}
}

// countRow is a tiny helper so tests can assert on real row counts without
// each hand-rolling a QueryRow call. It queries the raw pool directly
// (not through *store.Store, which has no generic query method by design —
// docs/CONVENTIONS.md section 4: all SQL lives in typed store methods) since
// this is test-only verification, the same pattern
// internal/store/store_test.go's own fixture helpers use.
func countRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, dest *int, args ...any) {
	t.Helper()
	if err := pool.QueryRow(ctx, sql, args...).Scan(dest); err != nil {
		t.Fatalf("count query %q: %v", sql, err)
	}
}
