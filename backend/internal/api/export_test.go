package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2/humatest"

	"github.com/Cawlumm/open-exercise-db/internal/contract"
	"github.com/Cawlumm/open-exercise-db/internal/export"
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// fullFixtureImport returns a store.UpsertImportInput with every optional
// field populated (mechanic, force, level, license, attribution_text,
// source_url, secondary_muscles, source_id). The key-set parity tests below
// need this: an omitempty pointer field that happens to be nil on both sides
// would "match" without the comparison actually exercising anything.
func fullFixtureImport(sourceID, slug string) store.UpsertImportInput {
	mechanic := "compound"
	force := "push"
	level := "intermediate"
	license := "Unlicense"
	attribution := "free-exercise-db contributors"
	sourceURL := "https://github.com/yuhonas/free-exercise-db"
	return store.UpsertImportInput{
		SourceID: sourceID,
		Slug:     slug,
		Revision: store.ImportRevisionInput{
			Name:             "Bench Press",
			Aliases:          []string{"Barbell Bench Press"},
			MuscleGroup:      "chest",
			SecondaryMuscles: []string{"triceps"},
			Category:         "strength",
			Equipment:        "barbell",
			Mechanic:         &mechanic,
			Force:            &force,
			Level:            &level,
			Instructions:     []string{"Lie on the bench.", "Press the bar up."},
			ImageIDs:         []string{},
			License:          &license,
			AttributionText:  &attribution,
			SourceURL:        &sourceURL,
		},
	}
}

// jsonKeyPaths flattens v's JSON encoding into a sorted, de-duplicated list
// of dotted key paths (e.g. "translation.name"). It's how
// TestExportExercises_KeySetMatchesGetExercise and
// TestExportExercises_PinnedKeySet compare shapes — by their actual JSON
// contract, the thing consumers key on, rather than by Go type identity.
// (As of this package's contract.Exercise extraction, GET /exercises/{id}'s
// Body and one entry of GET /exercises/export's Body.Exercises *are* the
// same Go type, so a type-identity check would now also work — but JSON is
// still the right level to assert at: it's what a downstream mirror
// actually parses, and it's what would have caught the original bug this
// package shipped with just as well as a type check would have. See
// TestExportExercises_KeySetMatchesGetExercise's comment for what this test
// does and does not guarantee post-fix.)
// Array elements are walked without indexing since every element of a given
// array in these responses shares one shape.
func jsonKeyPaths(t *testing.T, v any) []string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal for key paths: %v", err)
	}
	var raw any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal for key paths: %v", err)
	}

	seen := map[string]bool{}
	var walk func(prefix string, node any)
	walk = func(prefix string, node any) {
		switch n := node.(type) {
		case map[string]any:
			for k, v := range n {
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				seen[p] = true
				walk(p, v)
			}
		case []any:
			for _, item := range n {
				walk(prefix, item)
			}
		}
	}
	walk("", raw)

	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// diffKeyPaths reports the symmetric difference between two already-sorted
// path lists, or "" if they're identical.
func diffKeyPaths(want, got []string) string {
	wantSet := map[string]bool{}
	for _, p := range want {
		wantSet[p] = true
	}
	gotSet := map[string]bool{}
	for _, p := range got {
		gotSet[p] = true
	}

	var missing, extra []string
	for _, p := range want {
		if !gotSet[p] {
			missing = append(missing, p)
		}
	}
	for _, p := range got {
		if !wantSet[p] {
			extra = append(extra, p)
		}
	}
	if len(missing) == 0 && len(extra) == 0 {
		return ""
	}
	var sb strings.Builder
	if len(missing) > 0 {
		fmt.Fprintf(&sb, "  missing: %v\n", missing)
	}
	if len(extra) > 0 {
		fmt.Fprintf(&sb, "  unexpected: %v\n", extra)
	}
	return sb.String()
}

func findExported(body export.Body, id string) *contract.Exercise {
	for i := range body.Exercises {
		if body.Exercises[i].ID == id {
			return &body.Exercises[i]
		}
	}
	return nil
}

