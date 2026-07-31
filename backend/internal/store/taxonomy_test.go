package store

import (
	"context"
	"testing"
)

func TestTaxonomyStore_ListTerms(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	seedTaxonomyTerm(t, ctx, pool, "muscle_group", "chest", map[string]string{"en": "Chest", "es": "Pecho"})
	seedTaxonomyTerm(t, ctx, pool, "muscle_group", "back", map[string]string{"en": "Back"}) // no es label
	// Same slug, different facet — must not collide per the WO-1 (facet, slug) correction.
	seedTaxonomyTerm(t, ctx, pool, "equipment", "other", map[string]string{"en": "Other"})
	seedTaxonomyTerm(t, ctx, pool, "category", "other", map[string]string{"en": "Other category"})

	t.Run("requested locale served when present", func(t *testing.T) {
		terms, err := s.Taxonomy.ListTerms(ctx, "muscle_group", "es")
		if err != nil {
			t.Fatalf("ListTerms: unexpected error: %v", err)
		}
		if len(terms) != 2 {
			t.Fatalf("len(terms) = %d, want 2", len(terms))
		}
		// slug ASC: back, chest
		if terms[0].Slug != "back" || terms[0].Label != "Back" || terms[0].Locale != "en" {
			t.Errorf("terms[0] = %+v, want back/Back/en (no es label, falls back)", terms[0])
		}
		if terms[1].Slug != "chest" || terms[1].Label != "Pecho" || terms[1].Locale != "es" {
			t.Errorf("terms[1] = %+v, want chest/Pecho/es", terms[1])
		}
	})

	t.Run("empty locale defaults to english", func(t *testing.T) {
		terms, err := s.Taxonomy.ListTerms(ctx, "muscle_group", "")
		if err != nil {
			t.Fatalf("ListTerms: unexpected error: %v", err)
		}
		for _, term := range terms {
			if term.Locale != "en" {
				t.Errorf("term %+v: Locale = %q, want en", term, term.Locale)
			}
		}
	})

	t.Run("slug scoped per facet, not globally unique", func(t *testing.T) {
		equipment, err := s.Taxonomy.ListTerms(ctx, "equipment", "en")
		if err != nil {
			t.Fatalf("ListTerms(equipment): %v", err)
		}
		category, err := s.Taxonomy.ListTerms(ctx, "category", "en")
		if err != nil {
			t.Fatalf("ListTerms(category): %v", err)
		}
		if len(equipment) != 1 || equipment[0].Slug != "other" || equipment[0].Label != "Other" {
			t.Errorf("equipment terms = %+v", equipment)
		}
		if len(category) != 1 || category[0].Slug != "other" || category[0].Label != "Other category" {
			t.Errorf("category terms = %+v", category)
		}
	})

	t.Run("unknown facet returns empty, not an error", func(t *testing.T) {
		terms, err := s.Taxonomy.ListTerms(ctx, "not-a-real-facet", "en")
		if err != nil {
			t.Fatalf("ListTerms: unexpected error: %v", err)
		}
		if len(terms) != 0 {
			t.Errorf("len(terms) = %d, want 0", len(terms))
		}
	})
}

// TestTaxonomyStore_UpsertTerm_IsIdempotent backs the importer's
// (internal/seed) requirement that re-running never duplicates or errors on
// an already-seeded taxonomy term.
func TestTaxonomyStore_UpsertTerm_IsIdempotent(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	id1, err := s.Taxonomy.UpsertTerm(ctx, "muscle_group", "chest")
	if err != nil {
		t.Fatalf("UpsertTerm (first): %v", err)
	}

	id2, err := s.Taxonomy.UpsertTerm(ctx, "muscle_group", "chest")
	if err != nil {
		t.Fatalf("UpsertTerm (second): %v", err)
	}

	if id1 != id2 {
		t.Errorf("second UpsertTerm returned a different id: %s vs %s", id2, id1)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM taxonomy_terms WHERE facet = 'muscle_group' AND slug = 'chest'`).Scan(&count); err != nil {
		t.Fatalf("count taxonomy_terms: %v", err)
	}
	if count != 1 {
		t.Errorf("taxonomy_terms row count = %d, want 1 (no duplicate)", count)
	}

	// Same (facet, slug) in a different facet must not collide, per the
	// WO-1 (facet, slug) correction.
	otherID, err := s.Taxonomy.UpsertTerm(ctx, "equipment", "chest")
	if err != nil {
		t.Fatalf("UpsertTerm (different facet, same slug): %v", err)
	}
	if otherID == id1 {
		t.Error("UpsertTerm returned the same id for a different facet with the same slug")
	}
}

func TestTaxonomyStore_UpsertTermTranslation_IsIdempotentAndOverwrites(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	termID, err := s.Taxonomy.UpsertTerm(ctx, "muscle_group", "chest")
	if err != nil {
		t.Fatalf("UpsertTerm: %v", err)
	}

	if err := s.Taxonomy.UpsertTermTranslation(ctx, termID, "en", "Chest"); err != nil {
		t.Fatalf("UpsertTermTranslation (first): %v", err)
	}
	if err := s.Taxonomy.UpsertTermTranslation(ctx, termID, "en", "Chest"); err != nil {
		t.Fatalf("UpsertTermTranslation (second, identical): %v", err)
	}

	terms, err := s.Taxonomy.ListTerms(ctx, "muscle_group", "en")
	if err != nil {
		t.Fatalf("ListTerms: %v", err)
	}
	if len(terms) != 1 || terms[0].Label != "Chest" {
		t.Fatalf("terms = %+v, want a single Chest term", terms)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM taxonomy_term_translations WHERE term_id = $1`, termID).Scan(&count); err != nil {
		t.Fatalf("count taxonomy_term_translations: %v", err)
	}
	if count != 1 {
		t.Errorf("taxonomy_term_translations row count = %d, want 1 (no duplicate)", count)
	}
}
