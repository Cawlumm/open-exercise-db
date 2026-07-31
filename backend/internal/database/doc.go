// Package database owns the Postgres connection pool (pgxpool) and the
// migration runner. It deliberately does NOT contain queries — those live in
// internal/store.
//
// The split follows miniflux's internal/database vs internal/storage
// separation: connection lifecycle and schema migration have nothing in common
// with query code, and keeping them apart means the migration runner can be
// exercised without pulling in every query in the project.
//
// Migration .sql files live in internal/database/migrations/ and are
// embedded here — go:embed cannot reach outside (or above) the directory
// containing the directive, so they can't live at backend/migrations/ as
// originally sketched in docs/PHASE1_BUILD_PLAN.md; see
// internal/database/migrations/README.md.
package database
