# Running open-exercise-db yourself

You only need this to develop the API or run a private copy. To use the data, read the
[snapshot or the hosted API](../README.md#use-the-data) instead.

Requirements: Docker with Compose, and Go 1.26 for the importer.

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
image — it's a one-off bootstrap/admin tool per [`CONVENTIONS.md`](CONVENTIONS.md)'s `cmd/` layout, run
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

For production (Cloud Run, Postgres, R2, the snapshot workflow), see
[`DEPLOYMENT.md`](DEPLOYMENT.md).
