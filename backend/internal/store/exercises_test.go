package store

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Cawlumm/open-exercise-db/internal/model"
)

func TestExerciseStore_List_Filters(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	seedExercise(t, ctx, pool, exerciseFixture{
		slug: "bench-press", name: "Bench Press", aliases: []string{"Barbell Bench Press"},
		muscleGroup: "chest", equipment: "barbell", category: "strength", mechanic: "compound", level: "beginner",
	})
	seedExercise(t, ctx, pool, exerciseFixture{
		slug: "squat", name: "Squat",
		muscleGroup: "legs", equipment: "barbell", category: "strength", mechanic: "compound", level: "intermediate",
	})
	seedExercise(t, ctx, pool, exerciseFixture{
		slug: "bicep-curl", name: "Bicep Curl", aliases: []string{"Dumbbell Curl"},
		muscleGroup: "arms", equipment: "dumbbell", category: "strength", mechanic: "isolation", level: "beginner",
	})
	// Archived: must never appear regardless of filter, per WO-2's
	// "only status='published'" requirement.
	seedExercise(t, ctx, pool, exerciseFixture{
		slug: "archived-lift", name: "Archived Lift",
		muscleGroup: "chest", equipment: "barbell", category: "strength", status: "archived",
	})

	tests := []struct {
		name      string
		filter    ExerciseFilter
		wantNames []string
		wantTotal int
	}{
		{
			name:      "no filter returns all published, archived excluded",
			filter:    ExerciseFilter{},
			wantNames: []string{"Bench Press", "Bicep Curl", "Squat"},
			wantTotal: 3,
		},
		{
			name:      "muscle_group filter",
			filter:    ExerciseFilter{MuscleGroup: "chest"},
			wantNames: []string{"Bench Press"},
			wantTotal: 1,
		},
		{
			name:      "equipment filter",
			filter:    ExerciseFilter{Equipment: "barbell"},
			wantNames: []string{"Bench Press", "Squat"},
			wantTotal: 2,
		},
		{
			name:      "category filter matches all",
			filter:    ExerciseFilter{Category: "strength"},
			wantNames: []string{"Bench Press", "Bicep Curl", "Squat"},
			wantTotal: 3,
		},
		{
			name:      "mechanic filter",
			filter:    ExerciseFilter{Mechanic: "isolation"},
			wantNames: []string{"Bicep Curl"},
			wantTotal: 1,
		},
		{
			name:      "level filter",
			filter:    ExerciseFilter{Level: "beginner"},
			wantNames: []string{"Bench Press", "Bicep Curl"},
			wantTotal: 2,
		},
		{
			name:      "q matches name, case-insensitive",
			filter:    ExerciseFilter{Q: "curl"},
			wantNames: []string{"Bicep Curl"},
			wantTotal: 1,
		},
		{
			name:      "q matches alias, not just name",
			filter:    ExerciseFilter{Q: "Dumbbell"},
			wantNames: []string{"Bicep Curl"},
			wantTotal: 1,
		},
		{
			name:      "combined filters narrow further",
			filter:    ExerciseFilter{MuscleGroup: "chest", Equipment: "barbell"},
			wantNames: []string{"Bench Press"},
			wantTotal: 1,
		},
		{
			name:      "filter matching nothing",
			filter:    ExerciseFilter{MuscleGroup: "does-not-exist"},
			wantNames: nil,
			wantTotal: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, total, err := s.Exercises.List(ctx, tt.filter)
			if err != nil {
				t.Fatalf("List(%+v): unexpected error: %v", tt.filter, err)
			}
			if total != tt.wantTotal {
				t.Errorf("total = %d, want %d", total, tt.wantTotal)
			}
			if !sameNames(got, tt.wantNames) {
				t.Errorf("names = %v, want %v", exerciseNames(got), tt.wantNames)
			}
			for _, ex := range got {
				if ex.CurrentRevision == nil {
					t.Errorf("exercise %s: CurrentRevision not populated", ex.Slug)
				}
			}
		})
	}
}

