package store

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Cawlumm/open-exercise-db/internal/database"
)

// This file mirrors internal/database/database_test.go's disposable-database
// pattern (docs/CONVENTIONS.md section 6: store tests run against real
// Postgres, never a mock). It's duplicated rather than imported because the
// database package's helpers are unexported; the shape is identical on
// purpose.

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

	dbName := fmt.Sprintf("oedb_store_test_%d", time.Now().UnixNano())
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
// wired against it, plus the raw pool for test-only fixture seeding (store
// methods in WO-2 only expose read paths, so tests insert fixtures directly).
func newTestStore(t *testing.T) (*Store, *pgxpool.Pool) {
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

	return New(pool), pool
}

// exerciseFixture is the minimal set of fields a test needs to control when
// seeding an exercise + its first revision; anything not set gets a sane
// default so each test only spells out what it's actually asserting on.
type exerciseFixture struct {
	slug         string
	name         string
	aliases      []string
	muscleGroup  string
	secondary    []string
	force        string
	equipment    string
	category     string
	mechanic     string
	level        string
	instructions []string
	status       string // exercise status; defaults to "published"
}

// seedExercise inserts an exercise and its first (and, at seed time,
// current) revision inside one transaction, mirroring the pattern
// TestMigrate_CircularForeignKeyIsDeferred exercises for the schema itself.
func seedExercise(t *testing.T, ctx context.Context, pool *pgxpool.Pool, f exerciseFixture) (exerciseID, revisionID string) {
	t.Helper()

	if f.status == "" {
		f.status = "published"
	}
	if f.aliases == nil {
		f.aliases = []string{}
	}
	if f.instructions == nil {
		f.instructions = []string{"Step one."}
	}
	if f.secondary == nil {
		f.secondary = []string{}
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed transaction: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	if err := tx.QueryRow(ctx,
		`INSERT INTO exercises (slug, status) VALUES ($1, $2) RETURNING id`,
		f.slug, f.status,
	).Scan(&exerciseID); err != nil {
		t.Fatalf("seed exercise %s: %v", f.slug, err)
	}

	var mechanic, level, force *string
	if f.mechanic != "" {
		mechanic = &f.mechanic
	}
	if f.level != "" {
		level = &f.level
	}
	if f.force != "" {
		force = &f.force
	}

	if err := tx.QueryRow(ctx,
		`INSERT INTO exercise_revisions
			(exercise_id, name, aliases, muscle_group, secondary_muscles, category, equipment, mechanic, force, level, instructions, status)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'approved')
		 RETURNING id`,
		exerciseID, f.name, f.aliases, f.muscleGroup, f.secondary, f.category, f.equipment, mechanic, force, level, f.instructions,
	).Scan(&revisionID); err != nil {
		t.Fatalf("seed revision for %s: %v", f.slug, err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE exercises SET current_revision_id = $1 WHERE id = $2`,
		revisionID, exerciseID,
	); err != nil {
		t.Fatalf("point exercise %s at its revision: %v", f.slug, err)
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed for %s: %v", f.slug, err)
	}

	return exerciseID, revisionID
}

// seedRevision inserts an additional revision for an already-seeded
// exercise, with an explicit createdAt so ordering assertions in
// ListRevisions tests don't depend on clock resolution between two fast
// successive inserts. It does not touch exercises.current_revision_id or
// supersede any prior revision — callers do that explicitly, the same way
// the real approval flow (WO-2's guardrails exclude write paths) will.
func seedRevision(t *testing.T, ctx context.Context, pool *pgxpool.Pool, exerciseID, name string, createdAt time.Time, status string) string {
	t.Helper()

	if status == "" {
		status = "approved"
	}

	var revisionID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO exercise_revisions
			(exercise_id, name, aliases, muscle_group, secondary_muscles, category, equipment, instructions, status, created_at)
		 VALUES ($1, $2, '{}', 'chest', '{}', 'strength', 'barbell', '["Step one."]', $3, $4)
		 RETURNING id`,
		exerciseID, name, status, createdAt,
	).Scan(&revisionID); err != nil {
		t.Fatalf("seed additional revision for exercise %s: %v", exerciseID, err)
	}

	return revisionID
}

// seedTranslation inserts an approved exercise_translations row. Pass an
// empty (non-nil) slice for aliases/instructions to exercise the
// partial-field fallback case deliberately, since the column is NOT NULL
// and a nil Go slice would encode as SQL NULL rather than '{}'/'[]'.
func seedTranslation(t *testing.T, ctx context.Context, pool *pgxpool.Pool, revisionID, locale, name string, aliases, instructions []string, origin string) {
	t.Helper()

	if origin == "" {
		origin = "human"
	}
	if aliases == nil {
		aliases = []string{}
	}
	if instructions == nil {
		instructions = []string{}
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO exercise_translations (revision_id, locale, name, aliases, instructions, origin, status)
		 VALUES ($1, $2, $3, $4, $5, $6, 'approved')`,
		revisionID, locale, name, aliases, instructions, origin,
	); err != nil {
		t.Fatalf("seed translation (%s, %s): %v", revisionID, locale, err)
	}
}

// seedTaxonomyTerm inserts a taxonomy_terms row plus one
// taxonomy_term_translations row per entry in labels (locale -> label).
func seedTaxonomyTerm(t *testing.T, ctx context.Context, pool *pgxpool.Pool, facet, slug string, labels map[string]string) string {
	t.Helper()

	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO taxonomy_terms (facet, slug) VALUES ($1, $2) RETURNING id`,
		facet, slug,
	).Scan(&id); err != nil {
		t.Fatalf("seed taxonomy term (%s, %s): %v", facet, slug, err)
	}

	for locale, label := range labels {
		if _, err := pool.Exec(ctx,
			`INSERT INTO taxonomy_term_translations (term_id, locale, label) VALUES ($1, $2, $3)`,
			id, locale, label,
		); err != nil {
			t.Fatalf("seed taxonomy term translation (%s, %s): %v", slug, locale, err)
		}
	}

	return id
}
