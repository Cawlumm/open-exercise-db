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

	// Variants are the renditions of this image, one row per stored object.
	// Always contains at least "full" — migration 0002 backfilled one for
	// every pre-existing image from StorageKey — so a caller that finds it
	// empty is looking at a row written outside the normal path, not at an
	// image that has no bytes.
	//
	// StorageKey above remains the full rendition's key. It is not derived
	// from this slice and this slice is not derived from it: the column
	// backs the importer's idempotency lookup (GetImageByStorageKey), which
	// runs before any variant row exists for a brand-new image.
	Variants []ImageVariant `json:"variants"`
}

// ImageVariant is one stored rendition of an Image — one row in
// exercise_image_variants, one object in the media store.
//
// Exists so a storage key is recorded rather than computed. The importer
// used to derive "exercises/<source-id>/<index>.jpg" and every reader
// re-derived the same string, which is sound for exactly one rendition per
// image and becomes a bucket-wide rename the moment there are two. Naming
// each rendition explicitly means added sizes pick their own keys and
// existing objects never move — see migration 0002 for the full rationale.
type ImageVariant struct {
	ID          string    `json:"id"`
	Name        string    `json:"name" doc:"Rendition name — full today; thumb/medium as they are added"`
	StorageKey  string    `json:"storage_key"`
	ContentType string    `json:"content_type"`
	Width       *int      `json:"width,omitempty"`
	Height      *int      `json:"height,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// Image moderation statuses, matching the CHECK in migration 0001. Only an
// approved image is served publicly — see api.requireVisibleImage for why a
// pending one must not be, and why the refusal is a 404 rather than a 403.
const (
	ImageStatusPending  = "pending"
	ImageStatusApproved = "approved"
	ImageStatusRejected = "rejected"
)

// VariantFull is the name every image has a rendition under. Named rather
// than spelled inline at each call site because it is the one variant the
// contract guarantees: contract.Image.URL, Width, and Height all describe
// it, so a typo would silently produce an image whose advertised URL
// resolves to nothing.
const VariantFull = "full"
