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

`0002_image_variants.sql` adds `exercise_image_variants`, turning an image's
storage key from a value every reader re-derived into one the database
records. It backfills a `full` rendition for each existing image from that
image's current `storage_key`, so **no object moves** — its only effect on
storage is schema-shaped, which is exactly why it lands before the
production R2 backfill rather than after it.

## Migrations run against a lock, and can run concurrently

`Migrate` holds a Postgres advisory lock across the whole check-and-apply, so
instances starting simultaneously (Cloud Run scale-from-zero, a revision
rollout) cannot race each other into `relation already exists`. No migration
file has to do anything to get this.

It does mean a slow migration blocks other instances' *startup* rather than
corrupting anything — the right trade, but worth knowing before writing one
that rewrites a large table.

## Why this directory lives under `internal/database/`, not `backend/migrations/`

`//go:embed` cannot reach outside (or above) the directory containing the
directive — it can only embed paths at or below the source file's own
directory. `internal/database/database.go` is what needs to embed these
files (see `docs/CONVENTIONS.md` section 1), so the files have to live
somewhere under `internal/database/` for `//go:embed migrations/*.sql` to
resolve at all. `backend/migrations/` was the original layout in the Phase 1
build plan's draft but doesn't compile with `go:embed`; this directory
supersedes it.
