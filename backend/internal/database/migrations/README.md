# Migrations

Numbered, append-only `.sql` migration files, embedded and run by
`internal/database`.

Rules:

- **Append only.** Never edit a migration that has been applied anywhere — write a new one.
- **Numbered prefix**, zero-padded: `0001_initial_schema.sql`, `0002_add_taxonomy.sql`.
- **Indexes belong in the same migration as the table they index** — the `/exercises`
  filter columns are called out in `docs/DESIGN.md` and are not optional; they're the
  highest-traffic query path.

Phase 1 lands `0001_initial_schema.sql` covering all nine tables from
`docs/DESIGN.md`: `exercises`, `exercise_revisions`, `exercise_translations`,
`submissions`, `users`, `exercise_images`, `exercise_videos`, `taxonomy_terms`,
`taxonomy_term_translations`.

## Why this directory lives under `internal/database/`, not `backend/migrations/`

`//go:embed` cannot reach outside (or above) the directory containing the
directive — it can only embed paths at or below the source file's own
directory. `internal/database/database.go` is what needs to embed these
files (see `docs/CONVENTIONS.md` section 1), so the files have to live
somewhere under `internal/database/` for `//go:embed migrations/*.sql` to
resolve at all. `backend/migrations/` was the original layout in the Phase 1
build plan's draft but doesn't compile with `go:embed`; this directory
supersedes it.
