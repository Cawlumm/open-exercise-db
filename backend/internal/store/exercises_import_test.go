package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

// baseImportInput returns a valid, self-consistent import for one fixed
// upstream source id, so tests only need to override what they're actually
// asserting on.
func baseImportInput(sourceID, slug string) UpsertImportInput {
	license := "Unlicense"
	sourceURL := "https://github.com/yuhonas/free-exercise-db"
	return UpsertImportInput{
		SourceID: sourceID,
		Slug:     slug,
		Revision: ImportRevisionInput{
			Name:             "Bench Press",
			Aliases:          []string{},
			MuscleGroup:      "chest",
			SecondaryMuscles: []string{"triceps"},
			Category:         "strength",
			Equipment:        "barbell",
			Instructions:     []string{"Lie on the bench.", "Press the bar up."},
			ImageIDs:         []string{},
			License:          &license,
			SourceURL:        &sourceURL,
		},
	}
}

func TestExerciseStore_UpsertImport_CreatesNewExercise(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	result, err := s.Exercises.UpsertImport(ctx, baseImportInput("Bench_Press", "bench-press"))
	if err != nil {
		t.Fatalf("UpsertImport: unexpected error: %v", err)
	}
	if !result.Created || !result.RevisionAdded {
		t.Fatalf("result = %+v, want Created=true RevisionAdded=true", result)
	}

	ex, err := s.Exercises.Get(ctx, result.ExerciseID)
	if err != nil {
		t.Fatalf("Get(%s): %v", result.ExerciseID, err)
	}
	if ex.Slug != "bench-press" {
		t.Errorf("Slug = %q, want bench-press", ex.Slug)
	}
	if ex.SourceID == nil || *ex.SourceID != "Bench_Press" {
		t.Errorf("SourceID = %v, want Bench_Press", ex.SourceID)
	}
	if ex.CurrentRevisionID == nil || *ex.CurrentRevisionID != result.RevisionID {
		t.Errorf("CurrentRevisionID = %v, want %s (must be set, not left dangling)", ex.CurrentRevisionID, result.RevisionID)
	}
	if ex.CurrentRevision == nil || ex.CurrentRevision.Name != "Bench Press" {
		t.Errorf("CurrentRevision = %+v, want Name=Bench Press", ex.CurrentRevision)
	}
	if ex.CurrentRevision.Status != "approved" {
		t.Errorf("CurrentRevision.Status = %q, want approved (trusted import bypasses the moderation queue)", ex.CurrentRevision.Status)
	}
	if got := revisionCount(t, ctx, pool, result.ExerciseID); got != 1 {
		t.Errorf("revision count = %d, want 1", got)
	}
}

// TestExerciseStore_UpsertImport_RerunWithUnchangedContentIsANoOp is the
// single most important acceptance criterion for WO-4 (per
// docs/PHASE1_BUILD_PLAN.md): re-running the importer must not duplicate
// the exercise and must not insert a new revision when nothing changed.
func TestExerciseStore_UpsertImport_RerunWithUnchangedContentIsANoOp(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	first, err := s.Exercises.UpsertImport(ctx, baseImportInput("Bench_Press", "bench-press"))
	if err != nil {
		t.Fatalf("first UpsertImport: %v", err)
	}

	second, err := s.Exercises.UpsertImport(ctx, baseImportInput("Bench_Press", "bench-press"))
	if err != nil {
		t.Fatalf("second UpsertImport: %v", err)
	}

	if second.Created {
		t.Error("second run: Created = true, want false (must not duplicate the exercise)")
	}
	if second.RevisionAdded {
		t.Error("second run: RevisionAdded = true, want false (content is identical)")
	}
	if second.ExerciseID != first.ExerciseID {
		t.Errorf("second run: ExerciseID = %s, want %s (same row)", second.ExerciseID, first.ExerciseID)
	}
	if second.RevisionID != first.RevisionID {
		t.Errorf("second run: RevisionID = %s, want %s (no new revision)", second.RevisionID, first.RevisionID)
	}

	if got := exerciseCountBySourceID(t, ctx, pool, "Bench_Press"); got != 1 {
		t.Errorf("exercise row count for source Bench_Press = %d, want 1 (no duplicate)", got)
	}
	if got := revisionCount(t, ctx, pool, first.ExerciseID); got != 1 {
		t.Errorf("revision count = %d, want 1 (no new revision)", got)
	}
}

