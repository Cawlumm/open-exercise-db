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

// ListTerms returns every term in facet, with its label resolved for
// locale — falling back to the English label, and then to the slug itself
// if even English is missing, so a gap in seed translations degrades to a
// readable value instead of a NULL scan failure. Per
// docs/PHASE1_BUILD_PLAN.md's WO-1 status note, terms are keyed on
// (facet, slug), never slug alone, since slugs like "other" recur across
// facets.
func (s *TaxonomyStore) ListTerms(ctx context.Context, facet, locale string) ([]model.TaxonomyTerm, error) {
	if locale == "" {
		locale = englishLocale
	}

	rows, err := s.pool.Query(ctx, `
		SELECT t.id, t.facet, t.slug,
		       COALESCE(loc.label, en.label, t.slug) AS label,
		       CASE WHEN loc.label IS NOT NULL THEN $2 ELSE 'en' END AS served_locale
		FROM taxonomy_terms t
		LEFT JOIN taxonomy_term_translations loc ON loc.term_id = t.id AND loc.locale = $2
		LEFT JOIN taxonomy_term_translations en ON en.term_id = t.id AND en.locale = 'en'
		WHERE t.facet = $1
		ORDER BY t.slug ASC`, facet, locale)
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

// ListLocales returns every locale with translated content anywhere in the
// dataset — the union of exercise_translations and taxonomy_term_translations
// locales — plus "en" unconditionally, since English is the base language
// written directly onto exercise_revisions and taxonomy_term_translations'
// own seed rows rather than appearing as a translation row itself. Backs
// GET /meta/locales (docs/DESIGN.md), added in WO-3: none of WO-2's read
// paths needed to enumerate locales, only resolve a single requested one.
func (s *TaxonomyStore) ListLocales(ctx context.Context) ([]model.Locale, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT locale FROM (
			SELECT locale FROM exercise_translations WHERE status = 'approved'
			UNION
			SELECT locale FROM taxonomy_term_translations
			UNION
			SELECT $1
		) AS all_locales
		ORDER BY (locale = $1) DESC, locale ASC`, englishLocale)
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
