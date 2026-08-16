package store

import "github.com/jackc/pgx/v5/pgxpool"

// Store bundles the per-aggregate stores behind a single injected pool, so
// callers (main, and later internal/api) wire one *Store instead of one
// struct per aggregate. Per docs/CONVENTIONS.md section 2/4, the pool is
// injected here rather than held as a package-level global. No interface
// wraps *pgxpool.Pool: per CONVENTIONS.md section 2, interfaces are defined
// only where a real seam is needed, and store tests run against real
// Postgres rather than a mock (section 6), so there's no consumer for one.
type Store struct {
	Exercises    *ExerciseStore
	Translations *TranslationStore
	Taxonomy     *TaxonomyStore
	Media        *MediaStore
	Apps         *AppStore
	Submissions  *SubmissionStore
}

// New wires every aggregate store against pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{
		Exercises:    &ExerciseStore{pool: pool},
		Translations: &TranslationStore{pool: pool},
		Taxonomy:     &TaxonomyStore{pool: pool},
		Media:        &MediaStore{pool: pool},
		Apps:         &AppStore{pool: pool},
		Submissions:  &SubmissionStore{pool: pool},
	}
}

// englishLocale is what exercise_revisions' own name/aliases/instructions
// columns and taxonomy_term_translations' seed rows are always written in.
// It's the fallback target for both the translation and taxonomy locale
// rules, so it's shared here rather than duplicated per file.
const englishLocale = "en"

// rowScanner is satisfied by both pgx.Row (QueryRow) and pgx.Rows (Query),
// so the scan helpers in exercises.go work against either without a pgx
// import of their own.
type rowScanner interface {
	Scan(dest ...any) error
}
