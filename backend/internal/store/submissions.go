package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Cawlumm/open-exercise-db/internal/model"
)

// SubmissionStore holds every query touching the moderation queue.
type SubmissionStore struct {
	pool *pgxpool.Pool
}

// submissionColumns is the explicit column list every submission read shares.
const submissionColumns = `id, type, target_exercise_id, locale, payload,
	image_upload_ids, video_upload_ids, submitted_by, submitted_by_app,
	app_user_ref, status, reviewer_id, reviewer_notes, created_at, updated_at`

func scanSubmission(row rowScanner) (model.Submission, error) {
	var s model.Submission
	var payload []byte
	err := row.Scan(
		&s.ID, &s.Type, &s.TargetExerciseID, &s.Locale, &payload,
		&s.ImageUploadIDs, &s.VideoUploadIDs, &s.SubmittedBy, &s.SubmittedByApp,
		&s.AppUserRef, &s.Status, &s.ReviewerID, &s.ReviewerNotes,
		&s.CreatedAt, &s.UpdatedAt,
	)
	if err != nil {
		return model.Submission{}, err
	}
	s.Payload = json.RawMessage(payload)
	return s, nil
}

// CreateAppSubmissionInput is one proposed change arriving from an app.
//
// No Status and no reviewer fields: a new submission is always pending, and
// nothing a submitter sends may influence its review state. Leaving them off
// the input type is what makes that unforgeable rather than merely unchecked.
type CreateAppSubmissionInput struct {
	Type             string
	TargetExerciseID *string
	Locale           *string
	Payload          json.RawMessage
	AppID            string
	AppUserRef       *string
	ImageUploadIDs   []string
}

// CreateAppSubmission enqueues a submission attributed to an app.
//
// Deliberately app-specific rather than a general Create taking either
// attribution. The two paths differ in more than which column is set — they
// have different caps, different validation, and different abuse levers — and
// a single function taking both nullable would make "exactly one attribution"
// a runtime check inside this package rather than something the caller cannot
// express wrongly.
func (s *SubmissionStore) CreateAppSubmission(ctx context.Context, in CreateAppSubmissionInput) (model.Submission, error) {
	// $7::uuid[] is load-bearing. Without it Postgres infers the COALESCE from
	// the untyped '{}' literal, decides the whole expression is text, and
	// rejects the insert against a uuid[] column — at runtime, on the one path
	// that carries images.
	query := fmt.Sprintf(`
		INSERT INTO submissions (type, target_exercise_id, locale, payload, submitted_by_app, app_user_ref, image_upload_ids)
		VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7::uuid[], '{}'))
		RETURNING %s`, submissionColumns)

	sub, err := scanSubmission(s.pool.QueryRow(ctx, query,
		in.Type, in.TargetExerciseID, in.Locale, []byte(in.Payload), in.AppID, in.AppUserRef, in.ImageUploadIDs,
	))
	if err != nil {
		return model.Submission{}, fmt.Errorf("create submission for app %s: %w", in.AppID, err)
	}
	return sub, nil
}

// ListByApp returns an app's own submissions, newest first, optionally
// narrowed to one of its users.
//
// Scoped to the calling app by construction — appID is a parameter, not a
// filter the caller may omit — because this backs GET /submissions/mine, and
// an app must never be able to page through another app's queue by leaving a
// filter off.
func (s *SubmissionStore) ListByApp(ctx context.Context, appID string, appUserRef *string, limit, offset int) ([]model.Submission, int, error) {
	var total int
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM submissions
		WHERE submitted_by_app = $1 AND ($2::text IS NULL OR app_user_ref = $2)`,
		appID, appUserRef,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count submissions for app %s: %w", appID, err)
	}

	query := fmt.Sprintf(`
		SELECT %s FROM submissions
		WHERE submitted_by_app = $1 AND ($2::text IS NULL OR app_user_ref = $2)
		ORDER BY created_at DESC, id
		LIMIT $3 OFFSET $4`, submissionColumns)

	rows, err := s.pool.Query(ctx, query, appID, appUserRef, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list submissions for app %s: %w", appID, err)
	}
	defer rows.Close()

	// Empty rather than nil: encoding/json renders a nil slice as `null` and
	// an empty one as `[]`, and this is a list every consumer will iterate.
	out := make([]model.Submission, 0, limit)
	for rows.Next() {
		sub, err := scanSubmission(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan submission: %w", err)
		}
		out = append(out, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list submissions for app %s: %w", appID, err)
	}
	return out, total, nil
}
