package model

import "time"

// Revision is one immutable version of an exercise's content. History is
// append-only: nothing here is ever mutated or deleted, so a revert is just
// repointing Exercise.CurrentRevisionID at an older row (docs/DESIGN.md).
//
// MuscleGroup, Category, Equipment, Mechanic, Force, and Level are taxonomy
// slugs (facet-scoped, see docs/PHASE1_BUILD_PLAN.md's WO-1 correction) —
// validated against taxonomy_terms in the app layer, not by a database FK.
type Revision struct {
	ID               string    `json:"id"`
	ExerciseID       string    `json:"exercise_id"`
	Name             string    `json:"name"`
	Aliases          []string  `json:"aliases"`
	MuscleGroup      string    `json:"muscle_group"`
	SecondaryMuscles []string  `json:"secondary_muscles"`
	Category         string    `json:"category"`
	Equipment        string    `json:"equipment"`
	Mechanic         *string   `json:"mechanic,omitempty"`
	Force            *string   `json:"force,omitempty"`
	Level            *string   `json:"level,omitempty"`
	Instructions     []string  `json:"instructions"`
	ImageIDs         []string  `json:"image_ids"`
	VideoIDs         []string  `json:"video_ids"`
	License          *string   `json:"license,omitempty"`
	AttributionText  *string   `json:"attribution_text,omitempty"`
	SourceURL        *string   `json:"source_url,omitempty"`
	SubmittedBy      *string   `json:"submitted_by,omitempty"`
	ReviewedBy       *string   `json:"reviewed_by,omitempty"`
	ChangeSummary    *string   `json:"change_summary,omitempty"`
	Status           string    `json:"status"`
	CreatedAt        time.Time `json:"created_at"`
}
