# Contributing to open-exercise-db

Thanks for the interest — this is a background, single-maintainer project, so please read
the "moderation SLA" note below before submitting anything time-sensitive.

## Code contributions

Standard fork + PR flow:

1. Fork the repo, create a branch off `main`.
2. Match the existing layout (`backend/config|controllers|models|routes|stores|middleware`)
   and keep PRs scoped to one change.
3. `cd backend && go test ./...` must pass; add tests for new behavior.
4. Open a PR against `main` describing what changed and why.

## Data contributions (the actual point of this project)

Once Phase 2 ships (GitHub OAuth + submissions API), you'll be able to:

- **Submit a new exercise** — `POST /api/v1/submissions` with `type: "create"`, or the
  `/exercises/new` form once the web UI exists.
- **Suggest an edit** — `POST /api/v1/exercises/{id}/submissions`.
- **Add or improve a translation** — `POST /api/v1/exercises/{id}/translations`.

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

## Licensing note

By contributing data, you're agreeing it can be published under this project's data license,
[CC0 1.0](DATA_LICENSE) — public domain, no attribution required downstream (though we track
provenance internally as a courtesy). Code contributions fall under the project's
[MIT license](LICENSE).
