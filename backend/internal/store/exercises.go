package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Cawlumm/open-exercise-db/internal/model"
)

// ExerciseStore holds every query touching exercises and exercise_revisions.
type ExerciseStore struct {
	pool *pgxpool.Pool
}

// defaultPerPage is used when a caller passes PerPage <= 0. It does not cap
// the upper bound — that's a validation concern owned by the huma request
// struct's `maximum:"100"` tag in WO-3, not the store.
const defaultPerPage = 50

// ExerciseFilter narrows List's results. Every field except Q and the
// pagination fields is an exact match against a taxonomy slug. A zero-value
// field means "no filter" for that dimension.
type ExerciseFilter struct {
	Q           string // case-insensitive substring match against name + aliases
	MuscleGroup string
	Equipment   string
	Category    string
	Mechanic    string
	Level       string
	Page        int
	PerPage     int
}

// exerciseRevisionColumns is the explicit column list shared by List and
// Get — both join exercises to their current_revision_id row. No text casts
// on the UUID columns: pgx v5 scans uuid/uuid[] into string/[]string
// natively.
const exerciseRevisionColumns = `
	e.id, e.slug, e.source_id, e.current_revision_id, e.status, e.created_at, e.updated_at,
	r.id, r.exercise_id, r.name, r.aliases, r.muscle_group, r.secondary_muscles, r.category,
	r.equipment, r.mechanic, r.force, r.level, r.instructions, r.image_ids, r.video_ids,
	r.license, r.attribution_text, r.source_url, r.submitted_by, r.reviewed_by,
	r.change_summary, r.status, r.created_at`

// List returns published exercises joined to their current revision,
// filtered per ExerciseFilter, alongside the total matching row count
// (ignoring pagination) for the response envelope.
func (s *ExerciseStore) List(ctx context.Context, filter ExerciseFilter) ([]model.Exercise, int, error) {
	where, args := buildExerciseWhere(filter)

	total, err := s.countWhere(ctx, where, args)
	if err != nil {
		return nil, 0, fmt.Errorf("count exercises: %w", err)
	}

	page := filter.Page
	if page < 1 {
		page = 1
	}
	perPage := filter.PerPage
	if perPage < 1 {
		perPage = defaultPerPage
	}
	offset := (page - 1) * perPage

	listArgs := append(append([]any{}, args...), perPage, offset)
	query := fmt.Sprintf(`
		SELECT %s
		FROM exercises e
		JOIN exercise_revisions r ON r.id = e.current_revision_id
		WHERE %s
		ORDER BY r.name ASC, e.id ASC
		LIMIT $%d OFFSET $%d`, exerciseRevisionColumns, where, len(args)+1, len(args)+2)

	rows, err := s.pool.Query(ctx, query, listArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("list exercises: %w", err)
	}
	defer rows.Close()

	var exercises []model.Exercise
	for rows.Next() {
		ex, err := scanExerciseWithRevision(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan exercise: %w", err)
		}
		exercises = append(exercises, ex)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list exercises: %w", err)
	}

	return exercises, total, nil
}

// ListAll returns every published exercise joined to its current revision,
// with no pagination — for internal/export's full-catalog blob build
// (docs/PHASE1_BUILD_PLAN.md WO-5), not a public list endpoint. GET
// /exercises stays paginated via List because per-request cost matters
// there; ListAll exists precisely because the export blob is built once and
// cached rather than assembled per request, so a single unpaginated query is
// both simpler and safer than driving List's LIMIT/OFFSET in a loop (which
// risks skipping or duplicating rows across pages under concurrent writes,
// since each page would be its own snapshot).
func (s *ExerciseStore) ListAll(ctx context.Context) ([]model.Exercise, error) {
	query := fmt.Sprintf(`
		SELECT %s
		FROM exercises e
		JOIN exercise_revisions r ON r.id = e.current_revision_id
		WHERE e.status = 'published'
		ORDER BY r.name ASC, e.id ASC`, exerciseRevisionColumns)

	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list all exercises: %w", err)
	}
	defer rows.Close()

	var exercises []model.Exercise
	for rows.Next() {
		ex, err := scanExerciseWithRevision(rows)
		if err != nil {
			return nil, fmt.Errorf("scan exercise: %w", err)
		}
		exercises = append(exercises, ex)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list all exercises: %w", err)
	}

	return exercises, nil
}