// TestExportExercises_KeySetMatchesGetExercise asserts, for one real
// exercise, that GET /exercises/{id} and GET /exercises/export produce the
// exact same set of JSON keys, at every nesting level.
//
// Be precise about what this test does and doesn't guarantee. GET
// /exercises/{id}'s Body and GET /exercises/export's per-item entries are
// now both contract.Exercise (internal/contract) — literally the same Go
// type, not two independently maintained copies — so this test cannot
// currently fail from field-name drift; that class of bug is now
// structurally impossible, not merely detected. What it still guards
// against: (a) a regression back to two separate types (if either package
// ever redeclares its own Exercise/Translation instead of importing
// contract, this test's diff would immediately show it, the same way it
// would have caught the *original* WO-5 defect had it existed then), and
// (b) that export's assembly path (assembleExercise in export.go) actually
// wires the shared type's fields correctly end-to-end from real store data,
// not just that the type compiles.
//
// What this test never could and still doesn't cover: whether two Go types
// sharing a field-for-field-identical JSON shape can coexist inside one
// huma.API. That is a registration-time concern (huma's OpenAPI
// component-schema registry, keyed by Go type name) that only surfaces when
// the real Register wires every route onto one huma.API instance — this
// test calls GetExercise/ExportExercises directly and never does that. The
// project actually broke exactly this way once: two independently declared
// types both named "Exercise" passed every JSON-shape test in this file
// and still panicked api.Register at startup. TestRegister_DoesNotPanic
// (register_test.go) is what closes that gap now.
func TestExportExercises_KeySetMatchesGetExercise(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	result, err := s.Exercises.UpsertImport(ctx, fullFixtureImport("Bench_Press", "bench-press"))
	if err != nil {
		t.Fatalf("seed exercise: %v", err)
	}

	getOut, err := GetExercise(ctx, s, &GetExerciseInput{ID: result.ExerciseID})
	if err != nil {
		t.Fatalf("GetExercise: %v", err)
	}

	cache := export.NewCache(export.DefaultCacheTTL)
	exportOut, err := ExportExercises(ctx, s, cache, &ExportExercisesInput{})
	if err != nil {
		t.Fatalf("ExportExercises: %v", err)
	}
	entry := findExported(exportOut.Body, result.ExerciseID)
	if entry == nil {
		t.Fatalf("exercise %s missing from export body", result.ExerciseID)
	}

	getKeys := jsonKeyPaths(t, getOut.Body)
	exportKeys := jsonKeyPaths(t, *entry)
	if diff := diffKeyPaths(getKeys, exportKeys); diff != "" {
		t.Errorf("GET /exercises/{id} and GET /exercises/export key sets differ for the same exercise:\n%s", diff)
	}
}

// TestExportExercises_PinnedKeySet pins the exact JSON key set for one
// fully-populated export entry, so a future accidental rename anywhere in
// contract.Exercise/contract.Translation fails this test loudly instead of
// silently breaking every downstream mirror keying on these field names
// (docs/PHASE1_BUILD_PLAN.md WO-5 acceptance criteria). Since GET
// /exercises/export renders contract.Exercise directly, this also pins GET
// /exercises' and GET /exercises/{id}'s shape — there's only one type left
// to rename.
func TestExportExercises_PinnedKeySet(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	result, err := s.Exercises.UpsertImport(ctx, fullFixtureImport("Bench_Press", "bench-press"))
	if err != nil {
		t.Fatalf("seed exercise: %v", err)
	}

	cache := export.NewCache(export.DefaultCacheTTL)
	out, err := ExportExercises(ctx, s, cache, &ExportExercisesInput{})
	if err != nil {
		t.Fatalf("ExportExercises: %v", err)
	}
	entry := findExported(out.Body, result.ExerciseID)
	if entry == nil {
		t.Fatal("seeded exercise missing from export")
	}

	want := []string{
		"id", "slug", "source_id", "status", "created_at", "updated_at",
		"revision_id", "muscle_group", "secondary_muscles", "category", "equipment",
		"mechanic", "force", "level", "image_ids", "video_ids",
		"license", "attribution_text", "source_url", "translation",
		"translation.requested_locale",
		"translation.name", "translation.name_locale", "translation.name_machine_origin",
		"translation.aliases", "translation.aliases_locale", "translation.aliases_machine_origin",
		"translation.instructions", "translation.instructions_locale", "translation.instructions_machine_origin",
	}
	sort.Strings(want)

	got := jsonKeyPaths(t, *entry)
	if diff := diffKeyPaths(want, got); diff != "" {
		t.Errorf("contract.Exercise's JSON key set drifted from the pinned contract:\n%s", diff)
	}
}