func TestExerciseStore_List_Pagination(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	// Names chosen to sort alphabetically (List orders by r.name ASC) and
	// stay single-digit so lexicographic order matches intended order.
	for _, name := range []string{"Exercise A", "Exercise B", "Exercise C", "Exercise D", "Exercise E"} {
		seedExercise(t, ctx, pool, exerciseFixture{
			slug: slugify(name), name: name,
			muscleGroup: "chest", equipment: "barbell", category: "strength",
		})
	}

	tests := []struct {
		name      string
		filter    ExerciseFilter
		wantNames []string
		wantTotal int
	}{
		{
			name:      "first page",
			filter:    ExerciseFilter{Page: 1, PerPage: 2},
			wantNames: []string{"Exercise A", "Exercise B"},
			wantTotal: 5,
		},
		{
			name:      "middle page",
			filter:    ExerciseFilter{Page: 2, PerPage: 2},
			wantNames: []string{"Exercise C", "Exercise D"},
			wantTotal: 5,
		},
		{
			name:      "last page, partial",
			filter:    ExerciseFilter{Page: 3, PerPage: 2},
			wantNames: []string{"Exercise E"},
			wantTotal: 5,
		},
		{
			name:      "past the end returns empty, not an error",
			filter:    ExerciseFilter{Page: 4, PerPage: 2},
			wantNames: nil,
			wantTotal: 5,
		},
		{
			name:      "per_page larger than total returns everything on page 1",
			filter:    ExerciseFilter{Page: 1, PerPage: 10},
			wantNames: []string{"Exercise A", "Exercise B", "Exercise C", "Exercise D", "Exercise E"},
			wantTotal: 5,
		},
		{
			name:      "page <= 0 defaults to page 1",
			filter:    ExerciseFilter{Page: 0, PerPage: 2},
			wantNames: []string{"Exercise A", "Exercise B"},
			wantTotal: 5,
		},
		{
			name:      "per_page <= 0 defaults rather than erroring",
			filter:    ExerciseFilter{Page: 1, PerPage: 0},
			wantNames: []string{"Exercise A", "Exercise B", "Exercise C", "Exercise D", "Exercise E"},
			wantTotal: 5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, total, err := s.Exercises.List(ctx, tt.filter)
			if err != nil {
				t.Fatalf("List(%+v): unexpected error: %v", tt.filter, err)
			}
			if total != tt.wantTotal {
				t.Errorf("total = %d, want %d", total, tt.wantTotal)
			}
			if !sameNamesInOrder(got, tt.wantNames) {
				t.Errorf("names = %v, want %v (in order)", exerciseNames(got), tt.wantNames)
			}
		})
	}
}

func TestExerciseStore_Get(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	publishedID, revisionID := seedExercise(t, ctx, pool, exerciseFixture{
		slug: "deadlift", name: "Deadlift", aliases: []string{"Conventional Deadlift"},
		muscleGroup: "back", equipment: "barbell", category: "strength", mechanic: "compound", level: "intermediate",
		instructions: []string{"Set up.", "Pull."},
	})
	archivedID, _ := seedExercise(t, ctx, pool, exerciseFixture{
		slug: "archived-lift", name: "Archived Lift",
		muscleGroup: "chest", equipment: "barbell", category: "strength", status: "archived",
	})

	t.Run("found", func(t *testing.T) {
		ex, err := s.Exercises.Get(ctx, publishedID)
		if err != nil {
			t.Fatalf("Get(%s): unexpected error: %v", publishedID, err)
		}
		if ex.Slug != "deadlift" {
			t.Errorf("Slug = %q, want %q", ex.Slug, "deadlift")
		}
		if ex.CurrentRevision == nil {
			t.Fatal("CurrentRevision not populated")
		}
		if ex.CurrentRevision.ID != revisionID {
			t.Errorf("CurrentRevision.ID = %q, want %q", ex.CurrentRevision.ID, revisionID)
		}
		if got := ex.CurrentRevision.Instructions; len(got) != 2 || got[0] != "Set up." || got[1] != "Pull." {
			t.Errorf("Instructions = %v, want [Set up. Pull.]", got)
		}
		if len(ex.CurrentRevision.Aliases) != 1 || ex.CurrentRevision.Aliases[0] != "Conventional Deadlift" {
			t.Errorf("Aliases = %v, want [Conventional Deadlift]", ex.CurrentRevision.Aliases)
		}
	})

	t.Run("archived exercise is not returned", func(t *testing.T) {
		_, err := s.Exercises.Get(ctx, archivedID)
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("Get(%s) error = %v, want pgx.ErrNoRows", archivedID, err)
		}
	})

	t.Run("nonexistent id", func(t *testing.T) {
		_, err := s.Exercises.Get(ctx, "00000000-0000-0000-0000-000000000000")
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("Get(nonexistent) error = %v, want pgx.ErrNoRows", err)
		}
	})
}