// Get returns the published exercise identified by id, joined to its
// current revision. It returns pgx.ErrNoRows when there's no match — the
// store has no HTTP awareness (docs/CONVENTIONS.md section 4); mapping that
// to a 404 is the API layer's job (WO-3).
func (s *ExerciseStore) Get(ctx context.Context, id string) (model.Exercise, error) {
	query := fmt.Sprintf(`
		SELECT %s
		FROM exercises e
		JOIN exercise_revisions r ON r.id = e.current_revision_id
		WHERE e.id = $1 AND e.status = 'published'`, exerciseRevisionColumns)

	ex, err := scanExerciseWithRevision(s.pool.QueryRow(ctx, query, id))
	if err != nil {
		return model.Exercise{}, err
	}
	return ex, nil
}

// GetBySourceID returns the exercise (any status, current revision joined)
// whose exercises.source_id matches sourceID, or pgx.ErrNoRows if none has
// imported yet. Unlike Get, this deliberately does not filter
// status='published' — it's a write-path lookup for the importer
// (internal/seed), which needs to find its own previously-imported rows
// regardless of status, not a public read path.
func (s *ExerciseStore) GetBySourceID(ctx context.Context, sourceID string) (model.Exercise, error) {
	query := fmt.Sprintf(`
		SELECT %s
		FROM exercises e
		JOIN exercise_revisions r ON r.id = e.current_revision_id
		WHERE e.source_id = $1`, exerciseRevisionColumns)

	ex, err := scanExerciseWithRevision(s.pool.QueryRow(ctx, query, sourceID))
	if err != nil {
		return model.Exercise{}, err
	}
	return ex, nil
}

// SlugExists reports whether slug is already used by any exercise,
// regardless of status. Used by the importer (internal/seed) to dedupe a
// name-derived slug with a numeric suffix on collision
// (docs/PHASE1_BUILD_PLAN.md WO-4).
func (s *ExerciseStore) SlugExists(ctx context.Context, slug string) (bool, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM exercises WHERE slug = $1)`, slug).Scan(&exists); err != nil {
		return false, fmt.Errorf("check slug %s exists: %w", slug, err)
	}
	return exists, nil
}

// revisionColumns is the explicit column list for a bare exercise_revisions
// row (no join), used by ListRevisions.
const revisionColumns = `
	id, exercise_id, name, aliases, muscle_group, secondary_muscles, category, equipment,
	mechanic, force, level, instructions, image_ids, video_ids, license, attribution_text,
	source_url, submitted_by, reviewed_by, change_summary, status, created_at`

// CountRevisions returns how many revisions exerciseID has, ignoring
// pagination — the Total a paginated ListRevisions response reports.
func (s *ExerciseStore) CountRevisions(ctx context.Context, exerciseID string) (int, error) {
	var total int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM exercise_revisions WHERE exercise_id = $1`, exerciseID).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("count revisions for exercise %s: %w", exerciseID, err)
	}
	return total, nil
}

