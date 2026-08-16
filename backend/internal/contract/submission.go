package contract

import (
	"encoding/json"
	"time"
)

// Submission is the public shape of a queued change.
//
// Deliberately narrower than model.Submission. Reviewer identity and notes are
// omitted: a moderator's name and their internal reasoning are not the
// submitter's business, and publishing them would make every rejection a
// personal exchange rather than a queue decision. Status carries everything a
// submitter can act on.
//
// SubmittedByApp is likewise absent — an app already knows it sent this, and
// the field would leak one app's id to another the moment a shared listing
// exists.
type Submission struct {
	ID   string `json:"id" doc:"Stable identifier for this submission"`
	Type string `json:"type" doc:"create, edit, or translation"`

	TargetExerciseID *string `json:"target_exercise_id,omitempty" doc:"The exercise being edited or translated"`
	Locale           *string `json:"locale,omitempty" doc:"Locale of a translation submission"`

	Payload json.RawMessage `json:"payload" doc:"The proposed content, echoed back as stored"`

	// Echoed back so an app can reconcile a submission against its own user
	// without keeping a local mapping from submission id to person.
	AppUserRef *string `json:"app_user_ref,omitempty" doc:"The app's own opaque handle for the contributor"`

	Status string `json:"status" doc:"pending, approved, rejected, or needs_changes"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
