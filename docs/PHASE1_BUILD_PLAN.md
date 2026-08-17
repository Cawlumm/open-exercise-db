# Phase 1 build plan — data model + bootstrap + read API

Execution plan for Phase 1 of `docs/DESIGN.md`, split into seven work orders that can be
picked up one at a time. Each has explicit scope, files, and acceptance criteria.

**End state of Phase 1:** a publicly usable, self-documenting read API serving ~800
bootstrapped exercises, with zero auth and zero UI. That alone already solves the
stalled-upstream problem for read access.

---

## Read before starting

1. `docs/CONVENTIONS.md` — layout, layering rules, huma patterns, store rules, test rules.
   **These are not suggestions; they're the review criteria.**
2. `docs/DESIGN.md` — the data model, API surface, and why each choice was made.

## Guardrails (apply to every work order)

- **`internal/api` never writes SQL. `internal/store` never imports `internal/api`.**
- **`context.Context` is the first parameter** of every store method.
- **No `SELECT *`** — explicit column lists.
- **`gofmt`, `go vet`, and `go test ./...` must pass** before a work order is considered done.
- **No new dependencies** beyond the ones named in WO-1 without flagging it first.
- **Don't build Phase 2 things** — no auth, no submission endpoints, no moderation. If a work
  order seems to need them, stop and ask.

## Dependency order

```
WO-1  deps + database + migrations
   │
   ├──▶ WO-2  models + store (read paths)
   │        │
   │        ├──▶ WO-3  huma read API + OpenAPI/docs
   │        │        │
   │        └──▶ WO-4  bootstrap importer ──┐
   │                                        │
   │                                        ▼
   └────────────────────────────────▶ WO-5  /export endpoint
                                            │
                                            ▼
                                      WO-6  CI + compose verification
                                            │
                                            ▼
                                      WO-7  rate limiting + API policy
```

WO-3 and WO-4 are independent of each other and can be done in either order once WO-2 lands.

WO-7 gates going genuinely public: with live lookup as the primary consumption model, every
consumer search is a request here, so limits must exist before the API is announced.

---

## WO-1 — Dependencies, database package, migrations

**Scope:** get a schema onto a real Postgres, idempotently.

### Dependencies to add

```
github.com/jackc/pgx/v5
github.com/danielgtaylor/huma/v2
```

That's it for Phase 1. Migration running is hand-rolled (see below) rather than pulling in
`golang-migrate` — the same call lyftr and Miniflux both make, and it keeps the dependency
surface and binary small, which is the Cloud Run cold-start argument from `docs/DESIGN.md`.

### Files

**`backend/internal/database/migrations/0001_initial_schema.sql`** — all nine tables.
(Location resolved during WO-1 — see the `//go:embed` note below.) Notes that matter:

- **Table creation order** is constrained by foreign keys: `taxonomy_terms` →
  `taxonomy_term_translations` → `users` → `exercise_images` → `exercise_videos` →
  `exercises` → `exercise_revisions` → (then `ALTER TABLE exercises` to add the
  `current_revision_id` FK) → `exercise_translations` → `submissions`.
- **`exercises` ↔ `exercise_revisions` is circular by design.** Create `exercises` with
  `current_revision_id UUID` and *no* FK, create `exercise_revisions`, then add the FK as
  `DEFERRABLE INITIALLY DEFERRED` so a create-exercise transaction can insert both rows.
- **`gen_random_uuid()`** is built in on Postgres 13+; no `pgcrypto` extension needed.
- **Taxonomy facets are stored on revisions as TEXT slugs, not UUID FKs.** `taxonomy_terms`
  is a lookup table supplying translated labels. Rationale: a composite FK on
  `(facet, slug)` is awkward, and plain string facets keep the public JSON shape simple for
  every consumer. Validity is enforced in the app layer against the seeded taxonomy. Write
  this tradeoff down in a comment in the migration.