// ListRevisions returns one page of exerciseID's revisions, newest first.
// Revision history is append-only and never deleted, so this includes
// superseded revisions as well as the current one.
//
// Paginated at the SQL layer rather than sliced in the handler because this
// is the one collection in the API with genuinely unbounded growth: every
// approved edit appends a row that is never removed, so a heavily-edited
// exercise's history grows without limit for as long as the project runs.
// Slicing after the fact would still load the whole history into memory on
// every request to page 1.
//
// Ordered by created_at DESC with id as a tiebreak: two revisions written in
// the same transaction can share a created_at, and without a deterministic
// second key Postgres is free to order them differently between the query for
// page 1 and the query for page 2, silently dropping or duplicating a row
// across the page boundary.
func (s *ExerciseStore) ListRevisions(ctx context.Context, exerciseID string, limit, offset int) ([]model.Revision, error) {
	query := fmt.Sprintf(`
		SELECT %s
		FROM exercise_revisions
		WHERE exercise_id = $1
		ORDER BY created_at DESC, id DESC
		LIMIT $2 OFFSET $3`, revisionColumns)

	rows, err := s.pool.Query(ctx, query, exerciseID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list revisions for exercise %s: %w", exerciseID, err)
	}
	defer rows.Close()

	var revisions []model.Revision
	for rows.Next() {
		rev, err := scanRevision(rows)
		if err != nil {
			return nil, fmt.Errorf("scan revision: %w", err)
		}
		revisions = append(revisions, rev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list revisions for exercise %s: %w", exerciseID, err)
	}

	return revisions, nil
}

func (s *ExerciseStore) countWhere(ctx context.Context, where string, args []any) (int, error) {
	query := fmt.Sprintf(`
		SELECT count(*)
		FROM exercises e
		JOIN exercise_revisions r ON r.id = e.current_revision_id
		WHERE %s`, where)

	var total int
	if err := s.pool.QueryRow(ctx, query, args...).Scan(&total); err != nil {
		return 0, err
	}
	return total, nil
}

// buildExerciseWhere assembles the WHERE clause and its positional args
// shared by List and its count query. It always includes
// status='published' per docs/PHASE1_BUILD_PLAN.md WO-2: nothing
// unpublished is visible through this store method.
func buildExerciseWhere(filter ExerciseFilter) (string, []any) {
	conditions := []string{"e.status = 'published'"}
	var args []any

	addEq := func(column, value string) {
		if value == "" {
			return
		}
		args = append(args, value)
		conditions = append(conditions, fmt.Sprintf("%s = $%d", column, len(args)))
	}

	if filter.Q != "" {
		args = append(args, "%"+filter.Q+"%")
		n := len(args)
		conditions = append(conditions, fmt.Sprintf(
			"(r.name ILIKE $%d OR EXISTS (SELECT 1 FROM unnest(r.aliases) AS alias WHERE alias ILIKE $%d))",
			n, n,
		))
	}
	addEq("r.muscle_group", filter.MuscleGroup)
	addEq("r.equipment", filter.Equipment)
	addEq("r.category", filter.Category)
	addEq("r.mechanic", filter.Mechanic)
	addEq("r.level", filter.Level)

	return strings.Join(conditions, " AND "), args
}

func scanExerciseWithRevision(row rowScanner) (model.Exercise, error) {
	var (
		ex  model.Exercise
		rev model.Revision
	)

	if err := row.Scan(
		&ex.ID, &ex.Slug, &ex.SourceID, &ex.CurrentRevisionID, &ex.Status, &ex.CreatedAt, &ex.UpdatedAt,
		&rev.ID, &rev.ExerciseID, &rev.Name, &rev.Aliases, &rev.MuscleGroup, &rev.SecondaryMuscles, &rev.Category,
		&rev.Equipment, &rev.Mechanic, &rev.Force, &rev.Level, &rev.Instructions, &rev.ImageIDs, &rev.VideoIDs,
		&rev.License, &rev.AttributionText, &rev.SourceURL, &rev.SubmittedBy, &rev.ReviewedBy,
		&rev.ChangeSummary, &rev.Status, &rev.CreatedAt,
	); err != nil {
		return model.Exercise{}, err
	}

	ex.CurrentRevision = &rev
	return ex, nil
}

func scanRevision(row rowScanner) (model.Revision, error) {
	var rev model.Revision
	if err := row.Scan(
		&rev.ID, &rev.ExerciseID, &rev.Name, &rev.Aliases, &rev.MuscleGroup, &rev.SecondaryMuscles, &rev.Category,
		&rev.Equipment, &rev.Mechanic, &rev.Force, &rev.Level, &rev.Instructions, &rev.ImageIDs, &rev.VideoIDs,
		&rev.License, &rev.AttributionText, &rev.SourceURL, &rev.SubmittedBy, &rev.ReviewedBy,
		&rev.ChangeSummary, &rev.Status, &rev.CreatedAt,
	); err != nil {
		return model.Revision{}, err
	}
	return rev, nil
}

// --- Import write path (docs/PHASE1_BUILD_PLAN.md WO-4) ---
//
// This is the first write path in internal/store — WO-2 built read paths
// only. UpsertImport is the one entry point the free-exercise-db importer
// (internal/seed) calls per source item; it owns the create-vs-update
// decision, the content diff that makes re-imports idempotent, and the
// exercise+revision+pointer transaction, so that logic lives in one place
// next to the SQL it depends on rather than being split across package
// boundaries.

// ImportRevisionInput is the trusted-import content for one exercise's
// revision — every field UpsertImport compares against the current revision
// to decide whether a new revision is needed, and (if so) writes verbatim.
type ImportRevisionInput struct {
	Name             string
	Aliases          []string
	MuscleGroup      string
	SecondaryMuscles []string
	Category         string
	Equipment        string
	Mechanic         *string
	Force            *string
	Level            *string
	Instructions     []string
	ImageIDs         []string
	License          *string
	AttributionText  *string
	SourceURL        *string
	ChangeSummary    *string
}

// UpsertImportInput is UpsertImport's full input: which upstream item
// (SourceID), the slug to use *if* this turns out to be a brand new
// exercise (ignored on the update path, where the exercise's existing slug
// is kept), and the candidate revision content.
type UpsertImportInput struct {
	SourceID string
	Slug     string
	Revision ImportRevisionInput
	// DryRun makes UpsertImport report what it would do (create/update/
	// leave unchanged) using only read-only queries — no BEGIN, no INSERT,
	// no UPDATE. Per docs/PHASE1_BUILD_PLAN.md WO-4's --dry-run
	// requirement: "report only, write nothing."
	DryRun bool
}

// ImportResult reports what UpsertImport actually did (or, in DryRun mode,
// would do), so the importer can total create/update/unchanged counts.
type ImportResult struct {
	ExerciseID string
	RevisionID string // the current revision id after this call
	Created    bool   // true if this call created the exercises row
	// RevisionAdded is true if a new revision was inserted — always true
	// alongside Created (a brand new exercise needs a first revision), and
	// true on the update path only when the incoming content actually
	// differs from the current revision.
	RevisionAdded bool
}

// UpsertImport creates or updates exerciseID's published data from a
// trusted-import source, keyed on exercises.source_id (never on slug or
// name — DESIGN.md's "Migrating a consumer" section is exactly the failure
// mode a name-keyed upsert has). Idempotent: re-running with unchanged
// content neither duplicates the exercise nor inserts a new revision.
func (s *ExerciseStore) UpsertImport(ctx context.Context, in UpsertImportInput) (ImportResult, error) {
	if in.DryRun {
		return s.previewImport(ctx, in)
	}
	return s.applyImport(ctx, in)
}

// txQuerier is satisfied by both *pgxpool.Pool and pgx.Tx, so loadCurrent
// can run the same lookup either as a plain pooled query (previewImport,
// which never writes) or inside a transaction (applyImport, which needs a
// consistent read of "does this exist, and what's its current content"
// before deciding what to write).
type txQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// loadCurrent looks up the exercise for sourceID and, if found, its current
// revision's importable content. found=false with err=nil means "no
// exercise imported from this source yet" — not an error condition, since
// that's the expected first-import case for most of an 800-item dataset run.
func loadCurrent(ctx context.Context, q txQuerier, sourceID string) (exerciseID, slug string, currentRevisionID *string, current model.Revision, found bool, err error) {
	err = q.QueryRow(ctx,
		`SELECT id, slug, current_revision_id FROM exercises WHERE source_id = $1`, sourceID,
	).Scan(&exerciseID, &slug, &currentRevisionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", nil, model.Revision{}, false, nil
		}
		return "", "", nil, model.Revision{}, false, fmt.Errorf("look up exercise for source %s: %w", sourceID, err)
	}
	if currentRevisionID == nil {
		return "", "", nil, model.Revision{}, false, fmt.Errorf("exercise %s (source %s) has no current_revision_id", exerciseID, sourceID)
	}

	err = q.QueryRow(ctx, `
		SELECT name, aliases, muscle_group, secondary_muscles, category, equipment, mechanic, force, level,
		       instructions, image_ids, license, attribution_text, source_url
		FROM exercise_revisions WHERE id = $1`, *currentRevisionID,
	).Scan(
		&current.Name, &current.Aliases, &current.MuscleGroup, &current.SecondaryMuscles, &current.Category,
		&current.Equipment, &current.Mechanic, &current.Force, &current.Level, &current.Instructions,
		&current.ImageIDs, &current.License, &current.AttributionText, &current.SourceURL,
	)
	if err != nil {
		return "", "", nil, model.Revision{}, false, fmt.Errorf("load current revision %s: %w", *currentRevisionID, err)
	}

	return exerciseID, slug, currentRevisionID, current, true, nil
}

