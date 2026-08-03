package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Cawlumm/open-exercise-db/internal/model"
)

// TaxonomyStore holds every query touching taxonomy_terms and
// taxonomy_term_translations.
type TaxonomyStore struct {
	pool *pgxpool.Pool
}

// FacetSlug is one (facet, slug) pair — the only globally meaningful way to
// name a taxonomy term, since slugs are unique per facet rather than globally
// (see the UNIQUE (facet, slug) constraint and its comment in the schema).
type FacetSlug struct {
	Facet string
	Slug  string
}

// UnknownSlugs returns the subset of pairs that name no taxonomy term.
//
// Exists so a filter value that matches nothing can be told apart from a filter
// value that is simply spelled wrong. Both produce an empty result set, and
// without this the API cannot distinguish them — so `?muscle_group=Chest`
// (right idea, wrong case) answered 200 with zero exercises, exactly like a
// legitimate query for a combination nothing satisfies.
//
// One query for every pair rather than one per filter: a list request can carry
// five taxonomy filters at once, and five extra round trips on the hot search
// path to validate input is a worse trade than the unnest below.
func (s *TaxonomyStore) UnknownSlugs(ctx context.Context, pairs []FacetSlug) ([]FacetSlug, error) {
	if len(pairs) == 0 {
		return nil, nil
	}

	facets := make([]string, len(pairs))
	slugs := make([]string, len(pairs))
	for i, p := range pairs {
		facets[i] = p.Facet
		slugs[i] = p.Slug
	}

	// The second EXISTS makes this fail *open* on a facet with no vocabulary at
	// all. Without it, an unseeded or partially-seeded taxonomy table means
	// every value is "unknown" and every filtered request is rejected — so a
	// deployment that had merely skipped taxonomy seeding would answer 422 to
	// all filtering and look comprehensively broken, rather than degrading to
	// the unvalidated behavior it had before. Validation is only meaningful
	// against a vocabulary that exists; where one does not, the caller gets the
	// benefit of the doubt.
	rows, err := s.pool.Query(ctx, `
		SELECT w.facet, w.slug
		FROM unnest($1::text[], $2::text[]) AS w(facet, slug)
		WHERE NOT EXISTS (
			SELECT 1 FROM taxonomy_terms t
			WHERE t.facet = w.facet AND t.slug = w.slug
		)
		AND EXISTS (
			SELECT 1 FROM taxonomy_terms t
			WHERE t.facet = w.facet
		)`, facets, slugs)
	if err != nil {
		return nil, fmt.Errorf("check taxonomy slugs: %w", err)
	}
	defer rows.Close()

	var unknown []FacetSlug
	for rows.Next() {
		var fs FacetSlug
		if err := rows.Scan(&fs.Facet, &fs.Slug); err != nil {
			return nil, fmt.Errorf("scan unknown taxonomy slug: %w", err)
		}
		unknown = append(unknown, fs)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate unknown taxonomy slugs: %w", err)
	}
	return unknown, nil
}

