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

// GetImageByID returns the exercise_images row for id, or pgx.ErrNoRows if
// none exists — GET /api/v1/images/{id} (internal/api/images.go) uses this
// to resolve storage_key/content_type before streaming bytes from
// media.Store, and mapStoreError translates the not-found case to a 404
// there, same as every other id lookup in this package.
func (s *MediaStore) GetImageByID(ctx context.Context, id string) (model.Image, error) {
	query := fmt.Sprintf(`SELECT %s FROM exercise_images WHERE id = $1`, imageColumns)

	var img model.Image
	err := s.pool.QueryRow(ctx, query, id).Scan(
		&img.ID, &img.StorageKey, &img.ContentType, &img.Width, &img.Height,
		&img.License, &img.AttributionText, &img.UploadedBy, &img.Status, &img.CreatedAt,
	)
	if err != nil {
		return model.Image{}, err
	}
	return img, nil
}

// ListImagesByIDs returns the exercise_images rows for ids, in the same
// order as ids — the order a revision's image_ids[] array establishes
// (docs/DESIGN.md's "Images/videos are separate tables" note), and the order
// internal/media.BuildImages preserves into contract.Exercise.Images. Any id
// with no matching row is silently skipped rather than erroring: in
// practice this never happens (an id only ever lands in image_ids after its
// exercise_images row exists — see internal/seed's insert-then-attach
// import ordering), and a single dangling reference shouldn't take down an
// otherwise-servable exercise response.
func (s *MediaStore) ListImagesByIDs(ctx context.Context, ids []string) ([]model.Image, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	query := fmt.Sprintf(`SELECT %s FROM exercise_images WHERE id = ANY($1)`, imageColumns)
	rows, err := s.pool.Query(ctx, query, ids)
	if err != nil {
		return nil, fmt.Errorf("list images by ids: %w", err)
	}
	defer rows.Close()

	byID := make(map[string]model.Image, len(ids))
	for rows.Next() {
		var img model.Image
		if err := rows.Scan(
			&img.ID, &img.StorageKey, &img.ContentType, &img.Width, &img.Height,
			&img.License, &img.AttributionText, &img.UploadedBy, &img.Status, &img.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan image: %w", err)
		}
		byID[img.ID] = img
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list images by ids: %w", err)
	}

	images := make([]model.Image, 0, len(ids))
	for _, id := range ids {
		if img, ok := byID[id]; ok {
			images = append(images, img)
		}
	}
	return images, nil
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

// UpdateImageEncoding re-points an existing row's encoding facts at bytes
// that were just rewritten under the same storage key, leaving identity
// (id), provenance (license, attribution_text), and moderation (status)
// untouched.
//
// Exists for internal/seed's repair path, which re-uploads an image whose
// object went missing while its row survived. It keeps the id — that is the
// entire point, since exercise_revisions.image_ids reference it and
// re-inserting would churn every revision — but the bytes it writes are
// freshly encoded, and media.Encode's output is a function of the encoder's
// current dimension cap and format rules, not a constant across versions.
// Without this the row would keep asserting the width, height, and content
// type of an encode that no longer describes the object, which is the same
// species of quiet lie as the missing object it just repaired.
func (s *MediaStore) UpdateImageEncoding(ctx context.Context, id, contentType string, width, height *int) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE exercise_images
		SET content_type = $2, width = $3, height = $4
		WHERE id = $1`,
		id, contentType, width, height,
	)
	if err != nil {
		return fmt.Errorf("update image encoding %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("update image encoding %s: no such image", id)
	}
	return nil
}
