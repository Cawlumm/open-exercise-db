<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/logo-dark.svg">
    <img src="docs/assets/logo-light.svg" alt="open-exercise-db" height="56">
  </picture>
</p>

<p align="center">
  A community-maintained, open exercise database — <em>OpenFoodFacts, but for fitness.</em>
</p>

<p align="center">
  <img alt="code license" src="https://img.shields.io/badge/code%20license-MIT-blue">
  <img alt="data license" src="https://img.shields.io/badge/data%20license-CC0%201.0-lightgrey">
  <img alt="status" src="https://img.shields.io/badge/status-phase%202-yellow">
</p>

---

## Why this exists

Fitness apps that want a free exercise catalog today mostly lean on
[`yuhonas/free-exercise-db`](https://github.com/yuhonas/free-exercise-db) — a great
public-domain dataset of ~800 exercises, but its GitHub repo has PRs sitting unmerged for
months, with no active review queue behind it.

Other prior art exists — [wger.de](https://wger.de) (a full self-hosted fitness tracker with
its own CC-BY-SA exercise wiki), [exercemus/exercises](https://github.com/exercemus/exercises)
(a static dataset aggregating wger + free-exercise-db), and commercial APIs like ExerciseDB —
but none combine an open community submission + moderation queue, translations, versioned/
revertible revisions, a CC0 data license, and an API-first documented surface. That's the gap
this project fills.

Full design rationale, data model, and phased build plan: **[`docs/DESIGN.md`](docs/DESIGN.md)**.

## Status

Phase 1 (data model, read API, the free-exercise-db bootstrap import, and public-API
hardening) is done and deployed. Phase 2 — GitHub sign-in, app credentials, the submission
queue, moderation, payload validation and image upload — is implemented and runnable via the
Quickstart below. There is no web UI yet, so moderation is API-only.
Every `GET` endpoint is rate-limited and requires a `User-Agent`; see
[`docs/API_POLICY.md`](docs/API_POLICY.md) for the published limits, the `User-Agent` format,
debounce guidance for search, and how to request a higher limit.
See [`docs/DESIGN.md`](docs/DESIGN.md) and [`docs/PHASE1_BUILD_PLAN.md`](docs/PHASE1_BUILD_PLAN.md)
for the full phased build plan and current work-order status.

## Quickstart

This sequence has been run start-to-finish on a fresh clone, including the containerized
backend — not just `go run`.

```bash
git clone https://github.com/Cawlumm/open-exercise-db.git
cd open-exercise-db
cp .env.example .env          # Windows: copy .env.example .env
docker compose up -d --wait   # builds the backend image, waits for both containers to be ready
```

`docker compose up -d --wait` brings up Postgres and the API, and blocks until both are
considered ready (Postgres via its healthcheck; migrations run automatically against it on the
API's first boot — see `internal/database.Migrate` in `cmd/api-server/main.go`). At this point
`/health` and `/docs` already respond, but `/exercises` returns an empty list — the database
schema exists, but it has no rows until you run the bootstrap importer once:

```bash
cd backend
DATABASE_URL="postgres://openexercisedb:openexercisedb@localhost:5432/openexercisedb?sslmode=disable" \
  go run ./cmd/import-freeexercisedb
```

```powershell
# PowerShell equivalent
cd backend
$env:DATABASE_URL = "postgres://openexercisedb:openexercisedb@localhost:5432/openexercisedb?sslmode=disable"
go run ./cmd/import-freeexercisedb
```

This fetches `yuhonas/free-exercise-db`'s ~800 exercises and re-hosts their images locally
under `backend/data/media` (gitignored) — it downloads real images over the network, so expect
it to take a few minutes on first run. It's idempotent: re-running it upserts on
`exercises.source_id` rather than duplicating (`--dry-run` reports without writing; `--limit N`
imports a subset for fast iteration). The importer isn't baked into the `backend` container
image — it's a one-off bootstrap/admin tool per `docs/CONVENTIONS.md`'s `cmd/` layout, run
against the same Postgres the compose stack already published on `localhost:5432`, so
`DATABASE_URL` here points at `localhost`, not the `postgres` service-network hostname the
container itself uses.

Re-running it is also how you **repair missing images**. Image rows live in Postgres while
the bytes live in `MEDIA_DIR` (or an R2 bucket), and the two can be lost independently — a
media directory that wasn't on a persistent volume, a recreated bucket. The importer checks
that each image's object actually exists rather than trusting the row, and re-downloads the
ones that don't, keeping their existing ids so no revision changes. A run that repairs
anything says so:

```
images(uploaded=0 reused=0 repaired=1746 failed=0)
WARNING: 1746 image(s) had a database row but no stored object, and were re-downloaded.
```

That warning means storage lost data Postgres didn't — the import fixed the symptom, but
check `MEDIA_DIR`/`R2_BUCKET` before it happens again.

Once the import finishes, the API is live:

```bash
curl http://localhost:8080/health
curl http://localhost:8080/api/v1/exercises
```

The API is served at `http://localhost:8080/api/v1`, with interactive docs at
`http://localhost:8080/docs` and the raw OpenAPI 3.1 spec at
`http://localhost:8080/api/v1/openapi.json`.

**On `PORT`**: both `.env.example` and `docker-compose.yml` define `PORT=8080`. Only the
compose file's `ports:` mapping actually reads your `.env` value (`"${PORT:-8080}:8080"`) — it
picks which **host** port publishes the API. The container's own `PORT` env var is fixed at
`8080` in `docker-compose.yml` regardless, so the API always listens on `8080` *inside* the
container. Net effect: setting `PORT=9090` in `.env` makes the API reachable at
`localhost:9090` (verified) without touching anything else — it's a host-port override, not an
in-container one, which is what you want 95% of the time (avoiding a local port clash), but
worth knowing if `8080` shows up somewhere unexpected.

## API summary

Full spec: `GET /api/v1/openapi.json`, interactive docs at `/docs`.

```
GET  /api/v1/exercises              # search/filter/list, public, no auth
GET  /api/v1/exercises/{id}         # single exercise, ?locale= for translations
GET  /api/v1/exercises/export       # full catalog dump, for mirrors + offline consumers
GET  /api/v1/images/{id}            # image bytes; only approved images are public
POST /api/v1/images                 # upload an image, re-encoded on arrival (app credential)
POST /api/v1/submissions            # propose a create/edit/translation (auth required)
GET  /api/v1/submissions/mine       # your own submissions and their outcomes
GET  /api/v1/auth/github            # sign in with GitHub (people, not apps)
GET  /api/v1/admin/submissions      # moderation queue (moderator+ role)
```

Nothing a contributor sends is public when they send it. A submission waits in the queue, and
an uploaded image is fetchable only by a moderator until the submission using it is approved —
so an unreviewed photograph cannot be served from this API by anyone who knows its id.

**Two ways to consume it**, pick whichever fits: query the API live at use time (like apps
consume Open Food Facts), or pull the bulk dump and mirror it locally. Live is simplest and
always current; the dump is right if you need offline operation or don't want a runtime
dependency on this service. Key on `id` (a stable UUID), not `slug` or `name` — those can
change when an exercise is edited upstream. Details in
[`docs/DESIGN.md`](docs/DESIGN.md#consuming-this-api).

**Every request needs a `User-Agent`** in the form `AppName/Version (ContactEmail)`, and every
endpoint is rate-limited (tiered by cost — cheap cached reads get a generous budget, the full
export gets the tightest one). See [`docs/API_POLICY.md`](docs/API_POLICY.md) for the exact
numbers, debounce guidance for live search, and how to ask for a higher limit.

See [`docs/DESIGN.md`](docs/DESIGN.md#api-rest-json-apiv1) for the complete endpoint list.

## Contribute

- **Code**: standard fork + PR flow, see [`CONTRIBUTING.md`](CONTRIBUTING.md).
- **Exercise data**: submit new exercises, edits, or translations through the API — every
  submission goes through human review before publication, and images are re-encoded on upload
  (which also strips EXIF, so a gym photo does not publish the GPS coordinates it was taken at).
  See [`CONTRIBUTING.md`](CONTRIBUTING.md) for style guides and the moderation SLA. A web UI is
  Phase 3.
- **From inside your own app**: apps can hold their own credentials and submit on behalf of
  their users, who need no account here — the model Open Food Facts uses, and how most of its
  data actually arrives. Submissions carry the app plus an opaque per-user handle, so one bad
  contributor can be blocked without suspending the app. Review still applies either way.

This is a background, single-maintainer project — moderation turnaround is best-effort, not
guaranteed. Patience appreciated.

## Stack

Go + [huma v2](https://huma.rocks/) (OpenAPI 3.1 generated from types) + Postgres (Supabase)
backend, React + Vite + TypeScript web UI (Phase 3), Google Cloud Run + Cloudflare (R2,
Pages, edge cache) hosting.

- Full rationale and alternatives weighed: [`docs/DESIGN.md`](docs/DESIGN.md)
- Go project layout, layering, and code conventions: [`docs/CONVENTIONS.md`](docs/CONVENTIONS.md)
- Running it in production — GCP setup, secrets, bootstrap, rollback: [`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md)

## Sources and related projects

open-exercise-db stands on other people's open work. Go check these out:

- **[yuhonas/free-exercise-db](https://github.com/yuhonas/free-exercise-db)** (Unlicense):
  the day-one seed. Its ~870 exercises and their images were imported as-is, and every
  imported revision keeps its `license` and `source_url`.
- **[wrkout/exercises.json](https://github.com/wrkout/exercises.json)** by Ollie Jennings:
  the original dataset free-exercise-db restructured.
- **[wger](https://github.com/wger-project/wger)**: a full self-hosted fitness tracker with a
  community exercise wiki. Its data is CC-BY-SA, which is not compatible with CC0, so nothing
  is imported from it; it is only a reference for spotting gaps.
- **[exercemus/exercises](https://github.com/exercemus/exercises)**: a static dataset that
  aggregates wger and free-exercise-db. Reference only, for the same reason.
- **[Open Food Facts](https://world.openfoodfacts.org)**: the model this project follows for
  community submissions, moderation and bulk data dumps.
- **Spanish (`es`) translations** are machine-drafted (by an LLM) and marked as such in the API
  (`*_machine_origin`) until a person reviews them.
- **Logo** wordmark is set in [Inter](https://rsms.me/inter/) (SIL Open Font License),
  outlined to paths.

Used by [Lyftr](https://github.com/Cawlumm/lyftr), a self-hosted workout tracker.

If you hold rights to something in the catalog and want it credited differently or removed,
open an issue.

## License

Code is [MIT](LICENSE). Exercise **data** (database contents, API responses, exports) is
[CC0 1.0](DATA_LICENSE) — public domain, no attribution required, though individual entries
may carry provenance metadata as a courtesy.
