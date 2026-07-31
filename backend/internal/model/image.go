package model

import "time"

// Image is one re-hosted exercise_images row. Referenced from a revision via
// Revision.ImageIDs (an array column, not a join table — see docs/DESIGN.md's
// "Images/videos are separate tables" note).
type Image struct {
	ID              string    `json:"id"`
	StorageKey      string    `json:"storage_key"`
	ContentType     string    `json:"content_type"`
	Width           *int      `json:"width,omitempty"`
	Height          *int      `json:"height,omitempty"`
	License         *string   `json:"license,omitempty"`
	AttributionText *string   `json:"attribution_text,omitempty"`
	UploadedBy      *string   `json:"uploaded_by,omitempty"`
	Status          string    `json:"status"`
	CreatedAt       time.Time `json:"created_at"`
}