- **`exercises.source_id TEXT UNIQUE`** (nullable) — the *upstream* identity (e.g.
  free-exercise-db's `id`), used only for import idempotency. It is provenance, not the
  public key: consumers key on `exercises.id`.
- **Status/role/type columns get `CHECK` constraints** matching the enums in `DESIGN.md`.
- **`exercise_videos` needs `CHECK (storage_key IS NOT NULL OR url IS NOT NULL)`** — a video
  is either hosted or linked.
- **`submissions` needs two conditional checks**: a non-`create` submission must have a
  `target_exercise_id`; a `translation` submission must have a `locale`.
- **`exercise_translations` uniqueness is partial**, since superseded rows accumulate:
  `CREATE UNIQUE INDEX ... ON exercise_translations (revision_id, locale) WHERE status = 'approved';`
- **Indexes** (from `DESIGN.md`, not optional — `/exercises` is the hot path):
  `exercises(slug)` unique, `exercises(status)`, `exercise_revisions(exercise_id, status)`,
  `submissions(status)`, `exercise_images(status)`, and the filter columns
  `exercise_revisions(muscle_group)`, `(equipment)`, `(category)`.
- Every table gets `created_at TIMESTAMPTZ NOT NULL DEFAULT now()`; mutable tables
  (`exercises`, `submissions`) also get `updated_at`.

**`backend/internal/database/database.go`**

- `Connect(ctx, dsn) (*pgxpool.Pool, error)` — pool with `MaxConns` capped low (5–10) per
  `DESIGN.md`'s free-tier connection note, plus a health-check ping.
- `Migrate(ctx, pool) error` — `//go:embed migrations/*.sql`, applies files in lexical order
  inside a transaction each, tracked in a `schema_migrations (version TEXT PRIMARY KEY,
  applied_at TIMESTAMPTZ)` table. Already-applied files are skipped.

> **Resolved during WO-1.** `//go:embed` cannot reach parent directories, so migrations live
> at `internal/database/migrations/` next to the directive rather than `backend/migrations/`.
> `docs/CONVENTIONS.md` reflects this.

### Acceptance criteria

- `docker compose up -d postgres` then running the migrator applies cleanly against an empty DB.
- **Running it twice is a no-op the second time** (this is the actual test).
- `internal/database/database_test.go` proves the above against a real Postgres.
- `gofmt`/`vet`/`test` clean.

### WO-1 status: DONE

Implemented and verified against real Postgres. One correction applied after review:
`taxonomy_terms` uniqueness is **`UNIQUE (facet, slug)`**, not a global `UNIQUE(slug)`. A
slug is only meaningful within its facet, and generic values (`other`, `none`,
`body-weight`) legitimately recur across facets — a global constraint would let whichever
facet seeded first permanently claim the word. WO-2's taxonomy store must key on
`(facet, slug)` accordingly.

---

## WO-2 — Models + store read paths

**Scope:** typed access to published exercise data. No HTTP.

### Files

- **`internal/model/`** — `exercise.go`, `revision.go`, `translation.go`, `taxonomy.go`.
  Plain structs with `json` tags. No behavior, no imports from this project.
- **`internal/store/store.go`** — `New(pool) *Store` returning a struct that holds the
  per-aggregate stores, wired once in `main`.
- **`internal/store/exercises.go`**:
  - `List(ctx, ExerciseFilter) ([]model.Exercise, int, error)` — filters on `q` (name +
    aliases, case-insensitive), `muscle_group`, `equipment`, `category`, `mechanic`,
    `level`; `page`/`per_page` pagination; returns total count for the response envelope.
    Joins `exercises` → its `current_revision_id` row. Only `status='published'`.
  - `Get(ctx, id) (model.Exercise, error)` — returns `pgx.ErrNoRows` when missing; the API
    layer maps that to 404, not the store.
  - `ListRevisions(ctx, exerciseID) ([]model.Revision, error)` — full history, newest first.
- **`internal/store/translations.go`**:
  - `GetForRevision(ctx, revisionID, locale) (model.Translation, error)`
  - `ListLocalesForExercise(ctx, exerciseID) ([]model.TranslationSummary, error)` — locale +
    completeness + `origin`, for `GET /exercises/{id}/translations`.
- **`internal/store/taxonomy.go`** — `ListTerms(ctx, facet, locale)`, falling back to the
  English label when a locale has no translation.

### Locale fallback rule (applies everywhere a `locale` is honored)

Per-field fallback, not all-or-nothing: if a translation row exists but its `instructions`
are empty, fall back to English for `instructions` only, not the whole record. Response
must indicate which locale was actually served and whether it was machine-origin.

### Acceptance criteria

- `internal/store/exercises_test.go` + `translations_test.go`, table-driven, against a real
  Postgres (a disposable database or CI service container — **not** mocked SQL, per
  `CONVENTIONS.md` section 6).
- Filter combinations and pagination boundaries covered.
- Locale fallback covered, including the partial-fallback case above.

### WO-2 status: DONE

Three design outcomes that later work orders must inherit:

1. **`model.ResolvedTranslation` is the type WO-3 renders**, not `model.Translation`. The
   per-field fallback rule above cannot be represented by a single raw DB row — one row is
   one locale with one `origin`. `ResolvedTranslation` carries per-field locale/origin so a
   response can honestly report an `es` name alongside `en` instructions.
   `model.Translation` still exists as the raw 1:1 row mapping.
2. **`Get` filters `status='published'`**, same as `List` — nothing unpublished is reachable
   through any read path.
3. **`ListLocalesForExercise` scopes to the current revision only.** A translation attached
   to a superseded revision is not shown for the live exercise.

**Do not add `::text` casts on UUID columns.** pgx v5 scans `uuid` → `string` and `uuid[]` →
`[]string` natively; verified directly against pgx v5.10.0. An early WO-2 draft added them as
boilerplate and they were removed. If a scan fails, the cause is something else — investigate
rather than reaching for a cast.

---

## WO-3 — huma read API + OpenAPI + docs

**Scope:** every `GET` in `DESIGN.md` except `/exercises/export` (that's WO-5). No auth.

### Files

- **`internal/api/api.go`** — `Register(api huma.API, s *store.Store)`; shared response
  envelope types; error mapping (`pgx.ErrNoRows` → `huma.Error404NotFound`).
- **`internal/api/exercises.go`** — `GET /exercises`, `/exercises/{id}`,
  `/exercises/{id}/revisions`, `/exercises/{id}/translations`.
- **`internal/api/meta.go`** — `GET /meta/muscle-groups`, `/equipment`, `/categories`,
  `/locales`, all honoring `?locale=`.
- **`cmd/api-server/main.go`** — replace the Phase 0 health handler: load config, connect,
  migrate, build stores, create the huma API over `http.NewServeMux` via the `humago`
  adapter, register, serve. Keep `/health`. Keep `main` thin.

### Requirements

- Operations follow `CONVENTIONS.md` section 3: stable kebab-case `OperationID`, `Tags`
  (`exercises`, `meta`), `doc:` tags on non-obvious fields, validation in struct tags.
- `per_page` capped (`maximum:"100"`, default 50) — this is a public endpoint.
- Spec at `/api/v1/openapi.json`, docs UI at `/docs`.
- **`Cache-Control` + `ETag` on every GET** (`DESIGN.md` → Performance). Long `max-age` with
  `stale-while-revalidate`; the CDN in front is what absorbs concurrent read load.
- Graceful shutdown on SIGTERM — Cloud Run sends it before killing the instance.

### Acceptance criteria

- Handler tests call the huma handler functions directly with input structs (no HTTP
  round-trip needed for logic assertions).
- `/docs` renders and lists every registered operation.
- Generated spec validates (`npx @redocly/cli lint` or equivalent).
- `curl` against a locally-seeded DB returns real data for each endpoint.

---

## WO-4 — free-exercise-db bootstrap importer

**Scope:** populate a fresh database with ~800 exercises, idempotently.

### Files

- **`internal/media/filesystem.go`** — a filesystem implementation of the existing
  `media.Store` interface. **Phase 1 needs this** because `DESIGN.md` requires re-hosting
  images rather than hotlinking GitHub, and R2 doesn't land until Phase 2. The interface
  exists precisely so this swap is a config change.
- **`internal/media/encode.go`** — decode → resize (cap ~800px) → re-encode as JPEG. This
  supersedes this line's original "normalize to webp": there is no maintained pure-Go webp
  *encoder* (`golang.org/x/image/webp` only decodes; every real encoder shells out to libwebp
  via cgo), and `backend/Dockerfile` builds with `CGO_ENABLED=0` into
  `gcr.io/distroless/static-debian12` specifically for the Cloud Run cold-start economics in
  `docs/DESIGN.md` -> Hosting — a cgo dependency would break that build outright. JPEG via the
  stdlib needs zero new dependencies and strips EXIF as a side effect of full decode/re-encode.
  The real `content_type` is stored on `exercise_images` (not assumed by callers), so this is a
  data value, not a hardcoded format — webp/AVIF can be revisited behind the same `media.Store`
  interface later. Resizing uses `golang.org/x/image/draw` (pure Go, no cgo).
- **`internal/seed/freeexercisedb.go`** — fetch
  `https://raw.githubusercontent.com/yuhonas/free-exercise-db/main/dist/exercises.json`,
  map each item to an exercise + first revision, download and re-host each image.
- **`internal/seed/taxonomy.go`** — derive the distinct facet values from the imported set
  and populate `taxonomy_terms` with English labels.
- **`cmd/import-freeexercisedb/main.go`** — flags: `--dry-run` (report only, write nothing),
  `--limit N` (for fast iteration).

### Requirements

- **Idempotent**: upsert on `exercises.source_id`. Re-running must not duplicate, and must
  not create a new revision when nothing changed.
- Imported rows: `status='approved'`, `submitted_by=NULL`, `license='Unlicense'`,
  `source_url='https://github.com/yuhonas/free-exercise-db'`.
- `instructions` stored as a **JSONB array of strings**, not a concatenated string — this is
  a deliberate difference from lyftr's current storage, so it can be diffed and translated.
- `slug` generated from name, deduped with a numeric suffix on collision.
- Image download failures are logged and skipped, not fatal — one bad asset must not abort
  an 800-item import.

### Acceptance criteria

- Against a fresh DB: ~800 exercises, each with a revision and `current_revision_id` set.
- **Run twice → identical row counts, no new revisions.**
- `--dry-run` writes nothing.
- Import completes without a fatal error when an image URL 404s (test with a stubbed source).

### WO-4 status: DONE

Implemented and verified against real Postgres and the live upstream dataset. Design decisions
later work orders should inherit:

- **Idempotency comparison lives in `internal/store` (`ExerciseStore.revisionUnchanged`), not
  `internal/seed`.** `UpsertImport` loads the current revision's content and compares it
  field-by-field (`reflect.DeepEqual`, which compares pointed-to values for the nullable
  `*string` fields, not pointer identity) against the incoming content inside the same
  transaction that would write a new revision. This is a plain, transparent comparison of real
  materialized values on both sides — not a hash or summary — so there's no room for a
  collision or a stale-cache false negative to hide a real change.
- **Image idempotency uses a deterministic storage key**
  (`exercises/<source-id>/<index>.jpg`), looked up via `MediaStore.GetImageByStorageKey` before
  ever downloading. This is what keeps a revision's `image_ids` — and therefore the whole
  content comparison above — stable across re-imports of unchanged upstream data; without it,
  every run would mint fresh random image ids and every exercise would look "changed" forever.
- **A changed revision supersedes the old one** (`exercise_revisions.status` flips
  `approved` -> `superseded`) rather than leaving every historical row `approved`. WO-1's schema
  defines the CHECK constraint but WO-2 (read-only) never had a reason to set it; WO-4 is the
  first writer, so it establishes this convention. The Phase 2 submission-approval flow should
  follow the same pattern.
- **Null upstream `equipment` maps to taxonomy slug `none`**, a real distinct value (~9% of
  free-exercise-db items, verified against the live dataset) — not defaulted to the unrelated
  `other` bucket some items legitimately use instead.
- **`internal/store`'s first write path required no changes to WO-2's existing read methods or
  their `*pgxpool.Pool` field types.** The whole create/update/repoint sequence runs inside one
  `ExerciseStore` method using its own `pool.Begin(ctx)`, so no cross-aggregate transaction
  plumbing (e.g. a shared `db` interface satisfied by both `*pgxpool.Pool` and `pgx.Tx`) was
  needed for this work order. Revisit if a future write path genuinely needs to compose calls
  across multiple aggregate stores inside one transaction.

---

## WO-5 — `/exercises/export`

**Scope:** the full-catalog bulk dump, for mirroring and offline-first consumers.

This is a general-purpose public-data endpoint, **not a compatibility shim for any one
consumer.** This project exists so that anyone building a fitness app — or just wanting free,
open exercise data — can use it. Consumers adapt to this API's shape; the API does not bend
to theirs. (An earlier draft of this plan specified matching lyftr's `models.Exercise` struct
field-for-field. That was wrong and has been removed.)

### Files

- **`internal/export/export.go`** — builds the full catalog blob using **this project's own
  canonical exercise shape: the same field names `/exercises` already returns.** A consumer
  that starts on the bulk dump and later moves to live lookups (or vice versa) should not
  have to rewrite its mapping.
  - `instructions` stays a **JSON array of strings**, matching `/exercises` and the database.
    Do not flatten it into a numbered `description` string — that was a lyftr-shaped artifact.
  - Include `id` (stable UUID — what mirroring consumers should key on) and `source_id`
    (upstream provenance), plus the per-revision `license` / `attribution_text` / `source_url`
    so the dump is self-describing about provenance, which a CC0 data project needs.
- **`internal/api/export.go`** — `GET /exercises/export`.

### Phase 1 vs Phase 2

`DESIGN.md` specifies the blob is **pre-generated to R2 and CDN-served**. R2 lands in Phase 2.
For Phase 1: build it once at startup and on demand, cache in memory with a strong `ETag`,
serve with long cache headers. Structure `internal/export` so the "where it's written" part is
swappable — Phase 2 changes the destination, not the builder. Note this explicitly in the
package doc so it isn't mistaken for the finished design.

WO-3 established the conditional-request pattern via huma's `conditional.Params`; reuse it
rather than reinventing. Because the blob is built once and cached, the double-marshal
concern documented on `etagValue` in `internal/api/api.go` does not apply here — compute the
ETag when the blob is built, not per request.

### Acceptance criteria

- Field names and types are identical to what `/exercises` returns for the same exercise —
  a test asserts this by comparing key sets between the two endpoints, so the two can't drift.
- A test pins the exact JSON key set, so any future rename fails loudly rather than silently
  breaking downstream mirrors.
- `ETag` changes when underlying data changes; `If-None-Match` returns 304 (including the
  RFC 9110 OWS list cases WO-3 already handles).
- Serving the dump does not re-query per request — verify the cache is actually used.

---

## WO-6 — CI + compose verification

**Scope:** make the loop reproducible.

- Add a Postgres service container to `.github/workflows/ci.yml` so store/database tests
  actually run in CI.
- Confirm `docker compose up -d` brings up Postgres + backend, migrations run, `/health` and
  `/docs` respond.
- Add `make` or a short `scripts/` entry for the common loop (up, migrate, import, test) —
  only if it's genuinely shorter than the raw commands.

### Acceptance criteria

- CI green on a clean checkout, including store tests against real Postgres.
- Fresh-clone path documented in `README.md` actually works start to finish.

---

## WO-7 — Public-API hardening: rate limiting, identification, abuse

**Scope:** make this safe to expose as a genuinely public API. Runs after WO-6.

**Why it's needed now, not in Phase 2:** under a mirror-only model each consumer hit us once
per re-sync. With live lookup (the primary model in `DESIGN.md`'s "Consuming this API"),
every search from every consumer — including every self-hosted instance — is a request here.
That changes rate limiting from housekeeping into a prerequisite for going public.

### Prior art to follow: Open Food Facts

OFF publishes real numbers, and they're the closest operational precedent for this project:

| OFF policy | Value |
|---|---|
| Read product `GET` | 15 req/min/IP (official docs; some community sources cite 100) |
| **Search queries** | **10 req/min/IP** |
| Facet queries | 2 req/min/IP |
| User-Agent | **Required**, format `AppName/Version (ContactEmail)` |
| Over limit | Denied access / IP ban, appealable by contacting the team |
| Bulk access | "Download the data as CSV or JSONL" rather than looping the API |

Two details worth copying deliberately:

1. **OFF scopes limits per end-user, not per app**, when requests originate from end users.
   That matters here: many self-hosted consumer instances can share an IP (corporate NAT,
   CGNAT), and a naive per-IP limit punishes them collectively.
2. **A 10 req/min search limit breaks naive type-ahead.** One user typing a single query
   per keystroke exceeds it alone. Set our search limit with that in mind, and say plainly
   in the docs that clients must debounce — this is the single most likely way a
   well-meaning consumer gets itself banned.

### Work

- **Tiered limits by cost**, not one global number: cheap cached reads (`/exercises/{id}`,
  `/meta/*`) get a generous limit; `/exercises` search gets a middle one sized for debounced
  type-ahead; `/exercises/export` gets a low one because it's the heaviest and is meant to be
  cached/mirrored, not polled.
- **Require a `User-Agent`** identifying the consumer, matching OFF's format. Reject or
  sharply limit generic/absent UAs. Document it prominently — a consumer that doesn't know
  the rule finds out by getting blocked.
- **Standard rate-limit response**: `429` with `RateLimit-Limit` / `RateLimit-Remaining` /
  `RateLimit-Reset` headers and `Retry-After`, so clients can back off correctly instead of
  guessing. Return a typed huma error so it lands in the OpenAPI spec.
  **Correction (post-implementation):** this bullet originally cited "RFC 9331" for the
  `RateLimit-*` header naming. That citation was wrong — RFC 9331 is *"The Explicit
  Congestion Notification (ECN) Protocol for Low Latency, Low Loss, and Scalable Throughput
  (L4S)"*, an unrelated transport-layer congestion-control spec. The actual source for this
  header naming is the IETF **`draft-ietf-httpapi-ratelimit-headers`** Internet-Draft (not
  yet an RFC as of implementation), specifically the three-header form (`RateLimit-Limit`/
  `RateLimit-Remaining`/`RateLimit-Reset`) used by its earlier revisions and still the
  form most widely deployed in practice (including by Open Food Facts). See
  `docs/API_POLICY.md`'s "Standards note" for the full correction as published.
- **Enforce at the edge where possible.** Cloudflare rate-limiting rules in front of the
  origin cost nothing on the free tier and absorb load before it reaches Cloud Run — the
  origin limiter is defense in depth, not the primary mechanism.
- **Decide the shared-IP story**, given point 1 above. Options to weigh: optional
  unauthenticated API keys purely for quota attribution (not access control), higher limits
  for identified consumers, or accepting per-IP and documenting it. Pick deliberately and
  write down why.
- **Document the policy publicly** in `README.md` + a `docs/API_POLICY.md`: limits, UA
  requirement, debounce guidance, "mirror the dump instead of polling", and how to ask for
  more. OFF's appealable-ban framing is a good model — this is a community project, not an
  adversarial one.
- **Abuse response runbook**: how to identify and block a bad actor, and how a legitimate
  consumer gets unblocked.

### Explicitly not in scope

Paid tiers, per-user auth quotas (Phase 2 territory, once accounts exist), and anything
requiring Redis — a Postgres-backed or in-memory limiter behind Cloudflare is sufficient at
this scale, consistent with `DESIGN.md`'s "explicitly skipped" list.

### Acceptance criteria

- Exceeding a limit returns `429` with correct `RateLimit-*` and `Retry-After` headers,
  verified by a scripted burst.
- Limits are configurable per environment (a test/staging instance must not be trivially
  self-DoSed).
- A missing/generic `User-Agent` is handled per the documented policy, with a test.
- `docs/API_POLICY.md` exists and the README links it.
- Load test from WO-6/`DESIGN.md` re-run with limits enabled, confirming legitimate
  debounced-client traffic is unaffected.

### WO-7 status: DONE

Implemented and verified against real Postgres plus a live `./cmd/api-server` run (scripted
burst, header inspection, `/docs` and `/api/v1/openapi.json` checks). Design outcomes and
corrections later work orders (or a Phase 2 revisit of rate limiting) should inherit:

- **The Cloudflare/origin split resolved as the design trap in this WO's own text predicted
  it should**: Cloudflare's edge rules (`docs/API_POLICY.md` → "Cloudflare configuration")
  are the real, globally-accurate enforcement; `internal/ratelimit`'s in-process
  `golang.org/x/time/rate`-backed limiter is an explicitly-documented per-instance backstop,
  not a second copy of the same guarantee. Its package doc comment states the N-instances-live
  → N×-the-configured-limit consequence directly rather than letting it be discovered later.
  No shared datastore (Redis, or a per-request Postgres check) was added — both `docs/DESIGN.md`
  and this WO rule that out explicitly at this project's scale, and it was not revisited.
- **Tiered by cost as specified**: `TierRead` (60 req/min, `/exercises/{id}` + `/meta/*`),
  `TierSearch` (60 req/min, `/exercises`), `TierExport` (5 req/min, `/exercises/export`), plus
  an orthogonal `Unidentified` tier (5 req/min) that replaces — not stacks with — an
  endpoint's normal tier when the caller doesn't send a conformant `User-Agent`. All four are
  independently configurable per environment via `internal/config.RateLimitConfig`
  (`OEDB_RATELIMIT_*` env vars), satisfying "a test/staging instance must not be trivially
  self-DoSed" without a code change.
- **User-Agent strictness landed as "sharply limit, never hard-reject"**: a missing or
  generic (library/browser-default) `User-Agent` routes to the `Unidentified` tier instead of
  being rejected outright. `docs/API_POLICY.md` documents the full rationale, including the
  browser-`fetch`-can't-set-`User-Agent` caveat (mirroring Open Food Facts' own guidance to
  proxy through a backend).
- **The RFC 9331 citation in this document's own original WO-7 text was wrong** — see the
  correction inline above and `docs/API_POLICY.md`'s "Standards note" for the full writeup.
  The `RateLimit-Limit`/`RateLimit-Remaining`/`RateLimit-Reset` header names implemented here
  follow the IETF `draft-ietf-httpapi-ratelimit-headers` Internet-Draft (still a draft, not an
  RFC, as of implementation), not RFC 9331.
- **A real (if minor) routing defect surfaced by this WO's own tests, and was fixed**:
  `internal/api.Register`'s registration order (exercise routes, including the `{id}`
  wildcard, before the literal `/exercises/export` route) worked in production only because
  `cmd/api-server`'s `humago`-over-`http.ServeMux` adapter resolves a literal path segment
  over a wildcard at the same depth regardless of registration order (verified directly
  against `net/http.ServeMux`). `humatest`'s simplified test router
  (`adapters/humaflow/flow`) has no such rule — it matches in registration order — and this
  WO's own `internal/api/ratelimit_test.go` is the first test in the package to register both
  route groups together over real HTTP (every earlier `*_test.go` file registered
  `registerExerciseRoutes`/`registerExportRoutes` separately, never on the same test API
  instance, and so never hit this). Fixed by registering `registerExportRoutes` before
  `registerExerciseRoutes` in `Register` — a no-op for production's router, and no longer
  order-dependent for any router. See the comment on `Register` in `internal/api/api.go` for
  the full explanation.
- **`golang.org/x/time` (specifically its `rate` subpackage) is the one new dependency added**,
  per this WO's own instruction to prefer it over hand-rolling a token bucket. Pure Go, no
  cgo — preserves the `CGO_ENABLED=0` distroless build `docs/DESIGN.md`'s Cloud Run cold-start
  numbers depend on, the same constraint `golang.org/x/image` (WO-4) was chosen under.

---

## Definition of done for Phase 1

- Fresh clone → `docker compose up -d` → import → `curl /api/v1/exercises` returns real data.
- `/docs` renders every endpoint; spec validates.
- `/exercises/export` serves the canonical shape, key-for-key identical to `/exercises`.
- Importer is idempotent; migrations are idempotent.
- CI green with real-Postgres tests.
- Every `GET` is rate-limited and `User-Agent`-gated per `docs/API_POLICY.md`; the origin
  backstop and Cloudflare edge rules are both documented, honestly scoped, and testable.
- **Not built yet, by design:** auth, submissions, moderation, media uploads via API, web UI.

## Open items to raise rather than guess

- The `//go:embed` path constraint in WO-1 (migrations directory location).
- Whether `secondary_muscles`/`aliases` should be `TEXT[]` or `JSONB` if array handling gets
  awkward in `pgx` — `TEXT[]` is the current call.
- Anything that appears to require a Phase 2 capability.
