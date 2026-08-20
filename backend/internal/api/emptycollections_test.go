package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// This file exists because of a specific escaped defect: every list-shaped
// response body was built by appending onto a nil slice, so any request that
// matched nothing serialized as `"exercises": null` instead of `[]`. It
// survived a green Go suite and a 54-check end-to-end smoke suite, because
// both asserted status codes and the presence of keys — never the *shape* of
// a collection that came back empty. Assertions here are deliberately made
// against the marshaled JSON rather than the Go slice, since nil and empty
// are indistinguishable at the Go level and identical to every test that
// only checks len() == 0. The bug lives entirely in the encoding.

// assertJSONArrayNotNull marshals v and fails if field is `null` rather than
// an array.
func assertJSONArrayNotNull(t *testing.T, v any, field string) {
	t.Helper()

	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var generic map[string]json.RawMessage
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	got, ok := generic[field]
	if !ok {
		t.Fatalf("field %q missing from %s", field, raw)
	}
	if string(got) == "null" {
		t.Errorf("%q serialized as null, want []\nfull body: %s", field, raw)
	}
	if !strings.HasPrefix(string(got), "[") {
		t.Errorf("%q = %s, want a JSON array", field, got)
	}
}

func TestListExercises_NoMatchesSerializesAsEmptyArray(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	// A filter value no seeded taxonomy term uses. This is the exact request
	// a type-ahead search issues on its first non-matching keystroke — the
	// primary consumption model docs/DESIGN.md recommends — which is what
	// made `null` here a client-crashing default rather than a cosmetic wart.
	out, err := ListExercises(ctx, s, "", &ListExercisesInput{
		MuscleGroup: []string{"no-such-muscle-group"},
		PageParams:  PageParams{Page: 1, PerPage: 50},
	})
	if err != nil {
		t.Fatalf("ListExercises: %v", err)
	}
	if out.Body.Total != 0 {
		t.Fatalf("total = %d, want 0 (fixture assumption broken)", out.Body.Total)
	}
	assertJSONArrayNotNull(t, out.Body, "exercises")
}

func TestListExercises_PagePastEndSerializesAsEmptyArray(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	// Distinct from the no-matches case above: total is non-zero here, so the
	// handler takes a different path to the same empty page. A paginating
	// consumer walking to the end hits this one.
	out, err := ListExercises(ctx, s, "", &ListExercisesInput{
		PageParams: PageParams{Page: 99999, PerPage: 50},
	})
	if err != nil {
		t.Fatalf("ListExercises: %v", err)
	}
	assertJSONArrayNotNull(t, out.Body, "exercises")
}

func TestListExerciseTranslations_NoTranslationsSerializesAsEmptyArray(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	id, _ := seedExercise(t, ctx, pool, exerciseFixture{
		slug: "untranslated-lift", name: "Untranslated Lift",
		muscleGroup: "chest", equipment: "barbell", category: "strength",
	})

	// The common case, not an edge case: an exercise carrying only its
	// revision's own content has no translation rows at all, which was every
	// exercise in the bootstrap dataset.
	out, err := ListExerciseTranslations(ctx, s, &ListExerciseTranslationsInput{ID: id})
	if err != nil {
		t.Fatalf("ListExerciseTranslations: %v", err)
	}
	// "translations", not "locales" — /meta/locales already uses that name for
	// a different item type (docs/CONVENTIONS.md section 3a).
	assertJSONArrayNotNull(t, out.Body, "translations")
}

func TestListTaxonomyTerms_EmptyFacetSerializesAsEmptyArray(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	// Every facet is seeded non-empty today, so this path is unreachable
	// through real data — which is precisely why it needs a test. A facet
	// that ships empty later would otherwise reintroduce the same null.
	out, err := ListTaxonomyTerms(ctx, s, "force", &ListTaxonomyTermsInput{Locale: "en"})
	if err != nil {
		t.Fatalf("ListTaxonomyTerms: %v", err)
	}
	assertJSONArrayNotNull(t, out.Body, "terms")
}
