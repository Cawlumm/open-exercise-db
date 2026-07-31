package api

import (
	"context"
	"testing"
)

func TestListTaxonomyTerms_LocaleFallback(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	seedTaxonomyTerm(t, ctx, pool, facetMuscleGroup, "chest", map[string]string{"en": "Chest", "es": "Pecho"})
	seedTaxonomyTerm(t, ctx, pool, facetMuscleGroup, "back", map[string]string{"en": "Back"})

	out, err := ListTaxonomyTerms(ctx, s, facetMuscleGroup, &ListTaxonomyTermsInput{Locale: "es"})
	if err != nil {
		t.Fatalf("ListTaxonomyTerms: unexpected error: %v", err)
	}
	if len(out.Body.Terms) != 2 {
		t.Fatalf("len(Terms) = %d, want 2", len(out.Body.Terms))
	}

	byLabel := map[string]TaxonomyTerm{}
	for _, term := range out.Body.Terms {
		byLabel[term.Slug] = term
	}
	if byLabel["chest"].Label != "Pecho" || byLabel["chest"].Locale != "es" {
		t.Errorf("chest term = %+v, want label=Pecho locale=es", byLabel["chest"])
	}
	if byLabel["back"].Label != "Back" || byLabel["back"].Locale != "en" {
		t.Errorf("back term = %+v, want label=Back locale=en (no es label, falls back)", byLabel["back"])
	}
	if out.ETag == "" {
		t.Error("ETag header not set")
	}
}

func TestListTaxonomyTerms_FacetScoping(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	seedTaxonomyTerm(t, ctx, pool, facetEquipment, "other", map[string]string{"en": "Other"})
	seedTaxonomyTerm(t, ctx, pool, facetCategory, "other", map[string]string{"en": "Other category"})

	equipment, err := ListTaxonomyTerms(ctx, s, facetEquipment, &ListTaxonomyTermsInput{})
	if err != nil {
		t.Fatalf("ListTaxonomyTerms(equipment): %v", err)
	}
	category, err := ListTaxonomyTerms(ctx, s, facetCategory, &ListTaxonomyTermsInput{})
	if err != nil {
		t.Fatalf("ListTaxonomyTerms(category): %v", err)
	}

	if len(equipment.Body.Terms) != 1 || equipment.Body.Terms[0].Label != "Other" {
		t.Errorf("equipment terms = %+v", equipment.Body.Terms)
	}
	if len(category.Body.Terms) != 1 || category.Body.Terms[0].Label != "Other category" {
		t.Errorf("category terms = %+v", category.Body.Terms)
	}
}

func TestListLocales_AlwaysIncludesEnglish(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	out, err := ListLocales(ctx, s, &ListLocalesInput{})
	if err != nil {
		t.Fatalf("ListLocales: unexpected error: %v", err)
	}
	if len(out.Body.Locales) != 1 || out.Body.Locales[0].Code != "en" {
		t.Errorf("Locales = %+v, want just [en] on an empty database", out.Body.Locales)
	}
}

func TestListLocales_UnionsTranslationAndTaxonomyLocales(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	_, revisionID := seedExercise(t, ctx, pool, exerciseFixture{
		slug: "row", name: "Barbell Row",
		muscleGroup: "back", equipment: "barbell", category: "strength",
	})
	seedTranslation(t, ctx, pool, revisionID, "es", "Remo con barra", nil, nil, "human")
	seedTaxonomyTerm(t, ctx, pool, facetMuscleGroup, "back", map[string]string{"en": "Back", "fr": "Dos"})

	out, err := ListLocales(ctx, s, &ListLocalesInput{})
	if err != nil {
		t.Fatalf("ListLocales: unexpected error: %v", err)
	}

	codes := map[string]bool{}
	for _, l := range out.Body.Locales {
		codes[l.Code] = true
	}
	for _, want := range []string{"en", "es", "fr"} {
		if !codes[want] {
			t.Errorf("Locales = %+v, missing %q", out.Body.Locales, want)
		}
	}
	if out.Body.Locales[0].Code != "en" {
		t.Errorf("Locales[0] = %q, want en first", out.Body.Locales[0].Code)
	}
}
