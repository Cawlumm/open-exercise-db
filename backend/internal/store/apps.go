package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Cawlumm/open-exercise-db/internal/model"
)

// ErrNoSuchApp is returned by the update methods when no row matched.
//
// A sentinel rather than a formatted string, because the API layer has to turn
// it into a 404 and matching on message text is a test that passes until
// somebody rewords an error. An UPDATE affecting no rows is not a failed scan,
// so pgx.ErrNoRows is not available to mean this.
var ErrNoSuchApp = errors.New("no such app")

// AppStore holds every query touching `apps`, plus the per-app submission
// counts the write path checks before accepting anything.
type AppStore struct {
	pool *pgxpool.Pool
}

// appColumns is the explicit column list every app read shares. key_hash is
// absent on purpose: only GetByKeyIDWithHash asks for it, and it returns the
// hash as a separate value rather than on model.App, so no ordinary read can
// carry secret material into a log line or an error string.
const appColumns = `id, name, contact_email, key_id, owner_user_id, status, max_pending, max_per_day, created_at, updated_at`

// GetByKeyID resolves the public half of an app credential to its row, or
// pgx.ErrNoRows when no app claims that key_id.
//
// Returns the app regardless of status. A suspended app has a perfectly valid
// credential, and reporting "unknown key" for one would send its operator
// looking for a key problem that doesn't exist; the caller distinguishes
// authentication (is this credential real?) from authorization (may this app
// submit right now?) using App.Active.
func (s *AppStore) GetByKeyID(ctx context.Context, keyID string) (model.App, error) {
	query := fmt.Sprintf(`SELECT %s FROM apps WHERE key_id = $1`, appColumns)

	var a model.App
	err := s.pool.QueryRow(ctx, query, keyID).Scan(
		&a.ID, &a.Name, &a.ContactEmail, &a.KeyID, &a.OwnerUserID,
		&a.Status, &a.MaxPending, &a.MaxPerDay, &a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		return model.App{}, err
	}
	return a, nil
}

// GetByID returns an app by its primary key, or pgx.ErrNoRows.
func (s *AppStore) GetByID(ctx context.Context, id string) (model.App, error) {
	query := fmt.Sprintf(`SELECT %s FROM apps WHERE id = $1`, appColumns)

	var a model.App
	err := s.pool.QueryRow(ctx, query, id).Scan(
		&a.ID, &a.Name, &a.ContactEmail, &a.KeyID, &a.OwnerUserID,
		&a.Status, &a.MaxPending, &a.MaxPerDay, &a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		return model.App{}, err
	}
	return a, nil
}

// GetByKeyIDWithHash returns the app row together with its stored key hash,
// or pgx.ErrNoRows when no app claims that key_id.
//
// One query rather than two, because authenticating a request needs both and
// this is the hot path for the entire write API: a split would double the
// pool round trips per authenticated request, which is exactly the cost
// internal/api orders its middleware to avoid.
//
// The hash is returned rather than compared here on purpose. Verification
// belongs to the caller (internal/api's auth middleware) because that is what
// owns the hashing scheme; this package's job is storage, and putting the
// comparison here would mean the store had to know how secrets are derived.
// Returning it separately from model.App keeps it off the struct, so no
// ordinary app read can carry secret material into a log line.
func (s *AppStore) GetByKeyIDWithHash(ctx context.Context, keyID string) (model.App, string, error) {
	query := fmt.Sprintf(`SELECT %s, key_hash FROM apps WHERE key_id = $1`, appColumns)

	var a model.App
	var hash string
	err := s.pool.QueryRow(ctx, query, keyID).Scan(
		&a.ID, &a.Name, &a.ContactEmail, &a.KeyID, &a.OwnerUserID,
		&a.Status, &a.MaxPending, &a.MaxPerDay, &a.CreatedAt, &a.UpdatedAt,
		&hash,
	)
	if err != nil {
		return model.App{}, "", err
	}
	return a, hash, nil
}

// CreateAppInput is the set of fields a caller controls when registering an
// app. There is no Status: a new app is always active — registering one
// pre-suspended has no use case, and defaulting it in SQL keeps the rule in
// one place.
type CreateAppInput struct {
	Name         string
	ContactEmail string
	KeyID        string
	KeyHash      string
	OwnerUserID  *string

	// Nil means "use whatever the schema says", not "zero" — a CHECK (> 0)
	// would reject zero anyway. Pointers rather than an int with a sentinel
	// so that omitting the columns entirely is expressible, which is what
	// keeps the actual numbers in the migration.
	MaxPending *int
	MaxPerDay  *int
}

