package model

import (
	"encoding/json"
	"time"
)

// Submission is one proposed change waiting in the moderation queue — a
// `submissions` row.
//
// Nothing here is published. The queue is deliberately separate from the
// published tables (docs/DESIGN.md core principle 1), and only an approval
// materializes a submission into an exercise revision or translation. That
// separation is enforced by the schema, not by convention: no code path writes
// a revision from this struct except the approve handler.
type Submission struct {
	ID   string `json:"id"`
	Type string `json:"type"`

	// TargetExerciseID is set for every type except create, enforced by
	// submissions_target_exercise_required in migration 0001.
	TargetExerciseID *string `json:"target_exercise_id,omitempty"`

	// Locale is required for translation submissions and meaningless
	// otherwise, enforced by submissions_locale_required.
	Locale *string `json:"locale,omitempty"`

	// Payload is the proposed content, shaped like whatever it will become on
	// approval. Held as raw JSON rather than a typed struct because the shape
	// differs per Type, and because a submission must survive a later change
	// to the exercise schema without the queue rejecting rows it already
	// accepted.
	Payload json.RawMessage `json:"payload"`

	ImageUploadIDs []string `json:"image_upload_ids"`
	VideoUploadIDs []string `json:"video_upload_ids"`

	// Exactly one of SubmittedBy and SubmittedByApp is set — a CHECK, not a
	// convention, because every abuse control downstream keys on one of them
	// and silently does nothing on a row attributed to neither.
	SubmittedBy    *string `json:"submitted_by,omitempty"`
	SubmittedByApp *string `json:"submitted_by_app,omitempty"`

	// AppUserRef is the app's own opaque handle for the person behind this
	// submission. Never an identity in this system: no row, no email, no
	// login. It exists so one bad contributor inside an app can be blocked
	// without suspending the app, and so an app can answer "what happened to
	// my edit?" for its own user.
	AppUserRef *string `json:"app_user_ref,omitempty"`

	Status        string  `json:"status"`
	ReviewerID    *string `json:"reviewer_id,omitempty"`
	ReviewerNotes *string `json:"reviewer_notes,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Submission types, matching the CHECK in migration 0001.
const (
	SubmissionTypeCreate      = "create"
	SubmissionTypeEdit        = "edit"
	SubmissionTypeTranslation = "translation"
)

// Submission statuses, matching the CHECK in migration 0001.
const (
	SubmissionStatusPending      = "pending"
	SubmissionStatusApproved     = "approved"
	SubmissionStatusRejected     = "rejected"
	SubmissionStatusNeedsChanges = "needs_changes"
)

// ValidSubmissionType reports whether t is a type the queue accepts.
//
// Mirrors the database CHECK rather than replacing it. The constraint is what
// actually guarantees the invariant; this exists so a bad type is a 422 naming
// the field rather than a 500 carrying a constraint name the caller cannot act
// on.
func ValidSubmissionType(t string) bool {
	switch t {
	case SubmissionTypeCreate, SubmissionTypeEdit, SubmissionTypeTranslation:
		return true
	}
	return false
}
