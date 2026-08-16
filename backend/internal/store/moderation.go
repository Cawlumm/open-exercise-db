package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Cawlumm/open-exercise-db/internal/model"
)

// Moderation errors the API layer maps onto specific statuses. Sentinels
// rather than message text, because the alternative is an API that turns
// "already reviewed" into a 500 the first time someone rewords a string.
var (
	// ErrNoSuchSubmission is a decision aimed at a submission that does not
	// exist.
	ErrNoSuchSubmission = errors.New("no such submission")

	// ErrAlreadyDecided is a decision aimed at a submission that is not
	// pending. Deliberately distinct from success: a moderator who approves
	// the same row twice — a double-click, a retried request, two people in
	// the queue at once — must be told the second one did nothing, not left
	// believing they published something.
	ErrAlreadyDecided = errors.New("submission has already been reviewed")

	// ErrRevisionNotOnExercise is a revert naming a revision belonging to a
	// different exercise. Checked rather than trusted: repointing an
	// exercise at another exercise's revision would silently replace its
	// entire content with unrelated data, and nothing downstream would
	// notice because both ids are valid.
	ErrRevisionNotOnExercise = errors.New("revision does not belong to that exercise")
)

// ModerationStore holds the queue reads and the writes that publish.
type ModerationStore struct {
	pool *pgxpool.Pool
}

// SubmissionFilter narrows the moderation queue.
type SubmissionFilter struct {
	Status string // "" means every status
	Type   string // "" means every type
	AppID  string // "" means every source
	Limit  int
	Offset int
}

// ListQueue returns submissions matching filter, oldest first.
//
// Oldest first, unlike an app's own list: a queue is worked through, and the
// thing most at risk of being forgotten is the submission that has waited
// longest. Newest-first would bury it.
func (s *ModerationStore) ListQueue(ctx context.Context, f SubmissionFilter) ([]model.Submission, int, error) {
	where := `WHERE ($1 = '' OR status = $1) AND ($2 = '' OR type = $2) AND ($3 = '' OR submitted_by_app::text = $3)`
	args := []any{f.Status, f.Type, f.AppID}

	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM submissions `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count queue: %w", err)
	}

	query := fmt.Sprintf(`SELECT %s FROM submissions %s ORDER BY created_at ASC, id LIMIT $4 OFFSET $5`,
		submissionColumns, where)
	rows, err := s.pool.Query(ctx, query, f.Status, f.Type, f.AppID, f.Limit, f.Offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list queue: %w", err)
	}
	defer rows.Close()

	out := make([]model.Submission, 0, f.Limit)
	for rows.Next() {
		sub, err := scanSubmission(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan queued submission: %w", err)
		}
		out = append(out, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list queue: %w", err)
	}
	return out, total, nil
}

// GetSubmission returns one queued submission, or pgx.ErrNoRows.
func (s *ModerationStore) GetSubmission(ctx context.Context, id string) (model.Submission, error) {
	query := fmt.Sprintf(`SELECT %s FROM submissions WHERE id = $1`, submissionColumns)
	return scanSubmission(s.pool.QueryRow(ctx, query, id))
}

// Decision is what a moderator records.
type Decision struct {
	SubmissionID string
	ReviewerID   string
	Notes        *string
}

// Reject closes a submission without publishing anything.
func (s *ModerationStore) Reject(ctx context.Context, d Decision) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE submissions
		SET status = 'rejected', reviewer_id = $2, reviewer_notes = $3, updated_at = now()
		WHERE id = $1 AND status = 'pending'`,
		d.SubmissionID, d.ReviewerID, d.Notes)
	if err != nil {
		return fmt.Errorf("reject submission %s: %w", d.SubmissionID, err)
	}
	if tag.RowsAffected() == 0 {
		// Zero rows means the id is unknown or it was already decided. One
		// extra read to tell the moderator which, because "nothing happened"
		// is the least actionable thing an interface can say.
		return s.explainNoDecision(ctx, d.SubmissionID)
	}
	return nil
}

