package database

import (
	"context"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// baseDSN resolves the Postgres connection string tests run against. Per
// docs/CONVENTIONS.md section 6, store/database tests run against a real
// Postgres, never a mock. TEST_DATABASE_URL lets CI (WO-6) point at its
// service container; the fallback matches docker-compose.yml's local
// defaults (.env.example) so `docker compose up -d postgres` + `go test`
// works with zero extra setup.
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

// requirePostgres skips the test with a clear reason when no Postgres is
// reachable, instead of failing outright — a solo contributor running
// `go test ./...` without `docker compose up -d postgres` first should get a
// skip, not a red build. It does NOT fake or mock the database. See
// skipOrFail/requirePostgresEnv above for why this becomes a hard failure
// in CI instead.
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

// newTestDatabase creates a disposable, uniquely-named database on the same
// Postgres server as adminDSN and returns a DSN pointed at it, plus a
// cleanup func that drops it. This is what makes TestMigrate's "second run
// is a no-op" assertion trustworthy across repeated `go test` invocations —
// each run starts from a genuinely empty schema rather than one that may
// already carry a previous run's schema_migrations row.
func newTestDatabase(t *testing.T, adminDSN string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect to admin database: %v", err)
	}
	defer admin.Close(context.Background())

	dbName := fmt.Sprintf("oedb_test_%d", time.Now().UnixNano())
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

		// Terminate any lingering backends so DROP DATABASE doesn't fail
		// with "database is being accessed by other users" if a pool
		// connection hasn't been released yet.
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

func withDatabase(dsn, dbName string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse dsn: %w", err)
	}
	u.Path = "/" + dbName
	return u.String(), nil
}

func TestConnect(t *testing.T) {
	adminDSN := requirePostgres(t)

	tests := []struct {
		name    string
		dsn     string
		wantErr bool
	}{
		{
			name:    "valid dsn pings successfully",
			dsn:     adminDSN,
			wantErr: false,
		},
		{
			name:    "malformed dsn is rejected at parse time",
			dsn:     "://not-a-valid-dsn",
			wantErr: true,
		},
		{
			name:    "well-formed dsn pointed at a nonexistent database fails to ping",
			dsn:     "postgres://nobody:nobody@127.0.0.1:1/does-not-exist?connect_timeout=1",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			pool, err := Connect(ctx, tt.dsn)
			if pool != nil {
				defer pool.Close()
			}

			if tt.wantErr && err == nil {
				t.Fatalf("Connect(%q): expected error, got nil", tt.dsn)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Connect(%q): unexpected error: %v", tt.dsn, err)
			}
		})
	}
}

func TestConnect_ClampsMaxConns(t *testing.T) {
	adminDSN := requirePostgres(t)

	tests := []struct {
		name         string
		dsnSuffix    string // appended query params
		wantMaxConns int32
	}{
		{
			name:         "unset pool_max_conns is clamped up to the ceiling",
			dsnSuffix:    "",
			wantMaxConns: maxConnsCeiling,
		},
		{
			name:         "pool_max_conns below the floor is clamped up",
			dsnSuffix:    "&pool_max_conns=1",
			wantMaxConns: maxConnsCeiling,
		},
		{
			name:         "pool_max_conns above the ceiling is clamped down",
			dsnSuffix:    "&pool_max_conns=50",
			wantMaxConns: maxConnsCeiling,
		},
		{
			name:         "pool_max_conns within range is left alone",
			dsnSuffix:    "&pool_max_conns=7",
			wantMaxConns: 7,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			pool, err := Connect(ctx, adminDSN+tt.dsnSuffix)
			if err != nil {
				t.Fatalf("Connect: %v", err)
			}
			defer pool.Close()

			got := pool.Config().MaxConns
			if got != tt.wantMaxConns {
				t.Fatalf("MaxConns = %d, want %d", got, tt.wantMaxConns)
			}
		})
	}
}

