# Contributing to open-exercise-db

Thanks for the interest — this is a background, single-maintainer project, so please read
the "moderation SLA" note below before submitting anything time-sensitive.

## Code contributions

Standard fork + PR flow:

1. Fork the repo, create a branch off `main`.
2. Match the existing layout and keep PRs scoped to one change. Binaries live in
   `backend/cmd/` (`api-server`, importers); everything else is under `backend/internal/`
   (`api` handlers, `store` SQL, `model` types, `database`, `config`, `media`, `ratelimit`, ...).
   `docs/CONVENTIONS.md` explains the layering.
3. Needs Go 1.26 and, for the store/database tests, Postgres: `docker compose up -d postgres`
   (the compose service is named `postgres`; the default `.env.example` DSN points at it).
   Then `cd backend && go test ./...` must pass; add tests for new behavior. Without a database
   those tests skip locally; CI sets `OEDB_REQUIRE_POSTGRES=1` so they cannot skip there, and you
   can set it yourself to make a missing database a failure.
4. Open a PR against `main` describing what changed and why.

## Data contributions (the actual point of this project)

Sign in with GitHub (`GET /api/v1/auth/github`), then use `POST /api/v1/submissions` with a
`type` of:

- `create` — propose a new exercise.
- `edit` — suggest a change to an existing exercise (set `target_exercise_id`).
- `translation` — add or improve a translation (set `target_exercise_id` and `locale`).

`GET /api/v1/submissions/mine` lists your own. The full schema is in the generated OpenAPI
docs at `/docs`.

Every submission goes through human review before it's published — nothing you submit is
visible through the public API until a moderator approves it.

### Moderation SLA

Honest expectation: this is a single-maintainer, background project. Review turnaround is
best-effort, not guaranteed. If your submission sits for a while, that's not a rejection —
please be patient, or feel free to ping the PR/submission if it's been a long time.

### Description style guide

- Write instructions as complete, numbered steps — not sentence fragments.
- Prefer plain, direct language over marketing copy.
- Cite a `source_url` if you're adapting from another dataset, even when the text itself is
  original — provenance matters for an open data project.

### Translation style guide

- Translate meaning, not word-for-word — instructions should read naturally in the target
  language.
- If you're not a native/fluent speaker, say so in the submission notes so reviewers can
  weight that appropriately.
- Machine-translated drafts seeded at bootstrap are marked `origin: machine` and shown as
  "needs review" — correcting one of these into a human-verified translation is a great
  first contribution.

### Duplicate-detection guidance

Search existing exercises (including aliases) before submitting a new one. If you're unsure
whether something is a duplicate or a genuinely distinct variant, say so in the submission
notes — reviewers can merge/redirect if needed.

### Image/video guidelines

- Images and videos must be your own work or already CC0/permissively licensed — no
  watermarks, no content you don't have the rights to redistribute.
- Videos: keep them focused on the movement itself; there's a duration cap (see
  `docs/DESIGN.md` for the current limit).
- If you'd rather link than upload, an external URL (e.g. a YouTube demo) is accepted for
  videos.

## Licensing of contributions

**Data:** by submitting data (exercises, edits, translations, images, descriptions) you
dedicate your contribution to the public domain under [CC0 1.0](DATA_LICENSE). Only submit
what you wrote yourself or what is already CC0/public domain; do not copy from sources with
share-alike or attribution terms (for example wger, which is CC-BY-SA). Provenance is tracked
internally as a courtesy, but no attribution is required downstream.

**Code:** contributions to the code are licensed under the project's [MIT license](LICENSE).