// explainNoDecision distinguishes "no such submission" from "already
// reviewed" after a conditional update matched nothing.
func (s *ModerationStore) explainNoDecision(ctx context.Context, id string) error {
	var status string
	err := s.pool.QueryRow(ctx, `SELECT status FROM submissions WHERE id = $1`, id).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoSuchSubmission
	}
	if err != nil {
		return fmt.Errorf("explain non-decision for %s: %w", id, err)
	}
	return fmt.Errorf("%w (status %s)", ErrAlreadyDecided, status)
}

// ApproveResult reports what publishing a submission actually produced.
type ApproveResult struct {
	ExerciseID string
	RevisionID string
	Created    bool // a brand new exercise rather than a new revision
}

// Approve materializes a submission into the published tables.
//
// The whole thing runs in one transaction, and the submission row is locked
// FOR UPDATE before anything is written. Two moderators clicking approve on
// the same row at the same moment would otherwise both pass the status check
// and both publish, producing two revisions of the same change with no
// indication anything went wrong. The lock makes the second one wait and
// then find the row no longer pending.
func (s *ModerationStore) Approve(ctx context.Context, d Decision) (ApproveResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ApproveResult{}, fmt.Errorf("begin approve tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	var sub model.Submission
	var payload []byte
	err = tx.QueryRow(ctx, `
		SELECT id, type, target_exercise_id, locale, payload, image_upload_ids, status
		FROM submissions WHERE id = $1 FOR UPDATE`, d.SubmissionID,
	).Scan(&sub.ID, &sub.Type, &sub.TargetExerciseID, &sub.Locale, &payload, &sub.ImageUploadIDs, &sub.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ApproveResult{}, ErrNoSuchSubmission
	}
	if err != nil {
		return ApproveResult{}, fmt.Errorf("load submission %s: %w", d.SubmissionID, err)
	}
	if sub.Status != model.SubmissionStatusPending {
		return ApproveResult{}, fmt.Errorf("%w (status %s)", ErrAlreadyDecided, sub.Status)
	}

	var result ApproveResult
	switch sub.Type {
	case model.SubmissionTypeCreate:
		result, err = approveCreate(ctx, tx, payload, sub.ImageUploadIDs)
	case model.SubmissionTypeEdit:
		result, err = approveEdit(ctx, tx, sub, payload)
	case model.SubmissionTypeTranslation:
		result, err = approveTranslation(ctx, tx, sub, payload)
	default:
		err = fmt.Errorf("unknown submission type %q", sub.Type)
	}
	if err != nil {
		return ApproveResult{}, err
	}

	// Conditional on still being pending, and the row count is checked.
	// The FOR UPDATE above serialises concurrent approvals, but relying on
	// that alone makes correctness a property of lock timing — and a test
	// that removes the lock still passed, because goroutines do not reliably
	// interleave inside the window it protects. This makes single-publication
	// an invariant the database enforces on every path: if another
	// transaction decided this row first, zero rows match and the whole
	// transaction rolls back, including anything already written to the
	// published tables.
	tag, err := tx.Exec(ctx, `
		UPDATE submissions
		SET status = 'approved', reviewer_id = $2, reviewer_notes = $3, updated_at = now()
		WHERE id = $1 AND status = 'pending'`, d.SubmissionID, d.ReviewerID, d.Notes)
	if err != nil {
		return ApproveResult{}, fmt.Errorf("mark submission %s approved: %w", d.SubmissionID, err)
	}
	if tag.RowsAffected() == 0 {
		return ApproveResult{}, fmt.Errorf("%w (decided concurrently)", ErrAlreadyDecided)
	}

	if err := tx.Commit(ctx); err != nil {
		return ApproveResult{}, fmt.Errorf("commit approval of %s: %w", d.SubmissionID, err)
	}
	return result, nil
}

// submissionPayload is the content a submission proposes.
//
// Parsed here rather than at submit time because the queue must survive a
// later change to the exercise schema: a submission accepted last month is
// still readable even if the shape has since gained a field, and one that has
// become unreadable fails at approval — visibly, to a moderator — rather than
// having been rejected silently on arrival.
type submissionPayload struct {
	Name             string   `json:"name"`
	Aliases          []string `json:"aliases"`
	MuscleGroup      string   `json:"muscle_group"`
	SecondaryMuscles []string `json:"secondary_muscles"`
	Category         string   `json:"category"`
	Equipment        string   `json:"equipment"`
	Mechanic         *string  `json:"mechanic"`
	Force            *string  `json:"force"`
	Level            *string  `json:"level"`
	Instructions     []string `json:"instructions"`
	License          *string  `json:"license"`
	AttributionText  *string  `json:"attribution_text"`
	SourceURL        *string  `json:"source_url"`
	ChangeSummary    *string  `json:"change_summary"`
}

func (p submissionPayload) toRevision(imageIDs []string) ImportRevisionInput {
	return ImportRevisionInput{
		Name:             p.Name,
		Aliases:          orEmpty(p.Aliases),
		MuscleGroup:      p.MuscleGroup,
		SecondaryMuscles: orEmpty(p.SecondaryMuscles),
		Category:         p.Category,
		Equipment:        p.Equipment,
		Mechanic:         p.Mechanic,
		Force:            p.Force,
		Level:            p.Level,
		Instructions:     orEmpty(p.Instructions),
		ImageIDs:         orEmpty(imageIDs),
		License:          p.License,
		AttributionText:  p.AttributionText,
		SourceURL:        p.SourceURL,
		ChangeSummary:    p.ChangeSummary,
	}
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func parsePayload(raw []byte) (submissionPayload, error) {
	var p submissionPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return submissionPayload{}, fmt.Errorf("payload is not usable: %w", err)
	}
	if strings.TrimSpace(p.Name) == "" {
		return submissionPayload{}, errors.New("payload has no name")
	}
	return p, nil
}

func approveCreate(ctx context.Context, tx pgx.Tx, payload []byte, imageIDs []string) (ApproveResult, error) {
	p, err := parsePayload(payload)
	if err != nil {
		return ApproveResult{}, err
	}

	slug, err := uniqueSlug(ctx, tx, slugFromName(p.Name))
	if err != nil {
		return ApproveResult{}, err
	}

	var exerciseID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO exercises (slug, status) VALUES ($1, 'published') RETURNING id`, slug,
	).Scan(&exerciseID); err != nil {
		return ApproveResult{}, fmt.Errorf("insert exercise %q: %w", slug, err)
	}

	revisionID, err := insertRevision(ctx, tx, exerciseID, p.toRevision(imageIDs))
	if err != nil {
		return ApproveResult{}, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE exercises SET current_revision_id = $1, updated_at = now() WHERE id = $2`,
		revisionID, exerciseID); err != nil {
		return ApproveResult{}, fmt.Errorf("point new exercise at its revision: %w", err)
	}
	return ApproveResult{ExerciseID: exerciseID, RevisionID: revisionID, Created: true}, nil
}

