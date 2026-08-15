package model

import "time"

// App is a registered third-party client that submits on behalf of its own
// users — one `apps` row.
//
// The model Open Food Facts uses, and for the same reason: contributions
// arrive overwhelmingly through apps whose users have no account on the
// project they are contributing to. See docs/DESIGN.md's auth section.
//
// KeySecret is deliberately absent. Only the hash is stored, so this struct
// can never carry the credential — a logged App, a serialized App, or an App
// rendered into an error message leaks nothing usable.
type App struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	ContactEmail string  `json:"contact_email"`
	KeyID        string  `json:"key_id"`
	OwnerUserID  *string `json:"owner_user_id,omitempty"`
	Status       string  `json:"status"`

	// MaxPending and MaxPerDay are per-app rather than global because an app
	// is the aggregate of many humans: a cap sized for one contributor
	// throttles a legitimate app to one person's budget, while a cap sized
	// for an app would be far too generous for an individual.
	MaxPending int `json:"max_pending"`
	MaxPerDay  int `json:"max_per_day"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// App status values. Suspension is deliberately distinct from deletion:
// submissions already in the queue keep their attribution, and the decision
// is reversible.
const (
	AppStatusActive    = "active"
	AppStatusSuspended = "suspended"
)

// Active reports whether this app may currently submit. A suspended app
// authenticates successfully — the credential is still valid, and saying
// otherwise would send its operator hunting for a key problem that doesn't
// exist — but is refused at the authorization step with a distinct reason.
func (a App) Active() bool { return a.Status == AppStatusActive }