// revisionUnchanged reports whether in's content is identical to current's —
// the idempotency check the whole importer depends on. It's a plain
// field-by-field comparison of the exact values that round-tripped through
// Postgres against the exact values about to be written, via
// reflect.DeepEqual (which, for the *string fields here, compares the
// pointed-to value rather than the pointer address — nil-vs-nil and
// "x"-vs-"x" both count as equal regardless of allocation). Because both
// sides are real fully-materialized content (not summaries or hashes), a
// false "unchanged" would require the incoming and stored values to
// actually be byte-identical, which is exactly the condition that should
// suppress a new revision.
//
// Deliberately NOT included: ID/ExerciseID/SubmittedBy/ReviewedBy/Status/
// CreatedAt — those are row identity/audit metadata, not import content.
func revisionUnchanged(current model.Revision, in ImportRevisionInput) bool {
	return current.Name == in.Name &&
		reflect.DeepEqual(current.Aliases, in.Aliases) &&
		current.MuscleGroup == in.MuscleGroup &&
		reflect.DeepEqual(current.SecondaryMuscles, in.SecondaryMuscles) &&
		current.Category == in.Category &&
		current.Equipment == in.Equipment &&
		reflect.DeepEqual(current.Mechanic, in.Mechanic) &&
		reflect.DeepEqual(current.Force, in.Force) &&
		reflect.DeepEqual(current.Level, in.Level) &&
		reflect.DeepEqual(current.Instructions, in.Instructions) &&
		reflect.DeepEqual(current.ImageIDs, in.ImageIDs) &&
		reflect.DeepEqual(current.License, in.License) &&
		reflect.DeepEqual(current.AttributionText, in.AttributionText) &&
		reflect.DeepEqual(current.SourceURL, in.SourceURL)
}