func approveEdit(ctx context.Context, tx pgx.Tx, sub model.Submission, payload []byte) (ApproveResult, error) {
	if sub.TargetExerciseID == nil {
		return ApproveResult{}, errors.New("edit submission has no target exercise")
	}
	p, err := parsePayload(payload)
	if err != nil {
		return ApproveResult{}, err
	}

	var currentRevisionID *string
	if err := tx.QueryRow(ctx,
		`SELECT current_revision_id FROM exercises WHERE id = $1 FOR UPDATE`, *sub.TargetExerciseID,
	).Scan(&currentRevisionID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ApproveResult{}, fmt.Errorf("target exercise %s no longer exists", *sub.TargetExerciseID)
		}
		return ApproveResult{}, fmt.Errorf("lock target exercise: %w", err)
	}

	if currentRevisionID != nil {
		if _, err := tx.Exec(ctx,
			`UPDATE exercise_revisions SET status = 'superseded' WHERE id = $1`, *currentRevisionID,
		); err != nil {
			return ApproveResult{}, fmt.Errorf("supersede current revision: %w", err)
		}
	}

	revisionID, err := insertRevision(ctx, tx, *sub.TargetExerciseID, p.toRevision(sub.ImageUploadIDs))
	if err != nil {
		return ApproveResult{}, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE exercises SET current_revision_id = $1, updated_at = now() WHERE id = $2`,
		revisionID, *sub.TargetExerciseID); err != nil {
		return ApproveResult{}, fmt.Errorf("repoint exercise at new revision: %w", err)
	}
	return ApproveResult{ExerciseID: *sub.TargetExerciseID, RevisionID: revisionID}, nil
}

func approveTranslation(ctx context.Context, tx pgx.Tx, sub model.Submission, payload []byte) (ApproveResult, error) {
	if sub.TargetExerciseID == nil || sub.Locale == nil {
		return ApproveResult{}, errors.New("translation submission is missing its target or locale")
	}
	p, err := parsePayload(payload)
	if err != nil {
		return ApproveResult{}, err
	}

	var revisionID string
	if err := tx.QueryRow(ctx,
		`SELECT current_revision_id FROM exercises WHERE id = $1`, *sub.TargetExerciseID,
	).Scan(&revisionID); err != nil {
		return ApproveResult{}, fmt.Errorf("find current revision to translate: %w", err)
	}

	instructions, err := json.Marshal(orEmpty(p.Instructions))
	if err != nil {
		return ApproveResult{}, fmt.Errorf("encode translated instructions: %w", err)
	}

	// A previously approved translation for the same (revision, locale) is
	// superseded rather than replaced, so the history of what a locale used
	// to say survives — the same guarantee the revision chain gives English.
	if _, err := tx.Exec(ctx, `
		UPDATE exercise_translations SET status = 'superseded'
		WHERE revision_id = $1 AND locale = $2 AND status = 'approved'`,
		revisionID, *sub.Locale); err != nil {
		return ApproveResult{}, fmt.Errorf("supersede previous translation: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO exercise_translations
			(revision_id, locale, name, aliases, instructions, translated_by, origin, status)
		VALUES ($1, $2, $3, $4, $5, NULL, 'human', 'approved')`,
		revisionID, *sub.Locale, p.Name, orEmpty(p.Aliases), instructions); err != nil {
		return ApproveResult{}, fmt.Errorf("insert translation: %w", err)
	}
	return ApproveResult{ExerciseID: *sub.TargetExerciseID, RevisionID: revisionID}, nil
}