// CreateApp registers an app and returns the stored row.
//
// The caller supplies KeyHash, never a secret — this package never sees the
// credential it is storing a hash of.
//
// Unset caps are omitted from the INSERT rather than defaulted here, so the
// column DEFAULTs in migration 0003 actually apply. Naming a fallback in Go —
// even via COALESCE — would silently shadow the schema: raising a default by
// appending a migration would then change nothing for newly registered apps,
// and the mismatch would surface much later as an integration hitting a cap
// the operator is certain they raised.
func (s *AppStore) CreateApp(ctx context.Context, in CreateAppInput) (model.App, error) {
	cols := []string{"name", "contact_email", "key_id", "key_hash", "owner_user_id"}
	args := []any{in.Name, in.ContactEmail, in.KeyID, in.KeyHash, in.OwnerUserID}
	if in.MaxPending != nil {
		cols = append(cols, "max_pending")
		args = append(args, *in.MaxPending)
	}
	if in.MaxPerDay != nil {
		cols = append(cols, "max_per_day")
		args = append(args, *in.MaxPerDay)
	}

	placeholders := make([]string, len(args))
	for i := range args {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}

	query := fmt.Sprintf(`INSERT INTO apps (%s) VALUES (%s) RETURNING %s`,
		strings.Join(cols, ", "), strings.Join(placeholders, ", "), appColumns)

	var a model.App
	err := s.pool.QueryRow(ctx, query, args...).Scan(
		&a.ID, &a.Name, &a.ContactEmail, &a.KeyID, &a.OwnerUserID,
		&a.Status, &a.MaxPending, &a.MaxPerDay, &a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		return model.App{}, fmt.Errorf("create app %q: %w", in.Name, err)
	}
	return a, nil
}

// SetStatus suspends or reactivates an app — the blunt lever, when everything
// from one source has to stop at once.
func (s *AppStore) SetStatus(ctx context.Context, id, status string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE apps SET status = $2, updated_at = now() WHERE id = $1`, id, status)
	if err != nil {
		return fmt.Errorf("set app %s status: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set app %s status: %w", id, ErrNoSuchApp)
	}
	return nil
}

// RotateKey re-points an app at a new credential without touching its
// identity.
//
// Rotation deliberately costs no attribution: submissions reference apps by
// id, so every past submitted_by_app stays intact and the moderation queue
// reads identically before and after. That is the whole reason the credential
// lives in columns rather than being the app's primary key.
func (s *AppStore) RotateKey(ctx context.Context, id, keyID, keyHash string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE apps SET key_id = $2, key_hash = $3, updated_at = now() WHERE id = $1`,
		id, keyID, keyHash)
	if err != nil {
		return fmt.Errorf("rotate app %s key: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("rotate app %s key: %w", id, ErrNoSuchApp)
	}
	return nil
}

// AppUsage is what the write path checks a submission against, before
// accepting it.
type AppUsage struct {
	Pending  int
	LastDay  int
	PerRefID int // submissions from this app_user_ref in the same window
}

// UsageFor counts an app's outstanding and recent submissions, optionally
// narrowed to one of the app's own users.
//
// All three counts in one round trip because they are always needed together
// — a submission is refused if any is over — and because separate queries
// could observe different states either side of a concurrent insert, letting
// a burst slip through the gap.
//
// Written as three scalar subqueries rather than one scan with count(*)
// FILTER clauses. A FILTER is evaluated per row *after* the scan, so the
// window predicate could not use idx_submissions_app_created_at and every cap
// check read the app's entire submission history — a cost that grows with an
// app's success, on the path it hits hardest. Each subquery below carries its
// own WHERE, so the planner can restrict on the matching index.
//
// appUserRef may be empty, in which case PerRefID is zero: NULLIF turns it
// into a NULL that `app_user_ref = NULL` never matches. That is a sargable
// way to express "app-wide only" — a `$3 <> ”` guard inside the predicate
// would defeat the index for every caller, including the ones that do pass a
// ref.
func (s *AppStore) UsageFor(ctx context.Context, appID, appUserRef string, window time.Duration) (AppUsage, error) {
	var u AppUsage
	err := s.pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM submissions
			  WHERE submitted_by_app = $1 AND status = 'pending'),
			(SELECT count(*) FROM submissions
			  WHERE submitted_by_app = $1 AND created_at > now() - $2::interval),
			(SELECT count(*) FROM submissions
			  WHERE submitted_by_app = $1 AND created_at > now() - $2::interval
			    AND app_user_ref = NULLIF($3, ''))`,
		appID, window, appUserRef,
	).Scan(&u.Pending, &u.LastDay, &u.PerRefID)
	if err != nil {
		return AppUsage{}, fmt.Errorf("app %s usage: %w", appID, err)
	}
	return u, nil
}
