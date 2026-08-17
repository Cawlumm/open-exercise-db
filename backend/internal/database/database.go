package database

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

const migrationsDir = "migrations"

// maxConnsCeiling and maxConnsFloor bound the pool size to the managed provider's
// connection limit (docs/DESIGN.md -> Performance). A dsn that doesn't
// specify pool_max_conns, or specifies one outside this range, gets clamped
// rather than trusted, so a stray local .env value can't exhaust the free
// tier in production.
const (
	maxConnsCeiling = 10
	maxConnsFloor   = 5
)

// Connect opens a pgxpool.Pool for dsn, clamps its max connections into
// [maxConnsFloor, maxConnsCeiling], and verifies the connection with a ping
// before returning. Callers own the returned pool's lifecycle and must
// Close it.
func Connect(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database dsn: %w", err)
	}

	if cfg.MaxConns < maxConnsFloor || cfg.MaxConns > maxConnsCeiling {
		cfg.MaxConns = maxConnsCeiling
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return pool, nil
}

// migrationLockID namespaces this package's advisory lock. The value is
// arbitrary but must never change: it is the shared name every instance
// agrees to contend on, so a different value in a future release would let
// old and new instances migrate concurrently — precisely what the lock
// exists to prevent.
const migrationLockID = 0x0EDB_0001

// Migrate applies every embedded .sql file under migrations/ that hasn't
// already run, in lexical filename order, each inside its own transaction.
// Applied filenames are tracked in schema_migrations so a second call is a
// no-op — this is what makes it safe to run on every process start rather
// than as a separate release step.
//
// Safe to run from several processes at once, which is not the same as
// being a no-op on a second call. Deciding "has this run?" and running it
// are two statements, and on Cloud Run every cold start executes them: a
// scale-from-zero burst, a revision rollout, or min-instances > 1 all start
// containers simultaneously against one database. Two instances could both
// read "not applied" and both proceed, and since the migration SQL creates
// 17 objects with no IF NOT EXISTS, the loser gets "relation already
// exists", which main turns into log.Fatalf — an instance that crash-loops
// during a deploy, when the schema is least likely to be watched.
//
// A session-level advisory lock closes the window: the first instance holds
// it for the whole check-and-apply, the rest block, and by the time they
// proceed schema_migrations already says applied and they no-op. Advisory
// rather than a table lock because it costs nothing when uncontended (the
// overwhelmingly common case: one instance, nothing to apply) and needs no
// object to exist yet — including on the very first deploy, where
// schema_migrations itself is what is being created.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	// Taken on a dedicated connection, not the pool at large: a session-level
	// advisory lock belongs to the session that took it, and pool.Exec is
	// free to hand the unlock to a different connection, which would silently
	// fail to release and leave the lock held until that session closed.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection for migration lock: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, int64(migrationLockID)); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		// Best-effort by necessity: if this fails the connection is already
		// broken, and Release() closing it drops the session and the lock
		// with it. Logged rather than swallowed so it cannot be mistaken for
		// a clean release.
		if _, err := conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, int64(migrationLockID)); err != nil {
			log.Printf("release migration lock: %v (the lock drops when this session closes)", err)
		}
	}()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version     TEXT PRIMARY KEY,
			applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("create schema_migrations table: %w", err)
	}

	entries, err := fs.ReadDir(migrationFiles, migrationsDir)
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}

	var versions []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		versions = append(versions, entry.Name())
	}
	sort.Strings(versions)

	for _, version := range versions {
		applied, err := isApplied(ctx, pool, version)
		if err != nil {
			return fmt.Errorf("check migration %s: %w", version, err)
		}
		if applied {
			continue
		}

		if err := applyMigration(ctx, pool, version); err != nil {
			return fmt.Errorf("apply migration %s: %w", version, err)
		}
	}

	return nil
}

func isApplied(ctx context.Context, pool *pgxpool.Pool, version string) (bool, error) {
	var exists bool
	err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)`,
		version,
	).Scan(&exists)
	return exists, err
}

func applyMigration(ctx context.Context, pool *pgxpool.Pool, version string) error {
	sqlBytes, err := migrationFiles.ReadFile(migrationsDir + "/" + version)
	if err != nil {
		return fmt.Errorf("read migration file: %w", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	if _, err := tx.Exec(ctx, string(sqlBytes)); err != nil {
		return fmt.Errorf("execute migration sql: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version) VALUES ($1)`,
		version,
	); err != nil {
		return fmt.Errorf("record migration: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}