// Revert repoints an exercise at an earlier revision.
//
// Nothing is mutated or deleted — a revert is one pointer write, which is the
// entire reason exercise_revisions is append-only. That also makes a revert
// itself revertible.
func (s *ModerationStore) Revert(ctx context.Context, exerciseID, revisionID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin revert tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	var owner string
	err = tx.QueryRow(ctx, `SELECT exercise_id FROM exercise_revisions WHERE id = $1`, revisionID).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoSuchSubmission
	}
	if err != nil {
		return fmt.Errorf("load revision %s: %w", revisionID, err)
	}
	if owner != exerciseID {
		// Without this check, reverting to another exercise's revision would
		// silently replace this exercise's entire content with unrelated
		// data, and both ids would look perfectly valid.
		return ErrRevisionNotOnExercise
	}

	if _, err := tx.Exec(ctx,
		`UPDATE exercises SET current_revision_id = $1, updated_at = now() WHERE id = $2`,
		revisionID, exerciseID); err != nil {
		return fmt.Errorf("revert exercise %s: %w", exerciseID, err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE exercise_revisions SET status = 'approved' WHERE id = $1`, revisionID); err != nil {
		return fmt.Errorf("restore revision status: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit revert of %s: %w", exerciseID, err)
	}
	return nil
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slugFromName(name string) string {
	s := nonSlug.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-")
	return strings.Trim(s, "-")
}

// uniqueSlug appends a numeric suffix until the slug is free.
//
// Inside the approval transaction rather than at submit time, because two
// submissions proposing the same name can sit in the queue simultaneously and
// only collide when the second is approved — possibly weeks later.
func uniqueSlug(ctx context.Context, tx pgx.Tx, base string) (string, error) {
	if base == "" {
		base = "exercise"
	}
	candidate := base
	for i := 2; i < 1000; i++ {
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM exercises WHERE slug = $1)`, candidate).Scan(&exists); err != nil {
			return "", fmt.Errorf("check slug %q: %w", candidate, err)
		}
		if !exists {
			return candidate, nil
		}
		candidate = fmt.Sprintf("%s-%d", base, i)
	}
	return "", fmt.Errorf("could not find a free slug for %q", base)
}
