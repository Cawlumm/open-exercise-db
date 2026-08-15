package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Cawlumm/open-exercise-db/internal/model"
)

// AppStore holds every query touching `apps`, plus the per-app submission
// counts the write path checks before accepting anything.
type AppStore struct {
	pool *pgxpool.Pool
}

// appColumns is the explicit column list every app read shares. key_hash is
// absent on purpose: it is only ever needed by GetKeyHashByKeyID, which asks
// for it by name, so no ordinary read can accidentally carry the secret
// material into a log line or an error string.
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

// GetKeyHashByKeyID returns only the stored hash for a key_id, for the caller
// to compare against a presented secret.
//
// Separate from GetByKeyID so the hash is fetched deliberately rather than
// riding along on every app read. The comparison itself belongs to the caller
// (internal/api's auth middleware) because it owns the hashing scheme; this
// package's job is storage, and putting the verification here would mean the
// store had to know how secrets are derived.
func (s *AppStore) GetKeyHashByKeyID(ctx context.Context, keyID string) (string, error) {
	var hash string
	if err := s.pool.QueryRow(ctx, `SELECT key_hash FROM apps WHERE key_id = $1`, keyID).Scan(&hash); err != nil {
		return "", err
	}
	return hash, nil
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

	// Zero means "use the schema default" rather than "no submissions
	// allowed", which a CHECK (> 0) would reject anyway. Callers that want
	// the defaults leave these unset instead of duplicating the numbers.
	MaxPending int
	MaxPerDay  int
}

// CreateApp registers an app and returns the stored row.
//
// The caller supplies KeyHash, never a secret — this package never sees the
// credential it is storing a hash of.
func (s *AppStore) CreateApp(ctx context.Context, in CreateAppInput) (model.App, error) {
	// NULLIF(...,0) lets the column defaults apply for unset caps, so the
	// numbers live in the migration rather than being restated here where
	// they could drift.
	query := fmt.Sprintf(`
		INSERT INTO apps (name, contact_email, key_id, key_hash, owner_user_id, max_pending, max_per_day)
		VALUES ($1, $2, $3, $4, $5, COALESCE(NULLIF($6, 0), 50), COALESCE(NULLIF($7, 0), 200))
		RETURNING %s`, appColumns)

	var a model.App
	err := s.pool.QueryRow(ctx, query,
		in.Name, in.ContactEmail, in.KeyID, in.KeyHash, in.OwnerUserID, in.MaxPending, in.MaxPerDay,
	).Scan(
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
		return fmt.Errorf("set app %s status: no such app", id)
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
		return fmt.Errorf("rotate app %s key: no such app", id)
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
// Both counts in one round trip because they are always needed together — a
// submission is refused if either is over — and because two separate queries
// could observe different states either side of a concurrent insert, which
// would let a burst slip through the pair.
//
// appUserRef may be empty, in which case PerRefID is zero and only the
// app-wide caps are populated.
func (s *AppStore) UsageFor(ctx context.Context, appID, appUserRef string, window time.Duration) (AppUsage, error) {
	var u AppUsage
	err := s.pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE status = 'pending'),
			count(*) FILTER (WHERE created_at > now() - $2::interval),
			count(*) FILTER (WHERE created_at > now() - $2::interval
			                   AND $3 <> '' AND app_user_ref = $3)
		FROM submissions
		WHERE submitted_by_app = $1`,
		appID, window, appUserRef,
	).Scan(&u.Pending, &u.LastDay, &u.PerRefID)
	if err != nil {
		return AppUsage{}, fmt.Errorf("app %s usage: %w", appID, err)
	}
	return u, nil
}
