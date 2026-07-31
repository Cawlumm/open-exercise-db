package model

import "time"

// Exercise is the published, addressable entity: a stable id/slug pair whose
// current_revision_id points at whichever ExerciseRevision is live. The
// exercise row itself carries no exercise content (name, muscle group,
// etc.) — that all lives on the revision, per docs/DESIGN.md's "exercises
// <-> exercise_revisions is deliberately circular" note.
type Exercise struct {
	ID                string    `json:"id"`
	Slug              string    `json:"slug"`
	SourceID          *string   `json:"source_id,omitempty"`
	CurrentRevisionID *string   `json:"current_revision_id,omitempty"`
	Status            string    `json:"status"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`

	// CurrentRevision is populated by store.Exercises.List/Get, which joins
	// the exercise row to its current_revision_id row. It is not a database
	// column.
	CurrentRevision *Revision `json:"current_revision,omitempty"`
}
