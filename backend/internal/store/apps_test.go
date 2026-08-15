package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// insertTestApp registers an app with the schema's default caps, returning
// its id.
func insertTestApp(t *testing.T, st *Store, name string) string {
	t.Helper()

	app, err := st.Apps.CreateApp(context.Background(), CreateAppInput{
		Name:         name,
		ContactEmail: name + "@example.test",
		KeyID:        "key-" + name,
		KeyHash:      "hash-of-" + name,
	})
	if err != nil {
		t.Fatalf("create app %s: %v", name, err)
	}
	return app.ID
}

func TestCreateApp_DefaultsAndStatus(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	app, err := st.Apps.CreateApp(ctx, CreateAppInput{
		Name:         "lyftr",
		ContactEmail: "dev@lyftr.test",
		KeyID:        "key-lyftr",
		KeyHash:      "hash-lyftr",
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	// Unset caps take the schema's defaults rather than zero, which the
	// CHECK (> 0) would have rejected outright.
	if app.MaxPending != 50 || app.MaxPerDay != 200 {
		t.Errorf("default caps = (%d, %d), want (50, 200)", app.MaxPending, app.MaxPerDay)
	}
	if !app.Active() {
		t.Errorf("new app status = %q, want active", app.Status)
	}
	if app.ID == "" {
		t.Error("new app has no id")
	}
}

func TestGetByKeyID_ReturnsSuspendedAppsToo(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	id := insertTestApp(t, st, "suspended-app")
	if err := st.Apps.SetStatus(ctx, id, "suspended"); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	// A suspended app's credential is still real. Reporting "unknown key"
	// here would send its operator hunting for a key problem that doesn't
	// exist, so authentication succeeds and App.Active carries the refusal.
	app, err := st.Apps.GetByKeyID(ctx, "key-suspended-app")
	if err != nil {
		t.Fatalf("get suspended app by key id: %v", err)
	}
	if app.Active() {
		t.Error("suspended app reports Active() = true")
	}
}

func TestGetByKeyID_UnknownKeyIsErrNoRows(t *testing.T) {
	st, _ := newTestStore(t)

	_, err := st.Apps.GetByKeyID(context.Background(), "key-that-was-never-issued")
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unknown key_id error = %v, want pgx.ErrNoRows", err)
	}
}

func TestRotateKey_KeepsIdentityAndAttribution(t *testing.T) {
	st, pool := newTestStore(t)
	ctx := context.Background()

	id := insertTestApp(t, st, "rotating-app")

	// A submission attributed to this app before the rotation.
	if _, err := pool.Exec(ctx, `
		INSERT INTO submissions (type, payload, submitted_by_app, app_user_ref)
		VALUES ('create', '{}'::jsonb, $1, 'u-1')`, id); err != nil {
		t.Fatalf("insert submission: %v", err)
	}

	if err := st.Apps.RotateKey(ctx, id, "key-rotated", "hash-rotated"); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	// The old credential stops working...
	if _, err := st.Apps.GetByKeyID(ctx, "key-rotating-app"); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("old key_id still resolves, err = %v", err)
	}
	// ...the new one works, and it is the same app.
	rotated, err := st.Apps.GetByKeyID(ctx, "key-rotated")
	if err != nil {
		t.Fatalf("get by new key id: %v", err)
	}
	if rotated.ID != id {
		t.Errorf("rotated app id = %s, want %s", rotated.ID, id)
	}

	// The point of storing the credential in columns rather than as the
	// primary key: rotation costs no attribution.
	var attributed int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM submissions WHERE submitted_by_app = $1`, id).Scan(&attributed); err != nil {
		t.Fatalf("count attributed: %v", err)
	}
	if attributed != 1 {
		t.Errorf("submissions still attributed after rotation = %d, want 1", attributed)
	}
}

func TestUsageFor_CountsPendingDailyAndPerUser(t *testing.T) {
	st, pool := newTestStore(t)
	ctx := context.Background()

	appID := insertTestApp(t, st, "counting-app")
	other := insertTestApp(t, st, "other-app")

	// Three from u-1 (two pending, one approved), one from u-2, and one from
	// a different app entirely — which must not be counted at all.
	rows := []struct {
		app, ref, status string
	}{
		{appID, "u-1", "pending"},
		{appID, "u-1", "pending"},
		{appID, "u-1", "approved"},
		{appID, "u-2", "pending"},
		{other, "u-1", "pending"},
	}
	for _, r := range rows {
		if _, err := pool.Exec(ctx, `
			INSERT INTO submissions (type, payload, submitted_by_app, app_user_ref, status)
			VALUES ('create', '{}'::jsonb, $1, $2, $3)`, r.app, r.ref, r.status); err != nil {
			t.Fatalf("insert submission: %v", err)
		}
	}

	// Two rows from u-1 outside the window. Without these every fixture sits
	// at now(), the window predicate is true for all of them, and deleting it
	// entirely leaves this test green — which would let the rolling-24h cap
	// silently become an all-time count, permanently locking out any app that
	// ever reached its daily limit.
	if _, err := pool.Exec(ctx, `
		INSERT INTO submissions (type, payload, submitted_by_app, app_user_ref, status, created_at)
		VALUES ('create', '{}'::jsonb, $1, 'u-1', 'pending',  now() - interval '48 hours'),
		       ('create', '{}'::jsonb, $1, 'u-1', 'approved', now() - interval '48 hours')`,
		appID); err != nil {
		t.Fatalf("insert aged submissions: %v", err)
	}

	u, err := st.Apps.UsageFor(ctx, appID, "u-1", 24*time.Hour)
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	// Pending is deliberately unwindowed — an outstanding submission stays
	// outstanding however old it is — so the aged pending row counts here.
	if u.Pending != 4 {
		t.Errorf("Pending = %d, want 4 (all this app's pending rows, any age)", u.Pending)
	}
	// LastDay is windowed, so neither aged row counts.
	if u.LastDay != 4 {
		t.Errorf("LastDay = %d, want 4 (this app, last 24h only — aged rows excluded)", u.LastDay)
	}
	// The same ref string under a different app refers to a different person
	// and must never collide.
	if u.PerRefID != 3 {
		t.Errorf("PerRefID = %d, want 3 (u-1 under this app only)", u.PerRefID)
	}

	// Empty ref means "app-wide only", not "match rows with no ref".
	uAll, err := st.Apps.UsageFor(ctx, appID, "", 24*time.Hour)
	if err != nil {
		t.Fatalf("usage without ref: %v", err)
	}
	if uAll.PerRefID != 0 {
		t.Errorf("PerRefID with empty ref = %d, want 0", uAll.PerRefID)
	}
	if uAll.Pending != 4 {
		t.Errorf("Pending with empty ref = %d, want 4", uAll.Pending)
	}
}

func TestSubmissions_EmptyAppUserRefIsRejected(t *testing.T) {
	st, pool := newTestStore(t)
	ctx := context.Background()

	appID := insertTestApp(t, st, "empty-ref-app")

	// An empty ref is not a contributor. Allowed, an app could send '' on
	// every submission and the per-contributor counter would have nothing to
	// distinguish them by — the cap could never fire, leaving suspension of
	// the entire app as the only lever, which is the outcome the two-lever
	// design exists to avoid.
	for _, ref := range []string{"", "   "} {
		_, err := pool.Exec(ctx, `
			INSERT INTO submissions (type, payload, submitted_by_app, app_user_ref)
			VALUES ('create', '{}'::jsonb, $1, $2)`, appID, ref)
		if err == nil {
			t.Errorf("app_user_ref %q was accepted", ref)
			continue
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.ConstraintName != "submissions_app_user_ref_requires_app" {
			t.Errorf("app_user_ref %q rejected by %v, want submissions_app_user_ref_requires_app", ref, err)
		}
	}

	// NULL stays legal: "no per-user attribution offered" is a different
	// claim from "this user has no name".
	if _, err := pool.Exec(ctx, `
		INSERT INTO submissions (type, payload, submitted_by_app)
		VALUES ('create', '{}'::jsonb, $1)`, appID); err != nil {
		t.Fatalf("NULL app_user_ref rejected: %v", err)
	}
}

func TestSubmissions_AttributionConstraints(t *testing.T) {
	st, pool := newTestStore(t)
	ctx := context.Background()

	appID := insertTestApp(t, st, "constraint-app")

	// These assert that an INSERT is *rejected*, which a typo'd column name
	// would also satisfy — so each one checks the specific constraint that
	// did the rejecting, not merely that something went wrong.
	rejectedBy := func(t *testing.T, want string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("insert was accepted; expected %s to reject it", want)
			return
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Errorf("error is not a PgError: %v", err)
			return
		}
		if pgErr.Code != "23514" { // check_violation
			t.Errorf("SQLSTATE = %s (%s), want 23514 check_violation", pgErr.Code, pgErr.Message)
			return
		}
		if pgErr.ConstraintName != want {
			t.Errorf("rejected by %q, want %q", pgErr.ConstraintName, want)
		}
	}

	var userID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (github_id, username) VALUES ('gh-1', 'someone') RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	// Each case violates exactly one constraint. A row that breaks both —
	// say, app_user_ref with no attribution at all — is rejected by whichever
	// Postgres evaluates first, so asserting on the constraint name there
	// tests evaluation order rather than the rule.

	// Attributed to nobody: would sail past every per-app and per-user cap
	// while looking like a normal pending row in the queue.
	_, err := pool.Exec(ctx, `
		INSERT INTO submissions (type, payload) VALUES ('create', '{}'::jsonb)`)
	rejectedBy(t, "submissions_one_attribution", err)

	// Attributed to a person AND an app at once: the caps would then be
	// checked against one identity while the queue displayed the other.
	_, err = pool.Exec(ctx, `
		INSERT INTO submissions (type, payload, submitted_by, submitted_by_app)
		VALUES ('create', '{}'::jsonb, $1, $2)`, userID, appID)
	rejectedBy(t, "submissions_one_attribution", err)

	// Validly attributed to a person, but carrying an app_user_ref — the one
	// shape that isolates the second constraint. Left unchecked, the
	// per-app-user blocking logic would count this against an app that never
	// sent it.
	_, err = pool.Exec(ctx, `
		INSERT INTO submissions (type, payload, submitted_by, app_user_ref)
		VALUES ('create', '{}'::jsonb, $1, 'u-1')`, userID)
	rejectedBy(t, "submissions_app_user_ref_requires_app", err)

	// Both valid shapes still work.
	if _, err := pool.Exec(ctx, `
		INSERT INTO submissions (type, payload, submitted_by_app, app_user_ref)
		VALUES ('create', '{}'::jsonb, $1, 'u-1')`, appID); err != nil {
		t.Fatalf("valid app submission rejected: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO submissions (type, payload, submitted_by)
		VALUES ('create', '{}'::jsonb, $1)`, userID); err != nil {
		t.Fatalf("valid human submission rejected: %v", err)
	}
}