func TestExerciseStore_ListRevisions(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	exerciseID, firstRevisionID := seedExercise(t, ctx, pool, exerciseFixture{
		slug: "overhead-press", name: "Overhead Press v1",
		muscleGroup: "shoulders", equipment: "barbell", category: "strength",
	})

	base := time.Now()
	secondRevisionID := seedRevision(t, ctx, pool, exerciseID, "Overhead Press v2", base.Add(1*time.Hour), "approved")
	thirdRevisionID := seedRevision(t, ctx, pool, exerciseID, "Overhead Press v3", base.Add(2*time.Hour), "approved")

	// Supersede the earlier revisions and point the exercise at the newest,
	// mirroring what an approval flow (out of scope for WO-2) would do.
	if _, err := pool.Exec(ctx,
		`UPDATE exercise_revisions SET status = 'superseded' WHERE id IN ($1, $2)`,
		firstRevisionID, secondRevisionID,
	); err != nil {
		t.Fatalf("supersede earlier revisions: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE exercises SET current_revision_id = $1 WHERE id = $2`,
		thirdRevisionID, exerciseID,
	); err != nil {
		t.Fatalf("repoint current_revision_id: %v", err)
	}

	revisions, err := s.Exercises.ListRevisions(ctx, exerciseID, 1000, 0)
	if err != nil {
		t.Fatalf("ListRevisions(%s): unexpected error: %v", exerciseID, err)
	}

	if len(revisions) != 3 {
		t.Fatalf("len(revisions) = %d, want 3", len(revisions))
	}

	wantOrder := []string{thirdRevisionID, secondRevisionID, firstRevisionID}
	for i, want := range wantOrder {
		if revisions[i].ID != want {
			t.Errorf("revisions[%d].ID = %q, want %q (newest first)", i, revisions[i].ID, want)
		}
	}
	if revisions[0].Status != "approved" {
		t.Errorf("newest revision status = %q, want approved", revisions[0].Status)
	}
	if revisions[1].Status != "superseded" || revisions[2].Status != "superseded" {
		t.Errorf("older revisions should be superseded: got %q, %q", revisions[1].Status, revisions[2].Status)
	}
}

func TestExerciseStore_ListRevisions_NoRevisions(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	revisions, err := s.Exercises.ListRevisions(ctx, "00000000-0000-0000-0000-000000000000", 1000, 0)
	if err != nil {
		t.Fatalf("ListRevisions(nonexistent): unexpected error: %v", err)
	}
	if len(revisions) != 0 {
		t.Errorf("len(revisions) = %d, want 0", len(revisions))
	}
}

// exerciseNames extracts names in the order List returned them.
func exerciseNames(exercises []model.Exercise) []string {
	names := make([]string, 0, len(exercises))
	for _, ex := range exercises {
		if ex.CurrentRevision != nil {
			names = append(names, ex.CurrentRevision.Name)
		}
	}
	return names
}

// sameNames compares names ignoring order — used where the filter under
// test doesn't make ordering the point of the assertion.
func sameNames(exercises []model.Exercise, want []string) bool {
	got := exerciseNames(exercises)
	gotSorted := append([]string{}, got...)
	wantSorted := append([]string{}, want...)
	sort.Strings(gotSorted)
	sort.Strings(wantSorted)
	if len(gotSorted) != len(wantSorted) {
		return false
	}
	for i := range gotSorted {
		if gotSorted[i] != wantSorted[i] {
			return false
		}
	}
	return true
}

// sameNamesInOrder compares names preserving order — used for pagination
// assertions, where List's ORDER BY r.name ASC is exactly what's tested.
func sameNamesInOrder(exercises []model.Exercise, want []string) bool {
	got := exerciseNames(exercises)
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// slugify is a minimal, test-only slug generator — WO-4 owns the real
// collision-handling slug generator for the importer; tests here just need
// unique, valid slugs.
func slugify(name string) string {
	return strings.ToLower(strings.ReplaceAll(name, " ", "-"))
}
