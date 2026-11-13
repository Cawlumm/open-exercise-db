package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Cawlumm/open-exercise-db/internal/model"
)

// UserStore holds every query touching `users`.
type UserStore struct {
	pool *pgxpool.Pool
}

const userColumns = `id, github_id, username, avatar_url, role, created_at`

// UpsertGitHubUserInput is what a completed sign-in knows about a person.
//
// There is no Role: a caller cannot set one. Promotion is decided here from
// the configured admin list, so a bug in the OAuth handler cannot mint an
// admin, and neither can anything GitHub returns.
type UpsertGitHubUserInput struct {
	GitHubID  string
	Username  string
	AvatarURL *string

	// PromoteToAdmin is true when this sign-in matches the configured admin list.
	PromoteToAdmin bool

	// DemoteAdmin is set when the admin list is authoritative (ADMIN_GITHUB_IDS)
	// and this sign-in is not on it: a current admin becomes a contributor.
	// Moderators are left alone. Ignored when PromoteToAdmin is set.
	DemoteAdmin bool
}

// UpsertGitHubUser records a sign-in, creating the user on first login and
// refreshing their profile afterwards.
//
// Keyed on github_id, never on username. GitHub usernames are renameable, and
// keying on one would silently create a second account for the same person the
// day they renamed — splitting their submission history and, worse, leaving
// their old moderator role stranded on an account nobody can log into.
//
// Promotion is one-way unless the caller asks otherwise: a sign-in on the list
// lifts the role, and one that is not on it demotes only when DemoteAdmin is
// set, which the handler does only when ADMIN_GITHUB_IDS is configured. With no
// authoritative list a typo in the env var — or a deploy that forgets it —
// would otherwise strip admin from everyone the next time they sign in, which
// is exactly when nobody is watching.
func (s *UserStore) UpsertGitHubUser(ctx context.Context, in UpsertGitHubUserInput) (model.User, error) {
	query := fmt.Sprintf(`
		INSERT INTO users (github_id, username, avatar_url, role)
		VALUES ($1, $2, $3, CASE WHEN $4::boolean THEN 'admin' ELSE 'contributor' END)
		ON CONFLICT (github_id) DO UPDATE
		SET username   = EXCLUDED.username,
		    avatar_url = EXCLUDED.avatar_url,
		    role       = CASE WHEN $4::boolean THEN 'admin'
		                      WHEN $5::boolean AND users.role = 'admin' THEN 'contributor'
		                      ELSE users.role END
		RETURNING %s`, userColumns)

	var u model.User
	err := s.pool.QueryRow(ctx, query, in.GitHubID, in.Username, in.AvatarURL, in.PromoteToAdmin, in.DemoteAdmin).Scan(
		&u.ID, &u.GitHubID, &u.Username, &u.AvatarURL, &u.Role, &u.CreatedAt,
	)
	if err != nil {
		return model.User{}, fmt.Errorf("upsert github user %s: %w", in.GitHubID, err)
	}
	return u, nil
}

// GetByID returns a user, or pgx.ErrNoRows.
//
// Called on every request carrying a user token, so that a role change or a
// deleted account takes effect immediately rather than when the token expires.
// A token that embedded the role would leave a demoted moderator moderating
// for the rest of its lifetime, which is the one moment the demotion mattered.
func (s *UserStore) GetByID(ctx context.Context, id string) (model.User, error) {
	query := fmt.Sprintf(`SELECT %s FROM users WHERE id = $1`, userColumns)

	var u model.User
	err := s.pool.QueryRow(ctx, query, id).Scan(
		&u.ID, &u.GitHubID, &u.Username, &u.AvatarURL, &u.Role, &u.CreatedAt,
	)
	if err != nil {
		return model.User{}, err
	}
	return u, nil
}

// SetRole changes a user's role. Admin-only at the API layer.
func (s *UserStore) SetRole(ctx context.Context, id, role string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE users SET role = $2 WHERE id = $1`, id, role)
	if err != nil {
		return fmt.Errorf("set user %s role: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set user %s role: no such user", id)
	}
	return nil
}