// TestExportExercises_InstructionsIsAnArrayNotAFlattenedString guards
// against the specific regression docs/PHASE1_BUILD_PLAN.md WO-5 calls out
// by name: an earlier, incorrect draft of this plan flattened instructions
// into a numbered "description" string to match lyftr's schema. That must
// never come back — instructions stays a JSON array, matching /exercises
// and the database column, and there must be no "description" field at all.
func TestExportExercises_InstructionsIsAnArrayNotAFlattenedString(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	result, err := s.Exercises.UpsertImport(ctx, fullFixtureImport("Bench_Press", "bench-press"))
	if err != nil {
		t.Fatalf("seed exercise: %v", err)
	}

	cache := export.NewCache(export.DefaultCacheTTL)
	out, err := ExportExercises(ctx, s, cache, &ExportExercisesInput{})
	if err != nil {
		t.Fatalf("ExportExercises: %v", err)
	}
	entry := findExported(out.Body, result.ExerciseID)
	if entry == nil {
		t.Fatal("seeded exercise missing from export")
	}

	b, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if _, exists := raw["description"]; exists {
		t.Error(`contract.Exercise must not have a "description" field — that was the incorrect lyftr-shaped design, explicitly removed`)
	}

	tr, ok := raw["translation"].(map[string]any)
	if !ok {
		t.Fatalf(`"translation" is not a JSON object: %T`, raw["translation"])
	}
	if _, ok := tr["instructions"].([]any); !ok {
		t.Errorf(`"instructions" is not a JSON array: %T (%v)`, tr["instructions"], tr["instructions"])
	}
}

func TestExportExercises_ExcludesArchivedExercises(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	seedExercise(t, ctx, pool, exerciseFixture{
		slug: "bench-press", name: "Bench Press",
		muscleGroup: "chest", equipment: "barbell", category: "strength",
	})
	seedExercise(t, ctx, pool, exerciseFixture{
		slug: "old-lift", name: "Old Lift",
		muscleGroup: "back", equipment: "cable", category: "strength", status: "archived",
	})

	cache := export.NewCache(export.DefaultCacheTTL)
	out, err := ExportExercises(ctx, s, cache, &ExportExercisesInput{})
	if err != nil {
		t.Fatalf("ExportExercises: %v", err)
	}
	if out.Body.Total != 1 || len(out.Body.Exercises) != 1 {
		t.Fatalf("Total/len = %d/%d, want 1/1 (archived exercise must be excluded, same as GET /exercises)", out.Body.Total, len(out.Body.Exercises))
	}
	if out.Body.Exercises[0].Slug != "bench-press" {
		t.Errorf("Slug = %q, want bench-press", out.Body.Exercises[0].Slug)
	}
	if out.CacheControl != exportCacheControl {
		t.Errorf("CacheControl = %q, want %q", out.CacheControl, exportCacheControl)
	}
	if out.ETag == "" {
		t.Error("ETag header not set")
	}
}

