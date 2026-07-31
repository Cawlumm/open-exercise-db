package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Cawlumm/open-exercise-db/internal/model"
)

// MediaStore holds every query touching exercise_images. (exercise_videos
// isn't needed until Phase 2's upload endpoints exist, so it has no store
// methods yet — added when something actually calls them, per
// docs/CONVENTIONS.md's "no structure ahead of need.")
type MediaStore struct {
	pool *pgxpool.Pool
}

// imageColumns is the explicit column list shared by GetImageByStorageKey
// and InsertImage's RETURNING-backed read.
const imageColumns = `id, storage_key, content_type, width, height, license, attribution_text, uploaded_by, status, created_at`

// GetImageByStorageKey returns the exercise_images row for storageKey, or
// pgx.ErrNoRows if none exists yet. The importer
// (internal/seed/freeexercisedb.go) uses this to make re-hosted images
// idempotent: a deterministic storage key derived from the upstream source
// id + index means a re-run recognizes an already-uploaded image and reuses
// its id instead of re-downloading, re-encoding, and inserting a duplicate
// row every time — which in turn is what keeps a revision's image_ids
// stable across runs so an unchanged upstream item never looks "changed"
// just because images got fresh random ids.
func (s *MediaStore) GetImageByStorageKey(ctx context.Context, storageKey string) (model.Image, error) {
	query := fmt.Sprintf(`SELECT %s FROM exercise_images WHERE storage_key = $1`, imageColumns)

	var img model.Image
	err := s.pool.QueryRow(ctx, query, storageKey).Scan(
		&img.ID, &img.StorageKey, &img.ContentType, &img.Width, &img.Height,
		&img.License, &img.AttributionText, &img.UploadedBy, &img.Status, &img.CreatedAt,
	)
	if err != nil {
		return model.Image{}, err
	}
	return img, nil
}

// InsertImageInput is the set of fields the importer controls when
// re-hosting an image. UploadedBy is always nil for the free-exercise-db
// bootstrap, mirroring exercise_revisions.submitted_by=NULL for the same
// trusted-import reason.
type InsertImageInput struct {
	StorageKey      string
	ContentType     string
	Width           *int
	Height          *int
	License         *string
	AttributionText *string
	Status          string
}

// InsertImage records a newly re-hosted image and returns its id.
func (s *MediaStore) InsertImage(ctx context.Context, in InsertImageInput) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO exercise_images (storage_key, content_type, width, height, license, attribution_text, uploaded_by, status)
		VALUES ($1, $2, $3, $4, $5, $6, NULL, $7)
		RETURNING id`,
		in.StorageKey, in.ContentType, in.Width, in.Height, in.License, in.AttributionText, in.Status,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert image %s: %w", in.StorageKey, err)
	}
	return id, nil
}
