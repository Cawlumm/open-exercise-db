package store

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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
			filter:    ExerciseFilter{MuscleGroup: []string{"chest"}},
			wantNames: []string{"Bench Press"},
			wantTotal: 1,
		},
		{
			name:      "equipment filter",
			filter:    ExerciseFilter{Equipment: []string{"barbell"}},
			wantNames: []string{"Bench Press", "Squat"},
			wantTotal: 2,
		},
		{
			name:      "category filter matches all",
			filter:    ExerciseFilter{Category: []string{"strength"}},
			wantNames: []string{"Bench Press", "Bicep Curl", "Squat"},
			wantTotal: 3,
		},
		{
			name:      "mechanic filter",
			filter:    ExerciseFilter{Mechanic: []string{"isolation"}},
			wantNames: []string{"Bicep Curl"},
			wantTotal: 1,
		},
		{
			name:      "level filter",
			filter:    ExerciseFilter{Level: []string{"beginner"}},
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
			filter:    ExerciseFilter{MuscleGroup: []string{"chest"}, Equipment: []string{"barbell"}},
			wantNames: []string{"Bench Press"},
			wantTotal: 1,
		},
		{
			name:      "filter matching nothing",
			filter:    ExerciseFilter{MuscleGroup: []string{"does-not-exist"}},
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

// searchCorpus is a small fixture set chosen so each assertion below can only
// pass for the right reason: the kettlebell rows do not say "kettlebell" in
// their names, and the quadriceps rows do not say "quadriceps".
func seedSearchCorpus(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, f := range []exerciseFixture{
		{slug: "goblet-squat", name: "Goblet Squat", muscleGroup: "quadriceps", equipment: "kettlebells", category: "strength", force: "push", level: "beginner"},
		{slug: "kettlebell-swing", name: "Swing", muscleGroup: "glutes", equipment: "kettlebells", category: "strength", force: "pull", level: "intermediate"},
		{slug: "front-squat", name: "Front Squat", muscleGroup: "quadriceps", equipment: "barbell", category: "olympic-weightlifting", force: "push", level: "expert"},
		{slug: "bench-press", name: "Bench Press", muscleGroup: "chest", secondary: []string{"triceps"}, equipment: "barbell", category: "powerlifting", force: "push", level: "beginner"},
		{slug: "incline-bench-press", name: "Incline Bench Press", muscleGroup: "chest", equipment: "barbell", category: "strength", force: "push", level: "intermediate"},
		{slug: "kettlebell-press", name: "Kettlebell Press", muscleGroup: "shoulders", equipment: "kettlebells", category: "strength", force: "push", level: "beginner"},
	} {
		seedExercise(t, ctx, pool, f)
	}
}

func namesOf(exercises []model.Exercise) []string {
	out := make([]string, 0, len(exercises))
	for _, e := range exercises {
		out = append(out, e.CurrentRevision.Name)
	}
	return out
}

// The defect this fixes: search matched names and aliases only, so the words
// people actually type into a single search box — a muscle, a piece of
// equipment — found almost nothing. `q=kettlebells` returned the one exercise
// with the word in its name rather than every exercise that uses them, which
// left a consumer no option but to download the catalog and filter it locally.
func TestList_SearchMatchesEquipment(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()
	seedSearchCorpus(t, ctx, pool)

	got, total, err := store.Exercises.List(ctx, ExerciseFilter{Q: "kettlebell"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3 (%v)", total, namesOf(got))
	}
	// The one that says so in its name comes first; the two that merely use
	// kettlebells follow.
	if got[0].CurrentRevision.Name != "Kettlebell Press" {
		t.Errorf("first result = %q, want the name match first (%v)", got[0].CurrentRevision.Name, namesOf(got))
	}
}

func TestList_SearchMatchesMuscleGroupAndSecondary(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()
	seedSearchCorpus(t, ctx, pool)

	quads, _, err := store.Exercises.List(ctx, ExerciseFilter{Q: "quadriceps"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(quads) != 2 {
		t.Errorf("quadriceps -> %v, want the two quad exercises", namesOf(quads))
	}

	// A secondary muscle counts too: someone looking for triceps work should
	// find the press that trains them second.
	tris, _, err := store.Exercises.List(ctx, ExerciseFilter{Q: "triceps"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tris) != 1 || tris[0].CurrentRevision.Name != "Bench Press" {
		t.Errorf("triceps -> %v, want [Bench Press]", namesOf(tris))
	}
}

func TestList_SearchMatchesCategoryForceAndLevel(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()
	seedSearchCorpus(t, ctx, pool)

	for _, tc := range []struct {
		q    string
		want int
	}{
		{"powerlifting", 1},
		{"olympic", 1}, // substring of the slug, because nobody types "olympic-weightlifting"
		{"expert", 1},
		{"pull", 1},
	} {
		got, total, err := store.Exercises.List(ctx, ExerciseFilter{Q: tc.q})
		if err != nil {
			t.Fatalf("list %q: %v", tc.q, err)
		}
		if total != tc.want {
			t.Errorf("q=%q -> %d (%v), want %d", tc.q, total, namesOf(got), tc.want)
		}
	}
}

// Facet matching must not drown the obvious answer. "press" is in three names
// here and is also nothing's facet, but the ranking has to keep exact and
// prefix name matches on top regardless of what else qualified.
func TestList_NameMatchesOutrankFacetOnlyMatches(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()
	seedSearchCorpus(t, ctx, pool)

	got, _, err := store.Exercises.List(ctx, ExerciseFilter{Q: "squat"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) < 2 {
		t.Fatalf("got %v, want both squats", namesOf(got))
	}
	for _, e := range got {
		if !strings.Contains(strings.ToLower(e.CurrentRevision.Name), "squat") {
			t.Errorf("facet-only match %q ranked among name matches (%v)", e.CurrentRevision.Name, namesOf(got))
		}
	}

	// A term that only exists as a facet still finds its rows — it just sits
	// below anything matching by name.
	mixed, _, err := store.Exercises.List(ctx, ExerciseFilter{Q: "press"})
	if err != nil {
		t.Fatalf("list press: %v", err)
	}
	if mixed[0].CurrentRevision.Name != "Bench Press" {
		t.Errorf("first = %q, want the shortest exact-ish name match first (%v)", mixed[0].CurrentRevision.Name, namesOf(mixed))
	}
}

// Terms still AND together, and they may now be satisfied by different fields:
// "barbell bench" is one word of equipment and one of name.
func TestList_SearchTermsCombineAcrossNameAndFacets(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()
	seedSearchCorpus(t, ctx, pool)

	got, total, err := store.Exercises.List(ctx, ExerciseFilter{Q: "barbell bench"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 2 {
		t.Errorf("barbell bench -> %d (%v), want the two barbell benches", total, namesOf(got))
	}

	// Word order stays irrelevant, as it was before this change.
	reversed, reversedTotal, err := store.Exercises.List(ctx, ExerciseFilter{Q: "bench barbell"})
	if err != nil {
		t.Fatalf("list reversed: %v", err)
	}
	if reversedTotal != total {
		t.Errorf("word order changed the result: %d vs %d (%v)", reversedTotal, total, namesOf(reversed))
	}
}

// Free-text search composes with the explicit facet filters rather than
// replacing them: q narrows within the filter, not around it.
func TestList_SearchComposesWithFacetFilters(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()
	seedSearchCorpus(t, ctx, pool)

	got, total, err := store.Exercises.List(ctx, ExerciseFilter{Q: "press", Equipment: []string{"kettlebells"}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || got[0].CurrentRevision.Name != "Kettlebell Press" {
		t.Errorf("q=press&equipment=kettlebells -> %v, want [Kettlebell Press]", namesOf(got))
	}
}

// LIKE metacharacters stay inert now that they reach more columns: a search for
// a literal percent must not match every row through a facet either.
func TestList_SearchWildcardsRemainLiteralAcrossFacets(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()
	seedSearchCorpus(t, ctx, pool)

	for _, q := range []string{"%", "_", `\`} {
		_, total, err := store.Exercises.List(ctx, ExerciseFilter{Q: q})
		if err != nil {
			t.Fatalf("list %q: %v", q, err)
		}
		if total != 0 {
			t.Errorf("q=%q matched %d rows; wildcards must be literal", q, total)
		}
	}
}

// Values within one filter are OR'd and filters are AND'd — what a faceted
// picker means by "chest or lats, with a barbell". Before this the fields were
// single strings, so a repeated parameter was silently reduced to its first
// value and half the filter vanished without a word.
func TestList_MultiValueFiltersOrWithinAndAcross(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()
	seedSearchCorpus(t, ctx, pool)

	both, total, err := store.Exercises.List(ctx, ExerciseFilter{
		MuscleGroup: []string{"quadriceps", "chest"},
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 4 {
		t.Errorf("quadriceps OR chest -> %d (%v), want 4", total, namesOf(both))
	}

	// AND across dimensions: quads or chest, but only with a barbell.
	narrowed, narrowTotal, err := store.Exercises.List(ctx, ExerciseFilter{
		MuscleGroup: []string{"quadriceps", "chest"},
		Equipment:   []string{"barbell"},
	})
	if err != nil {
		t.Fatalf("list narrowed: %v", err)
	}
	if narrowTotal != 3 {
		t.Errorf("with equipment -> %d (%v), want 3", narrowTotal, namesOf(narrowed))
	}
	if narrowTotal >= total {
		t.Errorf("adding a second filter widened the result: %d >= %d", narrowTotal, total)
	}
}

// Blank values must not become a filter for the empty string — a trailing comma
// or an unset select would otherwise match nothing and look like real data.
func TestList_BlankFilterValuesAreIgnored(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()
	seedSearchCorpus(t, ctx, pool)

	all, total, err := store.Exercises.List(ctx, ExerciseFilter{})
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	got, gotTotal, err := store.Exercises.List(ctx, ExerciseFilter{MuscleGroup: []string{"", "  "}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if gotTotal != total {
		t.Errorf("blank filter changed the result: %d vs %d (%v)", gotTotal, total, namesOf(got))
	}
	_ = all
}

// Refreshing a known set of rows should cost one request for those rows, not the
// whole catalog. Without this a consumer holding its own copy had to pull the
// full export and intersect locally.
func TestList_ByIDsFetchesExactlyThoseRows(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()
	seedSearchCorpus(t, ctx, pool)

	all, _, err := store.Exercises.List(ctx, ExerciseFilter{})
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	want := []string{all[0].ID, all[2].ID}

	got, total, err := store.Exercises.List(ctx, ExerciseFilter{IDs: want})
	if err != nil {
		t.Fatalf("list by ids: %v", err)
	}
	if total != 2 || len(got) != 2 {
		t.Fatalf("got %d rows (total %d), want 2 (%v)", len(got), total, namesOf(got))
	}
	found := map[string]bool{got[0].ID: true, got[1].ID: true}
	for _, id := range want {
		if !found[id] {
			t.Errorf("id %s missing from the batch", id)
		}
	}
}

// The migration case: anyone moving off free-exercise-db holds its identifiers,
// and export has always published source_id while nothing could query by it.
func TestList_BySourceIDs(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()
	seedExercise(t, ctx, pool, exerciseFixture{slug: "a", name: "A", sourceID: "Upstream_A"})
	seedExercise(t, ctx, pool, exerciseFixture{slug: "b", name: "B", sourceID: "Upstream_B"})
	seedExercise(t, ctx, pool, exerciseFixture{slug: "c", name: "C", sourceID: "Upstream_C"})

	got, total, err := store.Exercises.List(ctx, ExerciseFilter{SourceIDs: []string{"Upstream_A", "Upstream_C"}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 2 {
		t.Fatalf("got %d (%v), want 2", total, namesOf(got))
	}
}