// previewImport is UpsertImport's DryRun path: read-only, no transaction,
// no writes of any kind.
func (s *ExerciseStore) previewImport(ctx context.Context, in UpsertImportInput) (ImportResult, error) {
	exerciseID, _, currentRevisionID, current, found, err := loadCurrent(ctx, s.pool, in.SourceID)
	if err != nil {
		return ImportResult{}, err
	}
	if !found {
		return ImportResult{Created: true, RevisionAdded: true}, nil
	}

	result := ImportResult{ExerciseID: exerciseID, RevisionAdded: !revisionUnchanged(current, in.Revision)}
	if currentRevisionID != nil {
		result.RevisionID = *currentRevisionID
	}
	return result, nil
}

// applyImport is UpsertImport's real write path: a single transaction that
// either (a) inserts a brand new exercise + its first revision + repoints
// current_revision_id, or (b), for an existing exercise whose content
// actually changed, supersedes the old current revision, inserts the new
// one, and repoints current_revision_id — or (c) does nothing at all when
// the content is unchanged. Per docs/CONVENTIONS.md section 4 and the
// schema's DEFERRABLE INITIALLY DEFERRED current_revision_id FK
// (docs/PHASE1_BUILD_PLAN.md WO-1), this whole sequence is one transaction:
// a crash between inserting the revision and repointing the pointer must
// not leave a published exercise pointing at nothing.
func (s *ExerciseStore) applyImport(ctx context.Context, in UpsertImportInput) (ImportResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ImportResult{}, fmt.Errorf("begin import tx for source %s: %w", in.SourceID, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	exerciseID, _, currentRevisionID, current, found, err := loadCurrent(ctx, tx, in.SourceID)
	if err != nil {
		return ImportResult{}, err
	}

	if !found {
		if err := tx.QueryRow(ctx,
			`INSERT INTO exercises (slug, source_id, status) VALUES ($1, $2, 'published') RETURNING id`,
			in.Slug, in.SourceID,
		).Scan(&exerciseID); err != nil {
			return ImportResult{}, fmt.Errorf("insert exercise for source %s: %w", in.SourceID, err)
		}

		revisionID, err := insertRevision(ctx, tx, exerciseID, in.Revision)
		if err != nil {
			return ImportResult{}, err
		}

		if _, err := tx.Exec(ctx,
			`UPDATE exercises SET current_revision_id = $1, updated_at = now() WHERE id = $2`,
			revisionID, exerciseID,
		); err != nil {
			return ImportResult{}, fmt.Errorf("point exercise %s at its first revision: %w", exerciseID, err)
		}

		if err := tx.Commit(ctx); err != nil {
			return ImportResult{}, fmt.Errorf("commit import for source %s: %w", in.SourceID, err)
		}
		return ImportResult{ExerciseID: exerciseID, RevisionID: revisionID, Created: true, RevisionAdded: true}, nil
	}

	if revisionUnchanged(current, in.Revision) {
		// Nothing to write. Still commit (not rollback) to cleanly release
		// the transaction's snapshot/locks rather than leaving semantics
		// ambiguous about whether the read-only work "succeeded".
		if err := tx.Commit(ctx); err != nil {
			return ImportResult{}, fmt.Errorf("commit no-op import for source %s: %w", in.SourceID, err)
		}
		result := ImportResult{ExerciseID: exerciseID, RevisionAdded: false}
		if currentRevisionID != nil {
			result.RevisionID = *currentRevisionID
		}
		return result, nil
	}

	if _, err := tx.Exec(ctx,
		`UPDATE exercise_revisions SET status = 'superseded' WHERE id = $1`, *currentRevisionID,
	); err != nil {
		return ImportResult{}, fmt.Errorf("supersede revision %s: %w", *currentRevisionID, err)
	}

	revisionID, err := insertRevision(ctx, tx, exerciseID, in.Revision)
	if err != nil {
		return ImportResult{}, err
	}

	if _, err := tx.Exec(ctx,
		`UPDATE exercises SET current_revision_id = $1, updated_at = now() WHERE id = $2`,
		revisionID, exerciseID,
	); err != nil {
		return ImportResult{}, fmt.Errorf("repoint exercise %s at new revision: %w", exerciseID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return ImportResult{}, fmt.Errorf("commit updated import for source %s: %w", in.SourceID, err)
	}
	return ImportResult{ExerciseID: exerciseID, RevisionID: revisionID, Created: false, RevisionAdded: true}, nil
}

// insertRevision writes one exercise_revisions row for exerciseID inside tx
// and returns its id. submitted_by/reviewed_by are always NULL for a
// trusted import (docs/DESIGN.md's Bootstrap data section), status is
// always 'approved' — this bypasses the submissions queue entirely, it
// doesn't go through it and get approved, so 'approved' is the correct
// terminal status from the moment it's written.
func insertRevision(ctx context.Context, tx pgx.Tx, exerciseID string, in ImportRevisionInput) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO exercise_revisions
			(exercise_id, name, aliases, muscle_group, secondary_muscles, category, equipment,
			 mechanic, force, level, instructions, image_ids, license, attribution_text, source_url,
			 submitted_by, reviewed_by, change_summary, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, NULL, NULL, $16, 'approved')
		RETURNING id`,
		exerciseID, in.Name, in.Aliases, in.MuscleGroup, in.SecondaryMuscles, in.Category, in.Equipment,
		in.Mechanic, in.Force, in.Level, in.Instructions, in.ImageIDs, in.License, in.AttributionText, in.SourceURL,
		in.ChangeSummary,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert revision for exercise %s: %w", exerciseID, err)
	}
	return id, nil
}