// TestExerciseStore_UpsertImport_ChangedContentAddsRevisionAndRepoints
// covers the flip side of idempotency: a real upstream change must produce
// a new revision and move current_revision_id, and the superseded revision
// must survive (append-only history), not be deleted or mutated in place.
func TestExerciseStore_UpsertImport_ChangedContentAddsRevisionAndRepoints(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	first, err := s.Exercises.UpsertImport(ctx, baseImportInput("Bench_Press", "bench-press"))
	if err != nil {
		t.Fatalf("first UpsertImport: %v", err)
	}

	changed := baseImportInput("Bench_Press", "bench-press")
	changed.Revision.Instructions = []string{"Lie on the bench.", "Press the bar up.", "Lower with control."}

	second, err := s.Exercises.UpsertImport(ctx, changed)
	if err != nil {
		t.Fatalf("second UpsertImport: %v", err)
	}

	if second.Created {
		t.Error("Created = true on an update, want false")
	}
	if !second.RevisionAdded {
		t.Error("RevisionAdded = false, want true (content actually changed)")
	}
	if second.RevisionID == first.RevisionID {
		t.Error("RevisionID unchanged, want a new revision id")
	}

	ex, err := s.Exercises.Get(ctx, first.ExerciseID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ex.CurrentRevisionID == nil || *ex.CurrentRevisionID != second.RevisionID {
		t.Errorf("current_revision_id = %v, want repointed to %s", ex.CurrentRevisionID, second.RevisionID)
	}
	if len(ex.CurrentRevision.Instructions) != 3 {
		t.Errorf("current revision instructions = %v, want the 3-step version", ex.CurrentRevision.Instructions)
	}

	if got := revisionCount(t, ctx, pool, first.ExerciseID); got != 2 {
		t.Errorf("revision count = %d, want 2 (old revision retained, not deleted)", got)
	}

	oldStatus := revisionStatus(t, ctx, pool, first.RevisionID)
	if oldStatus != "superseded" {
		t.Errorf("old revision status = %q, want superseded", oldStatus)
	}
	newStatus := revisionStatus(t, ctx, pool, second.RevisionID)
	if newStatus != "approved" {
		t.Errorf("new revision status = %q, want approved", newStatus)
	}
}

func TestExerciseStore_UpsertImport_DryRunWritesNothing(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	in := baseImportInput("Bench_Press", "bench-press")
	in.DryRun = true

	result, err := s.Exercises.UpsertImport(ctx, in)
	if err != nil {
		t.Fatalf("UpsertImport (dry run): %v", err)
	}
	if !result.Created || !result.RevisionAdded {
		t.Errorf("dry-run preview = %+v, want Created=true RevisionAdded=true (reports what WOULD happen)", result)
	}

	if got := exerciseCountBySourceID(t, ctx, pool, "Bench_Press"); got != 0 {
		t.Errorf("exercise row count after dry run = %d, want 0 (dry run must write nothing)", got)
	}

	exists, err := s.Exercises.SlugExists(ctx, "bench-press")
	if err != nil {
		t.Fatalf("SlugExists: %v", err)
	}
	if exists {
		t.Error("SlugExists(bench-press) = true after dry run, want false")
	}
}

// TestExerciseStore_UpsertImport_DryRunOnExistingRowReportsUpdateOrUnchanged
// exercises the dry-run preview path against an already-imported exercise,
// not just the empty-database case.
func TestExerciseStore_UpsertImport_DryRunOnExistingRowReportsUpdateOrUnchanged(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	if _, err := s.Exercises.UpsertImport(ctx, baseImportInput("Bench_Press", "bench-press")); err != nil {
		t.Fatalf("seed import: %v", err)
	}

	t.Run("unchanged content previews as no-op", func(t *testing.T) {
		in := baseImportInput("Bench_Press", "bench-press")
		in.DryRun = true
		result, err := s.Exercises.UpsertImport(ctx, in)
		if err != nil {
			t.Fatalf("UpsertImport: %v", err)
		}
		if result.Created || result.RevisionAdded {
			t.Errorf("result = %+v, want Created=false RevisionAdded=false", result)
		}
	})

	t.Run("changed content previews as an update", func(t *testing.T) {
		in := baseImportInput("Bench_Press", "bench-press")
		in.Revision.Instructions = []string{"A different instruction set."}
		in.DryRun = true
		result, err := s.Exercises.UpsertImport(ctx, in)
		if err != nil {
			t.Fatalf("UpsertImport: %v", err)
		}
		if result.Created {
			t.Error("Created = true, want false (exercise already exists)")
		}
		if !result.RevisionAdded {
			t.Error("RevisionAdded = false, want true (content differs)")
		}
	})
}

func TestExerciseStore_SlugExists(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	exists, err := s.Exercises.SlugExists(ctx, "bench-press")
	if err != nil {
		t.Fatalf("SlugExists: %v", err)
	}
	if exists {
		t.Error("SlugExists on empty database = true, want false")
	}

	if _, err := s.Exercises.UpsertImport(ctx, baseImportInput("Bench_Press", "bench-press")); err != nil {
		t.Fatalf("UpsertImport: %v", err)
	}

	exists, err = s.Exercises.SlugExists(ctx, "bench-press")
	if err != nil {
		t.Fatalf("SlugExists: %v", err)
	}
	if !exists {
		t.Error("SlugExists after import = false, want true")
	}
}

func TestExerciseStore_GetBySourceID_NotFound(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	_, err := s.Exercises.GetBySourceID(ctx, "does-not-exist")
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("GetBySourceID: err = %v, want pgx.ErrNoRows", err)
	}
}

func TestExerciseStore_GetBySourceID_Found(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	created, err := s.Exercises.UpsertImport(ctx, baseImportInput("Bench_Press", "bench-press"))
	if err != nil {
		t.Fatalf("UpsertImport: %v", err)
	}

	ex, err := s.Exercises.GetBySourceID(ctx, "Bench_Press")
	if err != nil {
		t.Fatalf("GetBySourceID: %v", err)
	}
	if ex.ID != created.ExerciseID {
		t.Errorf("ID = %s, want %s", ex.ID, created.ExerciseID)
	}
	if ex.Slug != "bench-press" {
		t.Errorf("Slug = %q, want bench-press", ex.Slug)
	}
}

// --- helpers ---

func revisionCount(t *testing.T, ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, exerciseID string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM exercise_revisions WHERE exercise_id = $1`, exerciseID).Scan(&count); err != nil {
		t.Fatalf("count revisions for %s: %v", exerciseID, err)
	}
	return count
}

func exerciseCountBySourceID(t *testing.T, ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, sourceID string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM exercises WHERE source_id = $1`, sourceID).Scan(&count); err != nil {
		t.Fatalf("count exercises for source %s: %v", sourceID, err)
	}
	return count
}

func revisionStatus(t *testing.T, ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, revisionID string) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM exercise_revisions WHERE id = $1`, revisionID).Scan(&status); err != nil {
		t.Fatalf("status for revision %s: %v", revisionID, err)
	}
	return status
}
