package model

// TaxonomyTerm is one controlled-vocabulary value within a facet
// (muscle_group, equipment, category, mechanic, force, level), with its
// label resolved for a requested locale. taxonomy_terms is keyed on
// (facet, slug) — see docs/PHASE1_BUILD_PLAN.md's WO-1 status note — since
// slugs like "other" and "none" legitimately recur across facets.
type TaxonomyTerm struct {
	ID     string `json:"id"`
	Facet  string `json:"facet"`
	Slug   string `json:"slug"`
	Label  string `json:"label"`
	Locale string `json:"locale"` // locale the label was actually served in; falls back to "en"
}

// Locale is one BCP-47 locale code with translated content somewhere in the
// dataset. Added in WO-3 to back GET /meta/locales (DESIGN.md's API list),
// which WO-2 didn't need since none of its read paths enumerate locales
// themselves — every other WO-2 method resolves *a* locale, never lists
// which ones exist. "en" is always included: it's the base language every
// exercise_revisions row's own name/aliases/instructions columns are
// written in, so it never appears as a row in a translations table.
type Locale struct {
	Code string `json:"code"`
}
