# Go conventions for open-exercise-db

The Go equivalent of "MVC for a C# API" — what this project's structure is, and why.

Short version: Go has no framework-enforced layout the way ASP.NET does. The Go team
publishes [module layout guidance](https://go.dev/doc/modules/layout) but deliberately
doesn't mandate a structure, and the widely-cited `golang-standards/project-layout` repo is
**not** official and is
[actively disputed](https://github.com/golang-standards/project-layout/issues/117). So the
rules below are chosen, and the reasoning is written down so they can be revisited.

---

## 1. Repository layout

Follows the Go team's own [server-project layout](https://go.dev/doc/modules/layout):

```
backend/
  go.mod
  cmd/                          # entry points — thin, wiring only
    api-server/main.go
    import-freeexercisedb/main.go
  internal/                     # all server logic; not importable by other modules
    config/                     # env config
    model/                      # domain structs shared across layers
    database/                   # pgxpool lifecycle + migration runner (no queries)
      migrations/                # .sql migration files, //go:embed'd by database.go
    store/                      # ALL SQL lives here
    api/                        # huma operations: input/output types + handlers
    media/                      # image/video processing + object-storage interface
    export/                     # pre-generated /export blob builder
    seed/                       # bootstrap import + gap-fill enrichment
```

### Validated against real Go projects

This layout was checked against actual Go OSS rather than blog advice:

| Project | `cmd/` | `internal/` | `pkg/` | Organization |
|---|---|---|---|---|
| [**Miniflux**](https://github.com/miniflux/v2) — closest comparable | ❌ (one binary) | ✅ everything | ❌ | `internal/{api,storage,model,database,config}` |
| [Prometheus](https://github.com/prometheus/prometheus) | ✅ | tools only | ❌ | domain top-level (`promql`, `tsdb`, `scrape`) |
| [Tailscale](https://github.com/tailscale/tailscale) | ✅ | ✅ partial | ❌ | domain top-level |
| [Gitea](https://github.com/go-gitea/gitea) | ✅ | ❌ | ❌ | layers: `models/`, `services/`, `routers/` |
| [Woodpecker CI](https://github.com/woodpecker-ci/woodpecker) | ✅ | ❌ | ❌ | components: `server/`, `agent/`, `cli/` |
| [Grafana](https://github.com/grafana/grafana) | ❌ | ❌ | ✅ | `pkg/` (monorepo outlier) |

What that survey settles:

- **`pkg/` is genuinely dead** — 1 of 6, and that one is an atypical monorepo.
- **`cmd/` is near-universal once there is more than one binary** (4 of 6). It is ceremony
  with a single binary, which is why Miniflux correctly skips it.
- **Miniflux is the strongest precedent for this project** — self-hosted Go + Postgres +
  REST API, no ORM, comparable size — and it lands on `internal/api` + `internal/storage` +
  `internal/model` + `internal/database` + `internal/config`. That is this layout, modulo
  naming (`store` vs `storage`).
- **`internal/database` split from `internal/store` is taken directly from Miniflux**:
  connection lifecycle and schema migration have nothing in common with query code.
- **Layer-based packages inside `internal/` are well-precedented** — Gitea does the same
  thing at far larger scale with `models`/`services`/`routers`.

### Why `internal/`

Two concrete reasons, not cargo-culting:

1. **The compiler enforces it.** Nothing outside this module can import `internal/...`. On a
   public repo whose entire value proposition is a *public HTTP API*, that's the right
   signal: consume `/api/v1`, not our Go packages. It also means every package below
   `internal/` can be refactored freely without breaking a downstream Go consumer.
2. Go's official server layout recommends exactly this.

### Why `cmd/`

Multiple binaries are genuinely planned — the API server, the free-exercise-db bootstrap
importer, and (Phase 5) an export/bulk-dump generator. `cmd/` is justified the moment there's
more than one `main`. With a single binary it would be pure ceremony.

### What this project deliberately does NOT use

| Pattern | Why not |
|---|---|
| `pkg/` | Outdated pre-`internal/` convention. Anything in `pkg/` can live at the root instead. |
| `util/`, `common/`, `shared/`, `helpers/` | Naming smell — a package named for what it *isn't*. Put helpers next to their use, or in a package named for what it actually does (`applog`, `httpx`). |
| Layer-per-domain nesting (`internal/exercise/{handler,service,store}.go`) | Domain-centric packages scale better at 20+ domains. At ~8 (exercises, revisions, translations, submissions, media, taxonomy, users, auth) it's premature. Revisit if `internal/api` or `internal/store` gets unwieldy. |
| A DI framework (wire, fx, dig) | Constructor injection from `main` is enough and stays explicit. |

**Structure is added when it's needed, not ahead of time.** Refactoring package layout later
is normal and cheap in Go; over-structuring early is not.

---

## 2. Layering (the "MVC" answer)

Three layers, dependencies point one direction only:

```
cmd/api-server/main.go     wires everything, owns nothing
        │
        ▼
internal/api               HTTP: huma operations, input/output types, auth checks
        │                  never writes SQL
        ▼
internal/store             ALL SQL. Returns internal/model types.
        │                  never imports internal/api
        ▼
internal/model             plain structs. imports nothing from this project.

internal/database          pool + migrations. Used by main and store, but holds
                           no queries of its own.
```

Rules:

- **`internal/api` never writes SQL.** If a handler needs data, it calls a store method.
  Same split as lyftr's `controllers/` ↔ `stores/`, which exists for the same reason.
- **`internal/store` never imports `internal/api`.** Dependencies point downward only. If you
  ever need the reverse, the type belongs in `internal/model`.
- **Database types never leak into the API surface.** Handlers return response structs
  defined in `internal/api`, not raw store rows. This matters more here than usual: the
  published shape is a public contract that mirrors and downstream apps depend on, and it
  must not accidentally change because a column was renamed.
- **No separate "service" layer until there's real logic to hold.** For CRUD paths, a service
  that only forwards to the store is noise. Add one when business rules justify it — the
  submission-approval flow (validate → insert revision → repoint `current_revision_id` →
  regenerate export → purge cache) is the likely first genuine case, as
  `internal/api/moderation` logic or a dedicated `internal/moderation` package.

### Dependency injection

Constructors, wired in `main`. No framework.

```go
// cmd/api-server/main.go
pool := db.MustConnect(ctx, cfg.DatabaseURL)
stores := store.New(pool)
mediaStore := media.NewR2Store(cfg.R2)
api.Register(humaAPI, stores, mediaStore)
```

### Interfaces

**Define interfaces where they're consumed, not where they're implemented** — the single
most-violated Go rule by people arriving from C#/Java. Do *not* write an interface for every
struct.

Only two boundaries in this project genuinely warrant one:

```go
// internal/media/store.go — object storage, so R2 is a config value not a rewrite
type Store interface {
    Put(ctx context.Context, key string, r io.Reader, contentType string) error
    Get(ctx context.Context, key string) (io.ReadCloser, error)
}
```

...plus narrow interfaces at test seams where a real Postgres isn't wanted. Everything else
takes concrete types.

---

## 3. API layer specifics (huma)

`github.com/danielgtaylor/huma/v2` — OpenAPI 3.1 generated from Go types, so the published
spec cannot drift from the handlers. This is what makes "API-first" enforceable rather than
aspirational.

**Router: stdlib `net/http` (Go 1.22+ `ServeMux`) via huma's `humago` adapter — not Gin.**
Huma owns routing registration, request validation, and serialization, so Gin would add a
dependency while doing almost nothing. Dropping it keeps the binary smaller, which directly
serves the Cloud Run cold-start economics that justified choosing Go
(`docs/DESIGN.md` → Hosting). *(Gin was the original pick, before huma was chosen; DESIGN.md
has been updated to match.)*

Handlers are plain functions — no framework context object, so they're trivially unit-testable:

```go
type ListExercisesInput struct {
    Query       string `query:"q" doc:"Free-text search over name, aliases and taxonomy"`
    MuscleGroup string `query:"muscle_group"`
    Locale      string `query:"locale" doc:"BCP-47; falls back to English"`
    Page        int    `query:"page" minimum:"1" default:"1"`
    PerPage     int    `query:"per_page" minimum:"1" maximum:"100" default:"50"`
}

type ListExercisesOutput struct {
    Body struct {
        Exercises []Exercise `json:"exercises"`
        Total     int        `json:"total"`
    }
}

huma.Register(api, huma.Operation{
    OperationID: "list-exercises",
    Method:      http.MethodGet,
    Path:        "/api/v1/exercises",
    Summary:     "List and filter exercises",
    Tags:        []string{"exercises"},
}, h.ListExercises)
```

Conventions:

- **Struct tags carry the contract** — `doc:` on every non-obvious field; validation
  (`minimum`, `maximum`, `enum`) in tags so it lands in the spec *and* is enforced.
- **`OperationID` is kebab-case and stable** — client generators use it as the method name;
  renaming one is a breaking change for consumers.
- **Tag every operation** (`exercises`, `submissions`, `moderation`, `meta`) — that's what
  groups the docs UI.
- **Spec + docs are served, not committed**: `/api/v1/openapi.json` and `/docs`.
- **Errors use `huma.Error404NotFound(...)` etc.** so error responses are typed in the spec
  (RFC 9457 problem details) instead of ad-hoc JSON.

---

## 3a. Payload standard

One contract for every endpoint, so a consumer learns the envelope once. Enforced by
`internal/api/payloadstandard_test.go` — a new collection endpoint that skips the envelope
fails there, not in review.

### Single resource: bare, no envelope

```jsonc
GET /api/v1/exercises/{id}
{ "id": "…", "slug": "…", "muscle_group": "…", … }   // the object itself
```

**Not** `{"exercise": {…}}`. This was researched rather than assumed: of the Go APIs this
project benchmarks against, **Kubernetes, Gitea/Forgejo, HashiCorp Nomad and Miniflux all
return single resources unwrapped**. The one surveyed API that wraps is Open Food Facts —
and it wraps because it answers `HTTP 200` with `{"status": "failure"}` in the body, so it
*needs* a body-level status field. This API uses real status codes and `problem+json`
instead, so the envelope would carry cost with none of the benefit.

### Collections: named key + shared pagination block

```jsonc
GET /api/v1/exercises?page=2&per_page=50
{
  "exercises": [ … ],   // named for its contents, never "data" or "items"
  "total": 873,         // across all pages, ignoring pagination
  "page": 2,
  "per_page": 50
}
```

- The collection field is **named for what it holds** (`exercises`, `revisions`, `terms`,
  `translations`, `locales`) and is **always an array, never `null`** — build it with
  `make([]T, 0, n)`, never by appending onto a nil slice. `encoding/json` renders a nil slice
  as `null`, and a consumer calling `.map()` on the first search that matches nothing crashes.
  This shipped once; see `internal/api/emptycollections_test.go`.
- A field name means **one shape across the whole API**. `/exercises/{id}/translations`
  returns `translations`, not `locales`, because `/meta/locales` already returns `locales`
  with a different item type.
- Pagination is the embedded `api.Pagination` / `api.PageParams` pair (`internal/api/pagination.go`),
  never hand-rolled per endpoint. **Every paginated collection carries it**, including ones too
  small to need it today — a uniform contract beats a per-endpoint lookup, and taxonomy is
  exactly the table that grows quietly once contributions open.
- **One deliberate exception: `GET /exercises/export`.** It is a bulk dump whose entire purpose
  is to hand back the complete dataset in one response, so it carries `total` but no
  `page`/`per_page` — paginating it would defeat the endpoint. The exception is named here, and
  pinned by name in `TestEveryCollectionCarriesPagination`, precisely so it reads as a decision
  rather than as the one endpoint somebody forgot. Any *other* collection omitting the block is
  a bug.
- `Page` is capped (`maxPage`) as well as floored, and `Normalize` clamps both factors before
  `Offset` multiplies them. This is arithmetic, not product policy: an unbounded `page`
  overflows `int64`, and a negative `OFFSET` is a 500 that any unauthenticated caller can
  trigger with one query parameter.
- Pagination happens **in SQL** (`LIMIT`/`OFFSET` + a matching `Count…` method), never by
  slicing in the handler. The `Count` query's `WHERE` must match the list query's exactly, or
  `total` stops describing the page attached to it.
- **`ORDER BY` must be deterministic.** A non-unique sort key needs a tiebreak (`created_at
  DESC, id DESC`); without one Postgres may order two rows differently between the query for
  page 1 and the query for page 2, silently duplicating or dropping a row across the boundary.

Offset pagination rather than Kubernetes-style cursors: collections here are small, ordered by
a stable key, and served from a CDN. A cursor's advantage — stability under concurrent
insertion — costs opaque tokens that break the plain-URL caching this API's performance
story rests on.

### Errors: RFC 9457 + a stable `type`

```jsonc
HTTP/1.1 404 Not Found
Content-Type: application/problem+json
{
  "type": "https://openexercisedb.org/errors/not-found",  // switch on THIS
  "title": "Not Found",
  "status": 404,
  "detail": "exercise not found",                          // prose; may be reworded
  "errors": [ { "message": "…", "location": "query.per_page", "value": 101 } ]
}
```

- **`type` is the contract; `detail` is not.** `detail` is prose for humans and will be
  reworded — a client branching on it breaks silently. This mirrors Kubernetes carrying
  `Status.Reason` (machine-readable) beside `Status.Message` (human).
- Types are declared as constants in `internal/api/errors.go` so the full set stays
  enumerable from one place, and are applied by status in an override of `huma.NewError`.
  That override is what makes huma's *own* validation failures carry a type too — a
  per-call-site scheme only covers what someone remembered to annotate.
- **Every** non-2xx response is `application/problem+json`, including unknown routes
  (`api.NotFoundHandler`, mounted on `/` in `cmd/api-server`). Otherwise a typo'd path —
  a common first contact with this API — is the one response not in the documented format.
- Real HTTP status codes, always. Never `200` with a failure in the body.

### Requests

- **Reads take query parameters only.** No `GET` in this API has a request body.
- **Writes (Phase 2) take a bare resource body**, mirroring the bare single-resource
  response — `POST /exercises` takes exercise fields at the top level, not
  `{"exercise": {…}}`. Symmetry with the read shape is the point: what you `GET` is what
  you `POST`.
- **Write responses may add `warnings[]`** alongside the created resource, using the same
  item shape as the error `errors[]` array. This is the one idea worth taking from Open Food
  Facts' envelope: a submission can succeed *and* have something worth telling the
  contributor ("`muscle_group: pecs` was normalized to `chest`"). Warnings never change the
  status code — a 201 with warnings is still a success.

---

## 4. Store layer

- **One file per aggregate**: `store/exercises.go`, `store/submissions.go`,
  `store/translations.go`, `store/media.go`, `store/taxonomy.go`, `store/users.go`.
- **`pgxpool` handle, injected** — never a package-level global DB var.
- **`context.Context` is the first parameter of every method** that touches the database, and
  it gets passed through from the HTTP request. This is what makes client-disconnect
  cancellation and query timeouts work.
- **Explicit column lists in SELECTs, never `SELECT *`** — a schema change should break
  compilation or the scan, not silently reorder fields.
- **Multi-statement writes run in a transaction.** Non-negotiable for submission approval:
  inserting a revision and repointing `exercises.current_revision_id` must be atomic, or a
  crash between them corrupts the published pointer.
- **Return `pgx.ErrNoRows` upward**; the API layer maps it to a 404. The store doesn't know
  about HTTP.

---

## 5. Errors

- **Wrap with context**: `fmt.Errorf("approve submission %s: %w", id, err)`. Always `%w`, so
  `errors.Is`/`errors.As` still work upward.
- **Sentinel errors for conditions callers branch on**: `var ErrDuplicateSlug = errors.New(...)`.
- **No panics in request paths.** Panic only for genuinely-impossible programmer errors at
  startup (`MustConnect`).
- **Never leak raw DB errors to clients** — log the wrapped error server-side, return a typed
  huma error.

---

## 6. Testing

- **Table-driven tests are the default** (`tests := []struct{ name string; ... }{...}` with
  `t.Run(tt.name, ...)`).
- **Test files sit next to the code**: `internal/store/exercises_test.go`.
- **Store tests run against real Postgres**, not mocks — mocked SQL tests assert that the mock
  was called, which proves nothing about the query. Use a disposable database
  (testcontainers or a dedicated CI service container); the same reasoning lyftr applies with
  its real-SQLite tests.
- **A missing Postgres skips locally but fails in CI, on purpose.** `requirePostgres` (duplicated
  per-package — see below) calls `t.Skipf` so a contributor without Docker still gets a useful
  `go test ./...`. But an unconditional skip is a silent-success trap: point it at an
  unreachable database and the suite reports `ok` in under 3 seconds having exercised nothing —
  exactly what would happen if CI's Postgres service container ever failed to start. CI sets
  `OEDB_REQUIRE_POSTGRES=1` (`.github/workflows/ci.yml`), which flips every `requirePostgres`
  call from `t.Skipf` to `t.Fatalf`, so the suite can't pass by doing nothing.
- **Handler tests call the huma handler function directly** with an input struct — no HTTP
  round-trip needed for logic assertions.
- **Concurrency is tested, not assumed.** `internal/store/concurrent_submission_test.go`
  fires concurrent approve/revert calls and asserts `current_revision_id` never lands in a
  torn state. Postgres was chosen specifically for this guarantee
  (`docs/DESIGN.md` → Performance), so it gets a test that would fail without it.

---

## 7. Naming & style

- `gofmt` (non-negotiable) + `go vet` in CI. `golangci-lint` once there's enough code to
  warrant it.
- **Package names**: short, lowercase, no underscores, no plurals — `store` not `stores`,
  `model` not `models`. The package name is part of every call site (`store.Exercises`), so
  stuttering (`store.StoreExercises`) is the thing to avoid.
- **Receivers**: short and consistent — `func (s *ExerciseStore) List(...)`.
- **Exported identifiers get doc comments starting with the identifier name.**
- **Comments explain WHY, not WHAT** — same rule as lyftr's own CLAUDE.md.
- **Accept interfaces, return structs.**

---

## 8. Where this differs from lyftr, and why

| | lyftr | open-exercise-db |
|---|---|---|
| Layout | Flat `backend/{controllers,stores,models,routes}` | `internal/` + `cmd/`, per go.dev's server layout |
| Router | Gin | stdlib `net/http` + huma |
| DB | SQLite (`database/sql`) | Postgres (`pgxpool`) |
| API docs | none | huma-generated OpenAPI 3.1 + `/docs` |
| Interfaces | few | `media.Store` (object storage) + test seams only |

lyftr is a single-tenant self-hosted app with one binary; a flat layout is right for it. This
is a public multi-consumer API with several binaries and external consumers, so it follows
the Go team's server guidance instead. The **handlers-never-write-SQL** split is identical in
both — that part transfers directly.

---

## Sources

- [go.dev — Organizing a Go module](https://go.dev/doc/modules/layout) (official Go team guidance)
- [Huma v2 docs](https://huma.rocks/)
- [No-nonsense guide to Go project layout](https://laurentsv.com/blog/2024/10/19/no-nonsense-go-package-layout.html)
- [`golang-standards/project-layout` is not official — issue #117](https://github.com/golang-standards/project-layout/issues/117)
- Real-project survey: [Miniflux](https://github.com/miniflux/v2),
  [Prometheus](https://github.com/prometheus/prometheus),
  [Tailscale](https://github.com/tailscale/tailscale),
  [Gitea](https://github.com/go-gitea/gitea),
  [Woodpecker CI](https://github.com/woodpecker-ci/woodpecker),
  [Grafana](https://github.com/grafana/grafana)