// CountTerms returns how many terms facet holds, ignoring pagination. Its
// WHERE clause is kept identical to ListTerms' below; if the two diverge, the
// reported Total stops describing the page it is attached to.
func (s *TaxonomyStore) CountTerms(ctx context.Context, facet string) (int, error) {
	var total int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM taxonomy_terms WHERE facet = $1`, facet).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("count taxonomy terms for facet %s: %w", facet, err)
	}
	return total, nil
}

// ListTerms returns one page of facet's terms, with each label resolved for
// locale — falling back to the English label, and then to the slug itself if
// even English is missing, so a gap in seed translations degrades to a
// readable value instead of a NULL scan failure. Per
// docs/PHASE1_BUILD_PLAN.md's WO-1 status note, terms are keyed on
// (facet, slug), never slug alone, since slugs like "other" recur across
// facets.
func (s *TaxonomyStore) ListTerms(ctx context.Context, facet, locale string, limit, offset int) ([]model.TaxonomyTerm, error) {
	if locale == "" {
		locale = englishLocale
	}

	// Every facet is small today (17 muscle groups, 13 equipment), so this
	// pagination buys nothing yet. It exists so the endpoint's contract
	// matches every other collection in the API rather than being a special
	// case a client has to learn — and because taxonomy is exactly the kind
	// of table that grows quietly once contributions open in Phase 2.
	// t.slug ASC is already unique within a facet (UNIQUE (facet, slug)), so
	// the ordering is deterministic across pages without a tiebreak column.
	rows, err := s.pool.Query(ctx, `
		SELECT t.id, t.facet, t.slug,
		       COALESCE(loc.label, en.label, t.slug) AS label,
		       CASE WHEN loc.label IS NOT NULL THEN $2 ELSE 'en' END AS served_locale
		FROM taxonomy_terms t
		LEFT JOIN taxonomy_term_translations loc ON loc.term_id = t.id AND loc.locale = $2
		LEFT JOIN taxonomy_term_translations en ON en.term_id = t.id AND en.locale = 'en'
		WHERE t.facet = $1
		ORDER BY t.slug ASC
		LIMIT $3 OFFSET $4`, facet, locale, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list taxonomy terms for facet %s: %w", facet, err)
	}
	defer rows.Close()

	var terms []model.TaxonomyTerm
	for rows.Next() {
		var term model.TaxonomyTerm
		if err := rows.Scan(&term.ID, &term.Facet, &term.Slug, &term.Label, &term.Locale); err != nil {
			return nil, fmt.Errorf("scan taxonomy term: %w", err)
		}
		terms = append(terms, term)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list taxonomy terms for facet %s: %w", facet, err)
	}

	return terms, nil
}

// localeUnion is the set every locale query below draws from, kept in one
// place so CountLocales and ListLocales can never disagree about what
// counts as a locale — a Total computed from a different set than the page
// it describes is worse than no Total at all.
const localeUnion = `
	SELECT locale FROM exercise_translations WHERE status = 'approved'
	UNION
	SELECT locale FROM taxonomy_term_translations
	UNION
	SELECT $1`

// CountLocales returns how many locales localeUnion yields, ignoring
// pagination. Sharing localeUnion with ListLocales is what keeps this Total
// describing the page it is attached to.
func (s *TaxonomyStore) CountLocales(ctx context.Context) (int, error) {
	var total int
	query := `SELECT count(*) FROM (` + localeUnion + `) AS all_locales`
	if err := s.pool.QueryRow(ctx, query, englishLocale).Scan(&total); err != nil {
		return 0, fmt.Errorf("count locales: %w", err)
	}
	return total, nil
}

// ListLocales returns one page of the locales with translated content anywhere
// in the dataset — the union of exercise_translations and
// taxonomy_term_translations locales — plus "en" unconditionally, since
// English is the base language written directly onto exercise_revisions and
// taxonomy_term_translations' own seed rows rather than appearing as a
// translation row itself. Backs GET /meta/locales (docs/DESIGN.md), added in
// WO-3: none of WO-2's read paths needed to enumerate locales, only resolve a
// single requested one.
func (s *TaxonomyStore) ListLocales(ctx context.Context, limit, offset int) ([]model.Locale, error) {
	// English sorts first regardless of page, then the rest alphabetically.
	// UNION already de-duplicates, so locale is unique and the ordering is
	// deterministic across pages without a tiebreak.
	query := `
		SELECT locale FROM (` + localeUnion + `
		) AS all_locales
		ORDER BY (locale = $1) DESC, locale ASC
		LIMIT $2 OFFSET $3`

	rows, err := s.pool.Query(ctx, query, englishLocale, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list locales: %w", err)
	}
	defer rows.Close()

	var locales []model.Locale
	for rows.Next() {
		var loc model.Locale
		if err := rows.Scan(&loc.Code); err != nil {
			return nil, fmt.Errorf("scan locale: %w", err)
		}
		locales = append(locales, loc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list locales: %w", err)
	}

	return locales, nil
}

// UpsertTerm ensures a taxonomy_terms row exists for (facet, slug) and
// returns its id, creating it if missing. Used by the importer
// (internal/seed/taxonomy.go) to populate the controlled vocabulary from the
// distinct facet values found in the imported set — idempotent by
// construction (docs/PHASE1_BUILD_PLAN.md WO-4), since ON CONFLICT always
// returns the existing row's id rather than erroring or duplicating.
func (s *TaxonomyStore) UpsertTerm(ctx context.Context, facet, slug string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO taxonomy_terms (facet, slug) VALUES ($1, $2)
		ON CONFLICT (facet, slug) DO UPDATE SET facet = EXCLUDED.facet
		RETURNING id`,
		facet, slug,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("upsert taxonomy term (%s, %s): %w", facet, slug, err)
	}
	return id, nil
}

// UpsertTermTranslation writes (or overwrites) the label for termID in
// locale. taxonomy_term_translations' primary key is (term_id, locale), so
// this is a plain upsert-on-conflict — re-running the importer with the same
// derived label is a no-op in effect, just a redundant write.
func (s *TaxonomyStore) UpsertTermTranslation(ctx context.Context, termID, locale, label string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO taxonomy_term_translations (term_id, locale, label) VALUES ($1, $2, $3)
		ON CONFLICT (term_id, locale) DO UPDATE SET label = EXCLUDED.label`,
		termID, locale, label,
	)
	if err != nil {
		return fmt.Errorf("upsert taxonomy term translation (%s, %s): %w", termID, locale, err)
	}
	return nil
}
