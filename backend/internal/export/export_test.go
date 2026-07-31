package export

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Cawlumm/open-exercise-db/internal/database"
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// This file mirrors internal/store/store_test.go's disposable-database
// pattern (docs/CONVENTIONS.md section 6: real Postgres, never a mock).
// Duplicated rather than imported because the source helpers are unexported.

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

func newTestDatabase(t *testing.T, adminDSN string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect to admin database: %v", err)
	}
	defer admin.Close(context.Background())

	dbName := fmt.Sprintf("oedb_export_test_%d", time.Now().UnixNano())
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
	return testDSN
}

// newTestStore spins up a disposable, migrated database and returns a Store
// wired against it. Unlike internal/store's and internal/api's own
// newTestStore, this package never needs the raw pool: every fixture here is
// seeded through store.Exercises.UpsertImport (WO-4's real write path),
// which is enough to populate every field export.Exercise surfaces,
// including the license/attribution/source fields WO-2's read-only fixtures
// don't cover.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()

	adminDSN := requirePostgres(t)
	testDSN := newTestDatabase(t, adminDSN)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := database.Connect(ctx, testDSN)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}

	return store.New(pool)
}

// seedFixture imports one exercise via the real UpsertImport write path and
// returns its exercise id.
func seedFixture(t *testing.T, ctx context.Context, s *store.Store, sourceID, slug, name string) string {
	t.Helper()

	license := "Unlicense"
	result, err := s.Exercises.UpsertImport(ctx, store.UpsertImportInput{
		SourceID: sourceID,
		Slug:     slug,
		Revision: store.ImportRevisionInput{
			Name:             name,
			Aliases:          []string{},
			MuscleGroup:      "chest",
			SecondaryMuscles: []string{},
			Category:         "strength",
			Equipment:        "barbell",
			Instructions:     []string{"Step one."},
			ImageIDs:         []string{},
			License:          &license,
		},
	})
	if err != nil {
		t.Fatalf("seed exercise %s: %v", slug, err)
	}
	return result.ExerciseID
}

func TestCache_Get_CachesUntilInvalidated(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedFixture(t, ctx, s, "Bench_Press", "bench-press", "Bench Press")

	c := NewCache(DefaultCacheTTL, "")
	first, err := c.Get(ctx, s)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if first.Body.Total != 1 {
		t.Fatalf("Total = %d, want 1", first.Body.Total)
	}

	// A second exercise lands in the DB, but without invalidating the cache
	// Get should still serve the first snapshot — this is WO-5's "does not
	// re-query" acceptance criterion: if Get queried the store on every
	// call, Total would already be 2 here.
	seedFixture(t, ctx, s, "Squat", "squat", "Squat")
	second, err := c.Get(ctx, s)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if second.Body.Total != 1 || second.ETag != first.ETag {
		t.Fatalf("Get after an uninvalidated write = Total %d ETag %s, want 1/%s (cache should still serve the stale snapshot)",
			second.Body.Total, second.ETag, first.ETag)
	}
	if second != first {
		t.Error("Get returned a different *Blob without a rebuild — it did work it shouldn't have")
	}

	c.Invalidate()
	third, err := c.Get(ctx, s)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if third.Body.Total != 2 {
		t.Fatalf("Total after Invalidate = %d, want 2", third.Body.Total)
	}
	if third.ETag == first.ETag {
		t.Error("ETag unchanged after Invalidate() + new data — expected a rebuild to detect the new exercise")
	}
}

// TestCache_Get_ETagStableAcrossRebuildOfUnchangedData guards the
// deliberate design choice on etagPayload (export.go): Body.GeneratedAt must
// not feed the ETag, or every TTL-triggered rebuild of otherwise-identical
// data would still churn the ETag and force every mirror to re-download the
// full catalog for nothing.
func TestCache_Get_ETagStableAcrossRebuildOfUnchangedData(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedFixture(t, ctx, s, "Bench_Press", "bench-press", "Bench Press")

	c := NewCache(DefaultCacheTTL, "")
	first, err := c.Get(ctx, s)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	c.Invalidate()
	time.Sleep(2 * time.Millisecond) // force a distinguishable BuiltAt/GeneratedAt
	second, err := c.Get(ctx, s)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if second.BuiltAt.Equal(first.BuiltAt) {
		t.Fatal("test didn't actually force a rebuild — BuiltAt unchanged, so this isn't testing anything")
	}
	if second.ETag != first.ETag {
		t.Errorf("ETag changed on a rebuild of unchanged data (%s -> %s); GeneratedAt must be excluded from the ETag computation", first.ETag, second.ETag)
	}
}

func TestCache_Get_ZeroTTLNeverExpires(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedFixture(t, ctx, s, "Bench_Press", "bench-press", "Bench Press")

	c := NewCache(0, "")
	first, err := c.Get(ctx, s)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	second, err := c.Get(ctx, s)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if second != first {
		t.Error("ttl<=0 should mean \"never expire once built\" (NewCache's doc comment), but Get rebuilt anyway")
	}
}

// Archived-exercise exclusion is covered in internal/api/export_test.go
// (TestExportExercises_ExcludesArchivedExercises) rather than here, since it
// needs a status='archived' fixture and this package's newTestStore
// deliberately doesn't expose the raw pool (see its doc comment) — every
// fixture here goes through the real UpsertImport write path instead, which
// has no archive step of its own.
func TestBuildBlob_ResolvesEnglishDefault(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id := seedFixture(t, ctx, s, "Bench_Press", "bench-press", "Bench Press")

	blob, err := buildBlob(ctx, s, "")
	if err != nil {
		t.Fatalf("buildBlob: %v", err)
	}
	if blob.Body.Total != 1 || len(blob.Body.Exercises) != 1 {
		t.Fatalf("Total/len = %d/%d, want 1/1", blob.Body.Total, len(blob.Body.Exercises))
	}

	ex := blob.Body.Exercises[0]
	if ex.ID != id {
		t.Errorf("ID = %q, want %q", ex.ID, id)
	}
	if ex.Translation.Name != "Bench Press" || ex.Translation.RequestedLocale != "en" {
		t.Errorf("Translation = %+v, want RequestedLocale=en Name=\"Bench Press\" (export takes no ?locale=)", ex.Translation)
	}
	if ex.SourceID == nil || *ex.SourceID != "Bench_Press" {
		t.Errorf("SourceID = %v, want Bench_Press", ex.SourceID)
	}
	if ex.License == nil || *ex.License != "Unlicense" {
		t.Errorf("License = %v, want Unlicense", ex.License)
	}
	if len(ex.Translation.Instructions) != 1 || ex.Translation.Instructions[0] != "Step one." {
		t.Errorf("Instructions = %v, want [\"Step one.\"]", ex.Translation.Instructions)
	}
}
