package api

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
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// This file mirrors internal/store/store_test.go's disposable-database
// pattern (docs/CONVENTIONS.md section 6: real Postgres, never a mock).
// Duplicated rather than imported for the same reason store_test.go gives:
// the helpers it would reuse are unexported.

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

	dbName := fmt.Sprintf("oedb_api_test_%d", time.Now().UnixNano())
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
// wired against it, plus the raw pool for fixture seeding — handler tests
// exercise real store methods against real data, per
// docs/PHASE1_BUILD_PLAN.md WO-3's "do not mock the store" requirement.
func newTestStore(t *testing.T) (*store.Store, *pgxpool.Pool) {
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

	return store.New(pool), pool
}

// exerciseFixture and seedExercise/seedTranslation mirror
// internal/store/store_test.go's fixtures — WO-3's handler tests need the
// same seeding, but internal/store's helpers are unexported and this
// package can't import a _test.go file, so the minimal subset used here is
// duplicated rather than shared.
type exerciseFixture struct {
	slug         string
	name         string
	aliases      []string
	muscleGroup  string
	equipment    string
	category     string
	mechanic     string
	level        string
	instructions []string
	status       string
}

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

	var mechanic, level *string
	if f.mechanic != "" {
		mechanic = &f.mechanic
	}
	if f.level != "" {
		level = &f.level
	}

	if err := tx.QueryRow(ctx,
		`INSERT INTO exercise_revisions
			(exercise_id, name, aliases, muscle_group, secondary_muscles, category, equipment, mechanic, level, instructions, status)
		 VALUES ($1, $2, $3, $4, '{}', $5, $6, $7, $8, $9, 'approved')
		 RETURNING id`,
		exerciseID, f.name, f.aliases, f.muscleGroup, f.category, f.equipment, mechanic, level, f.instructions,
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