// TestMigrate_IsIdempotent is the acceptance criterion from
// docs/PHASE1_BUILD_PLAN.md WO-1: running the migrator twice against the
// same database must be a no-op the second time.
func TestMigrate_IsIdempotent(t *testing.T) {
	adminDSN := requirePostgres(t)
	testDSN := newTestDatabase(t, adminDSN)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := Connect(ctx, testDSN)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer pool.Close()

	wantTables := []string{
		"schema_migrations",
		"taxonomy_terms",
		"taxonomy_term_translations",
		"users",
		"exercise_images",
		"exercise_videos",
		"exercises",
		"exercise_revisions",
		"exercise_translations",
		"submissions",
	}

	t.Run("first run applies the schema", func(t *testing.T) {
		if err := Migrate(ctx, pool); err != nil {
			t.Fatalf("Migrate (first run): %v", err)
		}

		for _, table := range wantTables {
			if !tableExists(t, ctx, pool, table) {
				t.Errorf("table %q does not exist after first Migrate", table)
			}
		}

		version := "0001_initial_schema.sql"
		if !migrationRecorded(t, ctx, pool, version) {
			t.Errorf("schema_migrations has no row for %q after first Migrate", version)
		}

		// Compared against the number of embedded migration files rather
		// than a literal. The literal was 1, correct when there was one
		// migration, and it turned the next migration ever added into a
		// failing test that said nothing about migrations being wrong —
		// which is exactly what 0002 did. What this actually cares about is
		// "one row per file", and that stays true however many files exist.
		if got, want := migrationRowCount(t, ctx, pool), embeddedMigrationCount(t); got != want {
			t.Errorf("schema_migrations row count = %d, want %d (one per embedded migration)", got, want)
		}
	})

	t.Run("second run is a no-op", func(t *testing.T) {
		if err := Migrate(ctx, pool); err != nil {
			t.Fatalf("Migrate (second run): %v", err)
		}

		if got, want := migrationRowCount(t, ctx, pool), embeddedMigrationCount(t); got != want {
			t.Errorf("schema_migrations row count after second Migrate = %d, want %d (re-applied instead of skipped)", got, want)
		}

		for _, table := range wantTables {
			if !tableExists(t, ctx, pool, table) {
				t.Errorf("table %q missing after second Migrate", table)
			}
		}
	})
}

// TestMigrate_CircularForeignKeyIsDeferred exercises the one genuinely
// tricky piece of the schema: exercises.current_revision_id and
// exercise_revisions.exercise_id reference each other, so inserting both
// rows only works within a single transaction if the FK is deferrable. This
// would fail with a foreign-key violation on the INSERT (not COMMIT) if the
// migration ever regressed to an immediate constraint.
func TestMigrate_CircularForeignKeyIsDeferred(t *testing.T) {
	adminDSN := requirePostgres(t)
	testDSN := newTestDatabase(t, adminDSN)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := Connect(ctx, testDSN)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer pool.Close()

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var exerciseID, revisionID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO exercises (slug, status) VALUES ('bench-press', 'published') RETURNING id`,
	).Scan(&exerciseID); err != nil {
		t.Fatalf("insert exercise: %v", err)
	}

	if err := tx.QueryRow(ctx,
		`INSERT INTO exercise_revisions (exercise_id, name, muscle_group, category, equipment, status)
		 VALUES ($1, 'Bench Press', 'chest', 'strength', 'barbell', 'approved') RETURNING id`,
		exerciseID,
	).Scan(&revisionID); err != nil {
		t.Fatalf("insert exercise_revision: %v", err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE exercises SET current_revision_id = $1 WHERE id = $2`,
		revisionID, exerciseID,
	); err != nil {
		t.Fatalf("point exercise at its revision: %v", err)
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit transaction (deferred FK should not block this): %v", err)
	}
}

func tableExists(t *testing.T, ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, table string) bool {
	t.Helper()

	var exists bool
	err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1)`,
		table,
	).Scan(&exists)
	if err != nil {
		t.Fatalf("check table %q exists: %v", table, err)
	}
	return exists
}

func migrationRecorded(t *testing.T, ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, version string) bool {
	t.Helper()

	var exists bool
	err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`,
		version,
	).Scan(&exists)
	if err != nil {
		t.Fatalf("check schema_migrations for %q: %v", version, err)
	}
	return exists
}

func migrationRowCount(t *testing.T, ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) int {
	t.Helper()

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("count schema_migrations rows: %v", err)
	}
	return count
}

// embeddedMigrationCount reports how many .sql files Migrate will apply,
// read from the same embed.FS Migrate itself walks.
//
// Derived rather than written down so adding a migration cannot break a
// test about idempotency. A hardcoded total says "one migration exists",
// which is a fact about the repo on the day it was written, not about the
// behavior under test — the assertion that matters is one row per file.
func embeddedMigrationCount(t *testing.T) int {
	t.Helper()

	entries, err := fs.ReadDir(migrationFiles, migrationsDir)
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}

	count := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			count++
		}
	}
	if count == 0 {
		t.Fatal("no embedded migrations found — the count would trivially match an empty schema_migrations")
	}
	return count
}
