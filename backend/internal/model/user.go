package model

import (
	"strings"
	"time"
)

// User is a person who signed in with GitHub — one `users` row.
//
// There is no password column and never will be: authentication is delegated
// to GitHub entirely (docs/DESIGN.md's auth section), so this project stores
// no credential for a human and has nothing to leak if the database is.
type User struct {
	ID        string    `json:"id"`
	GitHubID  string    `json:"github_id"`
	Username  string    `json:"username"`
	AvatarURL *string   `json:"avatar_url,omitempty"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

// Roles, matching the CHECK in migration 0001.
const (
	RoleContributor = "contributor"
	RoleModerator   = "moderator"
	RoleAdmin       = "admin"
)

// CanModerate reports whether this user may approve, reject, or revert.
//
// Admin implies moderator rather than being a separate grant: every admin
// power is a superset, and a two-list scheme would eventually produce an admin
// who cannot review, which is a state nobody intends and everybody has to
// remember.
func (u User) CanModerate() bool {
	return u.Role == RoleModerator || u.Role == RoleAdmin
}

// IsAdmin reports whether this user may register apps and change roles.
func (u User) IsAdmin() bool { return u.Role == RoleAdmin }

// AdminLogins parses ADMIN_GITHUB_LOGINS — a comma-separated list of GitHub
// usernames promoted to admin on sign-in.
//
// Comparison is case-insensitive because GitHub usernames are: "Cawlumm" and
// "cawlumm" are the same account, and an operator who types the wrong case
// into an env var would otherwise get a silent non-promotion — the failure
// being "I logged in and I'm still a contributor", with nothing to indicate
// why.
func AdminLogins(raw string) map[string]bool {
	out := map[string]bool{}
	for _, login := range strings.Split(raw, ",") {
		if login = strings.ToLower(strings.TrimSpace(login)); login != "" {
			out[login] = true
		}
	}
	return out
}

// AdminGitHubIDs parses ADMIN_GITHUB_IDS — a comma-separated list of numeric
// GitHub user ids. Unlike a login, an id is permanent: a renamed account keeps
// it, and a released username cannot be claimed by someone else to inherit
// admin. Entries that are not plain digits are skipped here; config.Validate
// rejects them at startup so a typo cannot go unnoticed.
func AdminGitHubIDs(raw string) map[string]bool {
	out := map[string]bool{}
	for _, id := range strings.Split(raw, ",") {
		if id = strings.TrimSpace(id); isDigitsOnly(id) {
			out[id] = true
		}
	}
	return out
}

// isDigitsOnly reports whether s is a non-empty string of ASCII digits.
func isDigitsOnly(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
