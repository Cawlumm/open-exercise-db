package seed

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// englishLocale is the only locale the bootstrap import writes labels in.
// Bootstrap-time machine translation into other locales is a Phase 1.5
// concern (docs/DESIGN.md's "Seed completeness bar"), not this importer's.
const englishLocale = "en"

// noneEquipmentSlug is what a null upstream `equipment` value maps to.
// exercise_revisions.equipment is NOT NULL (docs/DESIGN.md's ER diagram),
// but ~9% of free-exercise-db's items (77 of 873, checked directly against
// the live dataset) have equipment: null — meaning "no equipment", which is
// a real, distinct value from the upstream "other" bucket, not an absence
// this importer should paper over by picking one arbitrarily.
const noneEquipmentSlug = "none"

// TaxonomyResult totals what PopulateTaxonomy wrote (or, in dry-run mode,
// found), for the importer's summary report.
type TaxonomyResult struct {
	TermsSeen int
}

// facetValue pairs a taxonomy slug with the human-readable upstream string
// it was derived from, so the English label written to
// taxonomy_term_translations doesn't have to be reverse-engineered from the
// slug (e.g. "middle-back" -> "middle back", not a re-hyphenation guess).
type facetValue struct {
	facet string
	slug  string
	label string
}

// PopulateTaxonomy derives every distinct (facet, slug) pair used by items
// and upserts a taxonomy_terms row plus an English
// taxonomy_term_translations row for each, per docs/PHASE1_BUILD_PLAN.md
// WO-4. Idempotent by construction: TaxonomyStore.UpsertTerm/
// UpsertTermTranslation are themselves upserts, so re-running never
// duplicates or errors on an already-seeded term.
//
// secondary_muscles draws from the same controlled vocabulary as
// muscle_group (docs/DESIGN.md's ER diagram: "muscle_group/equipment/
// category/etc." are all taxonomy_terms-backed facets; there's no separate
// "secondary_muscle" facet), so both primaryMuscles and secondaryMuscles
// feed the muscle_group facet here.
func PopulateTaxonomy(ctx context.Context, st *store.Store, items []fedExercise, dryRun bool) (TaxonomyResult, error) {
	seen := map[[2]string]facetValue{}
	add := func(facet, raw string) {
		if raw == "" {
			return
		}
		slug := slugify(raw)
		key := [2]string{facet, slug}
		if _, ok := seen[key]; !ok {
			seen[key] = facetValue{facet: facet, slug: slug, label: humanize(raw)}
		}
	}

	for _, item := range items {
		for _, m := range item.PrimaryMuscles {
			add(facetMuscleGroup, m)
		}
		for _, m := range item.SecondaryMuscles {
			add(facetMuscleGroup, m)
		}
		add(facetCategory, item.Category)
		add(facetLevel, item.Level)
		if item.Equipment != nil && *item.Equipment != "" {
			add(facetEquipment, *item.Equipment)
		}
		if item.Mechanic != nil && *item.Mechanic != "" {
			add(facetMechanic, *item.Mechanic)
		}
		if item.Force != nil && *item.Force != "" {
			add(facetForce, *item.Force)
		}
	}
	// Always present regardless of whether any item actually had a null
	// equipment value, so the facet exists even against a --limit slice
	// that happens not to include one.
	seen[[2]string{facetEquipment, noneEquipmentSlug}] = facetValue{facet: facetEquipment, slug: noneEquipmentSlug, label: "None"}

	values := make([]facetValue, 0, len(seen))
	for _, v := range seen {
		values = append(values, v)
	}
	// Deterministic order — map iteration order isn't, and a stable order
	// makes --dry-run output (and test assertions) reproducible.
	sort.Slice(values, func(i, j int) bool {
		if values[i].facet != values[j].facet {
			return values[i].facet < values[j].facet
		}
		return values[i].slug < values[j].slug
	})

	if dryRun {
		return TaxonomyResult{TermsSeen: len(values)}, nil
	}

	for _, v := range values {
		termID, err := st.Taxonomy.UpsertTerm(ctx, v.facet, v.slug)
		if err != nil {
			return TaxonomyResult{}, fmt.Errorf("upsert taxonomy term (%s, %s): %w", v.facet, v.slug, err)
		}
		if err := st.Taxonomy.UpsertTermTranslation(ctx, termID, englishLocale, v.label); err != nil {
			return TaxonomyResult{}, fmt.Errorf("upsert taxonomy term translation (%s, %s): %w", v.facet, v.slug, err)
		}
	}

	return TaxonomyResult{TermsSeen: len(values)}, nil
}

// humanize renders an upstream facet string (e.g. "middle back", "e-z curl
// bar") as a simple capitalized label. It's a Day-1 seed default, not a
// polished translation — docs/DESIGN.md's moderation flow is how a human
// contributor later improves it, the same way machine-translated content
// gets promoted/corrected rather than shipped as final.
func humanize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