// TestExportExercises_ConditionalRequestReturns304 reuses WO-3's
// checkNotModified (api.go) over an HTTP round trip, same as
// TestListExercises_ConditionalRequestReturns304 in exercises_test.go —
// needed for a real request because the short-circuit only runs in huma's
// request pipeline, not when calling ExportExercises directly.
func TestExportExercises_ConditionalRequestReturns304(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	seedExercise(t, ctx, pool, exerciseFixture{
		slug: "bench-press", name: "Bench Press",
		muscleGroup: "chest", equipment: "barbell", category: "strength",
	})

	cache := export.NewCache(export.DefaultCacheTTL)
	_, testAPI := humatest.New(t)
	registerExportRoutes(testAPI, s, cache)

	first := testAPI.Get("/api/v1/exercises/export")
	if first.Code != http.StatusOK {
		t.Fatalf("initial request status = %d, want 200; body: %s", first.Code, first.Body.String())
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("initial 200 response had no ETag header")
	}
	if cc := first.Header().Get("Cache-Control"); cc != exportCacheControl {
		t.Errorf("Cache-Control = %q, want %q (export's own longer policy, not the standard cacheControl)", cc, exportCacheControl)
	}

	t.Run("matching If-None-Match short-circuits to a bodyless 304", func(t *testing.T) {
		resp := testAPI.Get("/api/v1/exercises/export", "If-None-Match: "+etag)
		if resp.Code != http.StatusNotModified {
			t.Fatalf("status = %d, want %d; body: %s", resp.Code, http.StatusNotModified, resp.Body.String())
		}
		if resp.Body.Len() != 0 {
			t.Errorf("304 response had a %d-byte body, want none: %s", resp.Body.Len(), resp.Body.String())
		}
		if got := resp.Header().Get("ETag"); got != etag {
			t.Errorf("304 ETag = %q, want %q", got, etag)
		}
		if got := resp.Header().Get("Cache-Control"); got != exportCacheControl {
			t.Errorf("304 Cache-Control = %q, want %q", got, exportCacheControl)
		}
	})

	t.Run("stale If-None-Match still returns 200", func(t *testing.T) {
		resp := testAPI.Get("/api/v1/exercises/export", `If-None-Match: "not-the-real-etag"`)
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", resp.Code, resp.Body.String())
		}
	})

	t.Run("wildcard If-None-Match matches", func(t *testing.T) {
		resp := testAPI.Get("/api/v1/exercises/export", `If-None-Match: *`)
		if resp.Code != http.StatusNotModified {
			t.Fatalf("status = %d, want %d; body: %s", resp.Code, http.StatusNotModified, resp.Body.String())
		}
	})

	// OWS (RFC 9110 optional whitespace) list cases — same defect class as
	// TestListExercises_ConditionalRequestHandlesListOWS, exercised here to
	// confirm the fix in checkNotModified (api.go) isn't specific to the
	// exercises.go call sites; export.go's handler routes through the exact
	// same function.
	owsCases := []struct {
		name   string
		header string
	}{
		{"single space after comma, target last", `If-None-Match: "other", ` + etag},
		{"tab after comma, target last", "If-None-Match: \"other\",\t" + etag},
		{"space after comma, target first", `If-None-Match: ` + etag + `, "other"`},
	}
	for _, tt := range owsCases {
		t.Run(tt.name, func(t *testing.T) {
			resp := testAPI.Get("/api/v1/exercises/export", tt.header)
			if resp.Code != http.StatusNotModified {
				t.Fatalf("status = %d, want %d (%s); body: %s", resp.Code, http.StatusNotModified, tt.header, resp.Body.String())
			}
		})
	}
}

// TestExportExercises_CacheAvoidsRequeryAndInvalidateTriggersRebuild is
// WO-5's other core acceptance criterion: serving the dump twice must not
// re-query the store, and ETag must change once the underlying data changes
// *and* the cache is told to rebuild. A direct DB write between the two
// requests with no Invalidate() call proves the first half (the response
// would already reflect two exercises if ExportExercises queried per
// request); calling Invalidate() and re-requesting proves the second half.
func TestExportExercises_CacheAvoidsRequeryAndInvalidateTriggersRebuild(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	seedExercise(t, ctx, pool, exerciseFixture{
		slug: "bench-press", name: "Bench Press",
		muscleGroup: "chest", equipment: "barbell", category: "strength",
	})

	cache := export.NewCache(export.DefaultCacheTTL)
	_, testAPI := humatest.New(t)
	registerExportRoutes(testAPI, s, cache)

	first := testAPI.Get("/api/v1/exercises/export")
	if first.Code != http.StatusOK {
		t.Fatalf("initial request status = %d, want 200; body: %s", first.Code, first.Body.String())
	}
	firstETag := first.Header().Get("ETag")
	if !strings.Contains(first.Body.String(), `"total":1`) {
		t.Fatalf("initial body missing total:1: %s", first.Body.String())
	}

	seedExercise(t, ctx, pool, exerciseFixture{
		slug: "squat", name: "Squat",
		muscleGroup: "legs", equipment: "barbell", category: "strength",
	})

	stillCached := testAPI.Get("/api/v1/exercises/export")
	if stillCached.Header().Get("ETag") != firstETag {
		t.Fatalf("ETag changed without Invalidate() — cache was not used, request re-queried the store")
	}
	if !strings.Contains(stillCached.Body.String(), `"total":1`) {
		t.Errorf("body changed to reflect the new exercise without Invalidate() — cache was not used: %s", stillCached.Body.String())
	}

	cache.Invalidate()

	rebuilt := testAPI.Get("/api/v1/exercises/export")
	if rebuilt.Code != http.StatusOK {
		t.Fatalf("status after Invalidate = %d, want 200; body: %s", rebuilt.Code, rebuilt.Body.String())
	}
	if rebuilt.Header().Get("ETag") == firstETag {
		t.Fatal("ETag unchanged after Invalidate() + new data — expected a rebuild to pick up the new exercise")
	}
	if !strings.Contains(rebuilt.Body.String(), `"total":2`) {
		t.Errorf("rebuilt body missing total:2: %s", rebuilt.Body.String())
	}
}
