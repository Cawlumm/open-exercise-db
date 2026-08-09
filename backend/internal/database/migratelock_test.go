package database

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMigrate_ConcurrentCallersAllSucceed covers what "safe to run on every
// process start" has to mean on Cloud Run, where a scale-from-zero burst, a
// revision rollout, or min-instances > 1 all start containers at the same
// moment against one database.
//
// Idempotency alone does not give this. Deciding "has this run?" and running
// it are separate statements, so two instances can both read "not applied"
// and both proceed. The migration SQL creates 17 objects with no
// IF NOT EXISTS, so the loser of that race gets "relation already exists" —
// which cmd/api-server turns into log.Fatalf, i.e. an instance crash-looping
// through a deploy.
//
// Separate pools rather than goroutines on one pool: instances are separate
// processes, and a session-level advisory lock is only interesting when the
// contenders are distinct sessions. Sharing a pool could let two callers
// land on the same connection and "pass" by re-entering a lock they already
// held, which is the one arrangement that proves nothing.
func TestMigrate_ConcurrentCallersAllSucceed(t *testing.T) {
	adminDSN := requirePostgres(t)
	testDSN := newTestDatabase(t, adminDSN)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const callers = 4

	pools := make([]*pgxpool.Pool, 0, callers)
	for i := 0; i < callers; i++ {
		pool, err := Connect(ctx, testDSN)
		if err != nil {
			t.Fatalf("Connect (caller %d): %v", i, err)
		}
		defer pool.Close()
		pools = append(pools, pool)
	}

	// A barrier so every caller attempts the migration at once. Without it
	// they would naturally stagger and the first would finish before the
	// rest looked, which is the passing case regardless of any locking.
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, callers)

	for i := range pools {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = Migrate(ctx, pools[i])
		}(i)
	}

	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: Migrate returned %v — concurrent callers must all succeed", i, err)
			// Name the specific failure the lock exists to prevent, so a
			// regression reads as itself rather than as a generic error.
			if strings.Contains(err.Error(), "already exists") {
				t.Errorf("caller %d lost a migration race (duplicate object) — the advisory lock is not holding", i)
			}
		}
	}

	// The schema must be applied exactly once, not once per caller: a lock
	// that serialized the callers but let each re-apply would produce
	// duplicate schema_migrations rows (or fail on the primary key).
	var rows int
	if err := pools[0].QueryRow(ctx,
		`SELECT count(*) FROM schema_migrations WHERE version = $1`,
		"0001_initial_schema.sql",
	).Scan(&rows); err != nil {
		t.Fatalf("count schema_migrations rows: %v", err)
	}
	if rows != 1 {
		t.Errorf("schema_migrations has %d rows for the initial migration, want exactly 1", rows)
	}

	for _, table := range []string{"exercises", "exercise_revisions", "taxonomy_terms"} {
		if !tableExists(t, ctx, pools[0], table) {
			t.Errorf("table %q missing after concurrent Migrate", table)
		}
	}
}

// TestMigrate_ReleasesItsLock pins that the advisory lock does not outlive
// the call. A leaked session-level lock would not fail this package's other
// tests — it would fail the *next* deployment, by blocking every instance
// that started after the leak until the holding session happened to close.
func TestMigrate_ReleasesItsLock(t *testing.T) {
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

	// Scoped to this test's own database. pg_locks is cluster-wide, and
	// advisory locks are per-database, so an unfiltered count also sees every
	// other package's Migrate running against its own disposable database —
	// which is exactly what `go test ./...` does, since it runs packages in
	// parallel. Locally under -p 1 the unfiltered query passed; CI failed it
	// immediately, and the lock was never the thing at fault.
	var held int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_locks
		 WHERE locktype = 'advisory'
		   AND objid = $1
		   AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`,
		int32(migrationLockID),
	).Scan(&held); err != nil {
		t.Fatalf("query pg_locks: %v", err)
	}
	if held != 0 {
		t.Errorf("advisory lock still held by %d session(s) after Migrate returned", held)
	}
}
