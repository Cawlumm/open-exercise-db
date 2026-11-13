# Deployment

How `open-exercise-db` runs in production, and why each choice is the one it is.

Production is **Cloud Run** (the API) + **Supabase** (Postgres) + **Cloudflare R2** (images),
per `docs/DESIGN.md`'s Hosting section. Every merge to `main` deploys automatically via
`.github/workflows/deploy.yml`.

```
GitHub (merge to main) → CI → deploy.yml → Artifact Registry (us-east4)
                                              ↓
                                   Cloud Run  oedb-api  (us-east4)
                                     ├── Supabase Postgres (us-east-1, session pooler)
                                     └── Cloudflare R2   (bucket oedb-media-dev)
```

The live resources:

| | |
|---|---|
| GCP project | `open-exercise-db` (number `324202275612`) |
| Region | `us-east4` |
| Artifact Registry | `oedb` |
| Cloud Run service | `oedb-api` |
| Deploy identity | `gh-deploy@open-exercise-db.iam.gserviceaccount.com` |
| Runtime identity | `oedb-run@open-exercise-db.iam.gserviceaccount.com` |
| Supabase project | `<project-ref>` — PostgreSQL 17.6, `us-east-1`, database `postgres` |
| Supabase org | `<org-ref>` |
| Keepalive | Cloud Scheduler job `oedb-supabase-keepalive` (`us-east4`) — see rule 3b |
| R2 bucket | `oedb-media-dev` |

Connection endpoints and account identifiers are deliberately not written down here — this
file is destined to be public. They live in Secret Manager (`oedb-database-url`) and in the
repository's Actions variables (`R2_ACCOUNT_ID`, `R2_BUCKET`).

Region pairing is deliberate. Every API request makes at least one database round trip and
none of that is cacheable, so origin↔database latency is the dominant term. `us-east4`
(Ashburn) and Supabase's `us-east-1` (N. Virginia) are ~1–2 ms apart; `us-central1` paired
with `aws-us-east-2` would be ~15 ms *per query*, which the export path pays many times over.
Artifact Registry lives in the same region so an image pull is neither slow nor billed as
cross-region egress.

---

## Read this before you change anything

Five rules, each of which has a specific failure it prevents.

**1. Use Supabase's SESSION pooler (port 5432), never the transaction pooler (6543) and
never the direct endpoint.** All three are offered on the connection page and only one works
here.

`database.Migrate` (`backend/internal/database/database.go`) takes a *session-level*
`pg_advisory_lock` on a dedicated pooled connection and releases it on that same connection.
A transaction-mode pooler — Supabase's port 6543, as PgBouncer/Supavisor in transaction mode —
lets the lock and the unlock land on different server connections. That silently defeats the
concurrent-startup race the lock exists to close and leaks a held lock. It also breaks the
prepared statements pgx uses by default.

The direct endpoint (`db.<project-ref>.supabase.co:5432`) does not have that problem, but it is
**IPv6-only**, and Cloud Run egress is IPv4 — so it is unreachable from production regardless.

That leaves the session pooler: `aws-0-us-east-1.pooler.supabase.com:5432`, user
`postgres.<project-ref>`. Session mode holds one server connection for the life of the client
session, so advisory locks and prepared statements both behave. Verify after any change to the
DSN, because the failure is silent until two instances start at once:

```bash
psql "$DATABASE_URL" -c "SELECT pg_advisory_lock(1); SELECT count(*) FROM pg_locks WHERE locktype='advisory'; SELECT pg_advisory_unlock(1);"
# count must be 1. A transaction pooler returns 0.
```

The hostname prefix is per-project and not guessable — `aws-1-us-east-1...` does not resolve
for this project. Read it off the dashboard rather than assuming.

**2. Every migration must be backward-compatible with the previous revision.** Migrations run
in-process on every boot (`cmd/api-server/main.go`), not as a separate release step. So the
moment a new revision starts, the schema has changed *and the old revision is still serving
traffic against it*. A traffic rollback (below) rolls back code, never the database. Expand
then contract: add nullable columns and new tables freely; never drop or rename a column in
the same release that stops using it. This is the single most important operational rule here,
and it is a direct consequence of in-process migration plus traffic-based rollback.

**3. Do not add a liveness probe.** `/health` pings the database with a 2-second timeout and
returns 503 on failure. Cloud Run's response to a failed liveness probe is to *restart the
container*, which turns a three-second database blip into a restart and a flapping database
into a restart storm. `/health` is also not rate-limited and consumes a pool connection per
call, so keep it off short-interval monitoring too. Monitor `GET /`, which touches no database.

**3b. The database pauses after 7 days of inactivity, and a Cloud Scheduler job is what stops
it.** Supabase suspends a free project after a week with no activity, and it does not wake on
its own — restoring it is a manual click in the dashboard. For a public API that is idle for
long stretches by design, that means the service stays down until somebody notices.

`oedb-supabase-keepalive` (Cloud Scheduler, `us-east4`, `0 12 */2 * *`) prevents it by calling
`GET /api/v1/exercises?per_page=1` every two days.

Two things about that job are load-bearing:

- **It must hit an endpoint that queries Postgres.** `GET /` would keep Cloud Run warm and do
  nothing at all for the database, which is the thing that pauses. Verify a change by watching
  the counter move rather than by trusting the HTTP status:
  ```bash
  psql "$DATABASE_URL" -t -c "SELECT tup_returned FROM pg_stat_database WHERE datname=current_database()"
  ```
- **It does not live in GitHub Actions.** A scheduled workflow is disabled automatically after
  60 days of repository inactivity once the repo is public — so the keepalive would switch
  itself off during exactly the quiet stretch it exists to cover. Cloud Scheduler has no such
  coupling to how often anyone commits.

Unlike the metered compute this project previously ran on, keeping this database awake costs
nothing: a free Supabase project is always-on until it pauses. The keepalive is protecting
against the pause, not managing a budget.

**3c. Never let `anon` or `authenticated` hold grants in `public`.** Supabase ships a
PostgREST API in front of every project and, by default, grants `anon` and `authenticated`
full DML — `SELECT, INSERT, UPDATE, DELETE, TRUNCATE` — on every table in `public`, with RLS
disabled. The anon key is *public by design*; it ships inside client apps. Left as-is, anyone
holding it could read `apps` credential hashes, read submissions that have not been through
moderation, or `TRUNCATE` the published catalog.

This API never uses PostgREST — it connects to Postgres directly as `postgres`. So the correct
state is no grants at all:

```sql
REVOKE ALL PRIVILEGES ON ALL TABLES    IN SCHEMA public FROM anon, authenticated;
REVOKE ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA public FROM anon, authenticated;
REVOKE ALL PRIVILEGES ON ALL FUNCTIONS IN SCHEMA public FROM anon, authenticated;
REVOKE USAGE ON SCHEMA public FROM anon, authenticated;

-- Migrations CREATE TABLE as `postgres` on every boot. Without this, the next new
-- table is granted to anon again and the hole reopens on a deploy nobody connects
-- to permissions.
ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON TABLES    FROM anon, authenticated;
ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON SEQUENCES FROM anon, authenticated;
ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON FUNCTIONS FROM anon, authenticated;
```

RLS is enabled on every table as a second, independent control: `postgres` has `rolbypassrls`
so the API is unaffected, while `anon` does not, so a grant restored by hand or by a
Supabase-side default still denies. Audit with:

```sql
SELECT count(*) FROM information_schema.role_table_grants
 WHERE table_schema='public' AND grantee IN ('anon','authenticated');   -- must be 0
SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname='public' AND c.relkind='r' AND NOT c.relrowsecurity;   -- must be 0
```

Re-run that audit after adding a migration. It is the one check that a normal deploy will not
perform for you.

**4. `--set-env-vars` and `--set-secrets` are full replacements, not merges.** gcloud removes
everything already set before applying what you passed. Both lists must therefore always be
complete — which is why `deploy.yml` spells out the entire configuration rather than
inheriting whatever the last deploy happened to leave behind. In particular all four `R2_*`
values must arrive together: `media.NewStore` treats a partial set as a hard startup error,
not a fallback (see "Failure modes" below).

---

## One-time setup

Everything in this section is run by hand, once. Nothing here lives in git — this document is
the record.

```bash
export PROJECT_ID="open-exercise-db"
export REGION="us-east4"
export AR_REPO="oedb"
export SERVICE="oedb-api"
export GH_REPO="Cawlumm/open-exercise-db"
```

### Project and APIs

```bash
gcloud projects create "$PROJECT_ID" --name="open-exercise-db"
# `gcloud billing`, not `gcloud beta billing`: the beta component is not
# installed by default, and installing it mid-script fails in non-interactive
# mode with a CLOUDSDK_PYTHON error. The GA command does the same job.
gcloud billing projects link "$PROJECT_ID" --billing-account="<ACCOUNT>"
gcloud config set project "$PROJECT_ID"

export PROJECT_NUMBER="$(gcloud projects describe "$PROJECT_ID" --format='value(projectNumber)')"

gcloud services enable \
  run.googleapis.com artifactregistry.googleapis.com secretmanager.googleapis.com \
  iamcredentials.googleapis.com sts.googleapis.com iam.googleapis.com \
  cloudresourcemanager.googleapis.com
```

Billing must be linked even though everything here targets free tiers — Cloud Run and
Artifact Registry refuse to create resources on an unlinked project. Set a $5 budget alert
while you are in the console; it is the only guard against a surprise.

### Artifact Registry, with a cleanup policy that is load-bearing

```bash
gcloud artifacts repositories create "$AR_REPO" \
  --repository-format=docker --location="$REGION" \
  --description="open-exercise-db container images"
```

Expect this to fail once with `PERMISSION_DENIED` on
`artifactregistry.repositories.create` if you run it immediately after enabling
the API — the grant has not propagated yet, even for a project owner. Wait
20 seconds and re-run rather than going looking for a missing role.

The free tier is **0.5 GB**. This image measures 5.75 MB (verified with `docker inspect`), and
successive revisions share the distroless base layer, so the incremental cost of a deploy is
roughly the Go binary alone. That is far more headroom than the 0.5 GB figure suggests — but a
policy is still worth arming, because what actually accumulates is not tagged images. Every
buildx push leaves untagged manifests behind, and those grow without bound and without ever
appearing in a revision list.

```bash
cat > /tmp/oedb-cleanup.json <<'JSON'
[
  {"name":"delete-untagged","action":{"type":"Delete"},
   "condition":{"tagState":"UNTAGGED","olderThan":"3d"}},
  {"name":"keep-recent-tagged","action":{"type":"Keep"},
   "mostRecentVersions":{"keepCount":10}}
]
JSON

# --dry-run only reports. Read the report before arming it: a wrong keepCount
# deletes the image a revision is pinned to. The running revision survives on
# cached layers, but the ability to roll back to it does not.
gcloud artifacts repositories set-cleanup-policies "$AR_REPO" \
  --location="$REGION" --policy=/tmp/oedb-cleanup.json --dry-run

gcloud artifacts repositories set-cleanup-policies "$AR_REPO" \
  --location="$REGION" --policy=/tmp/oedb-cleanup.json --no-dry-run
```

`Keep` wins over `Delete` when both match, so the last 10 SHA-tagged images are protected
while `delete-untagged` reaps what buildx leaves behind. The `type=gha` buildx cache lives in
GitHub's cache, not here, so it does not count against the 0.5 GB.

On Windows, write that JSON from a shell that does not emit a byte-order mark.
PowerShell 5.1's `Out-File -Encoding utf8` adds one and gcloud rejects the file
with `Unexpected UTF-8 BOM`. The bash heredoc above is fine; from PowerShell use
`[System.IO.File]::WriteAllText($path, $json, (New-Object System.Text.UTF8Encoding($false)))`.

### Two service accounts, kept distinct

The deploy account is reachable by anything that can push to `main`. The runtime account holds
the keys to the database. They must not be the same principal.

```bash
gcloud iam service-accounts create gh-deploy --display-name="GitHub Actions deployer"
gcloud iam service-accounts create oedb-run  --display-name="open-exercise-db Cloud Run runtime"

export DEPLOY_SA="gh-deploy@${PROJECT_ID}.iam.gserviceaccount.com"
export RUNTIME_SA="oedb-run@${PROJECT_ID}.iam.gserviceaccount.com"
```

Deploy account — three roles, two of them scoped below project level:

```bash
# Push images: scoped to this one repository, not project-wide.
gcloud artifacts repositories add-iam-policy-binding "$AR_REPO" --location="$REGION" \
  --member="serviceAccount:${DEPLOY_SA}" --role="roles/artifactregistry.writer"

# Create revisions and shift traffic. NOT roles/run.admin: that carries
# run.services.setIamPolicy — the power to make the service private, or to grant
# invoker to anyone. CI never needs it.
gcloud projects add-iam-policy-binding "$PROJECT_ID" \
  --member="serviceAccount:${DEPLOY_SA}" --role="roles/run.developer"

# Deploying a service that RUNS AS oedb-run requires actAs on oedb-run. Bound on
# the runtime account resource, not the project — the single most commonly
# over-granted binding in a Cloud Run setup.
gcloud iam service-accounts add-iam-policy-binding "$RUNTIME_SA" \
  --member="serviceAccount:${DEPLOY_SA}" --role="roles/iam.serviceAccountUser"
```

Deliberately withheld from the deploy account: `roles/run.admin`,
`roles/secretmanager.secretAccessor` (CI passes secret *names*, never values, so it has no
reason to read one), and `roles/editor`.

Runtime account — as close to nothing as Cloud Run allows:

```bash
gcloud projects add-iam-policy-binding "$PROJECT_ID" \
  --member="serviceAccount:${RUNTIME_SA}" --role="roles/logging.logWriter"
```

Secret access is granted per-secret below, never project-wide. The runtime account gets **no**
storage roles: `internal/media/r2.go` uses `credentials.NewStaticCredentialsProvider` and
deliberately avoids `config.LoadDefaultConfig`, so no Google metadata credential chain is ever
consulted. R2 authenticates from the literal environment variables and nothing else.

### Workload Identity Federation

Keyless. No Google service-account key is ever stored in GitHub, which is the whole reason to
prefer this over a JSON key in a repository secret.

```bash
gcloud iam workload-identity-pools create github \
  --location=global --display-name="GitHub Actions"

gcloud iam workload-identity-pools providers create-oidc github \
  --location=global --workload-identity-pool=github \
  --display-name="GitHub OIDC" \
  --issuer-uri="https://token.actions.githubusercontent.com" \
  --attribute-mapping="google.subject=assertion.sub,attribute.actor=assertion.actor,attribute.repository=assertion.repository,attribute.repository_owner=assertion.repository_owner,attribute.ref=assertion.ref" \
  --attribute-condition="assertion.repository_owner == 'Cawlumm' && assertion.repository == '${GH_REPO}'"

gcloud iam service-accounts add-iam-policy-binding "$DEPLOY_SA" \
  --role="roles/iam.workloadIdentityUser" \
  --member="principalSet://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/github/attribute.repository/${GH_REPO}"

# The value for the GCP_WIF_PROVIDER GitHub variable:
echo "projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/github/providers/github"
```

The `--attribute-condition` is mandatory, not hygiene: gcloud refuses to create a
`token.actions.githubusercontent.com` provider that maps `assertion.repository` without one,
because otherwise *any* GitHub repository on the planet can mint a token for this pool.

The member string, exactly:

```
principalSet://iam.googleapis.com/projects/<PROJECT_NUMBER>/locations/global/workloadIdentityPools/<POOL_ID>/attribute.<NAME>/<VALUE>
```

Three things that are easy to get wrong: it is the project **number**, not the project ID; it
is `principalSet://` (plural) not `principal://`; and `locations/global` is literal, because
WIF pools are not regional. Slashes inside the value are fine —
`Cawlumm/open-exercise-db` is one value.

**Optional tightening to main-only.** Add `attribute.repo_ref=assertion.repository + '@' +
assertion.ref` to the attribute mapping and bind
`.../attribute.repo_ref/${GH_REPO}@refs/heads/main` instead. Do this only *after* a successful
deploy has proven the basic binding works, and verify what the `ref` claim actually contains
on a `workflow_run`-triggered job first — a `repo_ref` binding that does not match denies
silently, with an error that does not point at the mismatch.

### R2: `oedb-media-dev` serves production

One bucket, shared by local development and production. It already holds every object, so
there is nothing to migrate.

The cost of sharing is that development and production are no longer isolated: a local
importer run writes to the same objects production serves. In practice that is benign —
storage keys are deterministic (`exercises/<source-id>/<n>.jpg`) and so are the bytes, so a
re-import overwrites each object with an identical one. What it does mean is that **the
bucket's API token is a production credential**, and any token compromise or rotation mistake
takes production images down rather than a scratch environment. Treat it accordingly.

If dev and production ever need to diverge — a schema change to storage keys, an encoder
change, anything that would make the two write different bytes to the same key — split them
then. Buckets cannot be renamed, so splitting means creating a second bucket and pointing one
side at it. `cmd/migrate-media` exists for exactly that: it copies `MEDIA_DIR` into a bucket
with storage keys preserved, so a fresh bucket can be populated from a local `data/media`
without re-downloading from upstream.

Required: an R2 API token with Object Read & Write, **scoped to `oedb-media-dev` only**. Token
creation has no CLI path — it is a dashboard action.

### Secret Manager

```bash
for s in oedb-database-url oedb-r2-access-key-id oedb-r2-secret-access-key; do
  gcloud secrets create "$s" --replication-policy=automatic
  gcloud secrets add-iam-policy-binding "$s" \
    --member="serviceAccount:${RUNTIME_SA}" --role="roles/secretmanager.secretAccessor"
done
```

Adding versions — **use `printf`, never `echo`**:

```bash
printf '%s' 'postgres://postgres.<project-ref>:PASS@aws-0-us-east-1.pooler.supabase.com:5432/postgres?sslmode=require' \
  | gcloud secrets versions add oedb-database-url --data-file=-
printf '%s' '<r2 access key id>'     | gcloud secrets versions add oedb-r2-access-key-id --data-file=-
printf '%s' '<r2 secret access key>' | gcloud secrets versions add oedb-r2-secret-access-key --data-file=-
```

`echo` appends a newline, Secret Manager stores the bytes verbatim, and Cloud Run injects them
into the environment variable unchanged. A trailing newline in `R2_SECRET_ACCESS_KEY` produces
`403 SignatureDoesNotMatch` on every R2 call, from a key that looks perfectly correct in the
console. Verify:

```bash
gcloud secrets versions access latest --secret=oedb-r2-secret-access-key | xxd | tail -2
```

`R2_ACCOUNT_ID` and `R2_BUCKET` stay plain environment variables — they are identifiers, not
credentials, and both are already documented in `.env.example`.

### GitHub configuration

Repository (or `production` environment) **variables** — `vars.*`, not `secrets.*`, because
none of these are credentials and keeping them out of `secrets` stops the log redactor
mangling unrelated output:

| Variable | Value |
|---|---|
| `GCP_WIF_PROVIDER` | the `projects/<number>/locations/global/...` string echoed above |
| `GCP_DEPLOY_SA` | `gh-deploy@open-exercise-db.iam.gserviceaccount.com` |
| `R2_ACCOUNT_ID` | Cloudflare account id |
| `R2_BUCKET` | `oedb-media-dev` |

No GitHub **secrets** are needed at all. Workload Identity Federation means there is no
long-lived Google credential to store, which is the point.

Environment `production`: **no required reviewers** — deploying every merge without a human
step is the intent, and on a single-maintainer repository the squash-merge is the approval.
Do set **Deployment branches and tags → Selected branches → `main`**; that is the rule that
stops a `workflow_dispatch` on an arbitrary branch from reaching the environment. Add required
reviewers the day a second person gets write access, or the day the first destructive
migration lands.

---

## Bootstrapping a fresh environment

### Seed the database and the bucket

The importer is not in the container image — `backend/Dockerfile` builds only
`./cmd/api-server`, because the importer is a one-off bootstrap tool per
`docs/CONVENTIONS.md`'s `cmd/` layout. Run it from a workstation against the production DSN.
This is one of the concrete reasons production uses a managed public Postgres rather than
Cloud SQL: Supabase's pooler is a public TCP+TLS host, so this needs no proxy sidecar.

```bash
cd backend

# -limit 5 first: a real (not dry-run) write proves the R2 credentials before
# committing to a 40-minute run.
DATABASE_URL='postgres://postgres.<project-ref>:...@aws-0-us-east-1.pooler.supabase.com:5432/postgres?sslmode=require' \
R2_ACCOUNT_ID='...' R2_ACCESS_KEY_ID='...' R2_SECRET_ACCESS_KEY='...' R2_BUCKET='oedb-media-dev' \
go run ./cmd/import-freeexercisedb -limit 5

# then the full run, same environment
... go run ./cmd/import-freeexercisedb
```

Watch the startup line. It must name `r2 bucket "oedb-media-dev"`. If it says `filesystem`, one of
the four `R2_*` variables did not survive the shell and you are about to write 110 MB to the
local disk instead.

The importer calls `database.Migrate` first, so **this single command also creates the
schema**. There is no separate migration step, here or anywhere.

What it costs: ~30–45 minutes wall clock, single-threaded and unattended; ~400 MB downloaded
from upstream; ~110 MB uploaded to R2; ~1,746 Class A operations against a 1,000,000/month
allowance; $0. An already-populated *other* bucket saves nothing — on an empty database
`processImages` finds no row for a key and re-downloads unconditionally.

If it dies partway through, **run it again**. It is idempotent and self-healing: it checks
`Store.Exists` before reusing a row and re-uploads objects whose row survived but whose bytes
did not, reporting `images(uploaded=N reused=N repaired=N failed=N)`. A nonzero `repaired` on
a *later* run means storage lost data that Postgres did not, and the command says so
explicitly.

### The first deploy, by hand

Do the initial create manually. It is the only place `--allow-unauthenticated` is needed —
which requires `run.services.setIamPolicy`, a permission deliberately withheld from the deploy
account — and it means finding out whether Postgres and R2 are wired correctly in a terminal
rather than through a workflow log.

Run the push from a shell that has `gcloud` on its `PATH`. `configure-docker` installs a
credential *helper* rather than a stored password, so a shell that cannot find
`docker-credential-gcloud` fails the push with `Unauthenticated request ... does not have
permission "artifactregistry.repositories.uploadArtifacts"` — which reads like a missing IAM
role and is not one. On Windows that means PowerShell after the installer's `PATH` update, not
a Git Bash session started earlier.

```bash
gcloud auth configure-docker "${REGION}-docker.pkg.dev"

# --platform matters if you are building from an ARM machine.
docker buildx build --platform linux/amd64 \
  -t "${REGION}-docker.pkg.dev/${PROJECT_ID}/${AR_REPO}/api-server:bootstrap" \
  --push ./backend

gcloud run deploy "$SERVICE" \
  --project="$PROJECT_ID" --region="$REGION" \
  --image="${REGION}-docker.pkg.dev/${PROJECT_ID}/${AR_REPO}/api-server:bootstrap" \
  --service-account="$RUNTIME_SA" \
  --allow-unauthenticated --ingress=all \
  --port=8080 --cpu=1 --memory=512Mi --concurrency=80 \
  --min-instances=0 --max-instances=4 --timeout=120s \
  --execution-environment=gen1 --cpu-boost \
  --set-env-vars="ENV=production,R2_ACCOUNT_ID=<account id>,R2_BUCKET=oedb-media-dev" \
  --set-secrets="DATABASE_URL=oedb-database-url:latest,R2_ACCESS_KEY_ID=oedb-r2-access-key-id:latest,R2_SECRET_ACCESS_KEY=oedb-r2-secret-access-key:latest"
```

`--allow-unauthenticated` is a property of the service's IAM policy and persists across
revisions, so CI never re-sets it — which is why it does not appear in `deploy.yml`.

---

## Why the service is configured this way

**`--max-instances=4`** is the database connection ceiling expressed as a Cloud Run flag.
`database.Connect` clamps `MaxConns` into `[5,10]`, forcing 10 for anything outside that range
— so `pool_max_conns=3` in the DSN is *clamped back up to 10* and per-instance connections
cannot be lowered from configuration at all. Four instances is 40 connections, well inside
what Supabase's session pooler allows on the free plan, leaving room for a local importer run
(another 10) and a psql session. It also
caps runaway cost from a scraper. Four instances at 80 concurrency is 320 concurrent requests,
far beyond anything this API will see.

**`--concurrency=80`** (the default). Every handler is a short read from a 10-connection pool
and everything hot carries `Cache-Control`. Lowering concurrency would *increase* instance
count and therefore *increase* total database connections — the opposite of what the clamp
above wants.

**`--memory=512Mi`.** `docs/DESIGN.md`'s cold-start figure is quoted for a 512 MB container,
so this is the number the design's own research describes. The working set is the cached
export payload plus its gzip, one fully-buffered image body at a time, the rate limiter's
bucket map, and pgx's connections.

**`--min-instances=0`.** Cloud Run's free tier is ~180K vCPU-seconds/month, about 50 hours;
one always-on instance is 730 hours, roughly $10–15/month. Cold start is ~120 ms for this
image. Accept it.

**`--timeout=120s`** matches the server's own `WriteTimeout`, which was sized for
`GET /exercises/export` (~1.6 MB) on a slow connection. A Cloud Run timeout below the server's
write timeout would sever exactly that request from the outside while the process still
believed it was fine.

**`--port=8080`.** `PORT` cannot be passed via `--set-env-vars` — Cloud Run reserves it,
alongside `K_SERVICE`/`K_REVISION`/`K_CONFIGURATION`, and rejects the deploy. `--port` is how
it is set; Cloud Run then injects `$PORT`, which `config.Load` reads and `main.go` binds as
`":" + cfg.Port` across all interfaces.

**`--execution-environment=gen1`** — nothing here needs a writable filesystem or the full
Linux syscall surface, and gen1 has the faster cold start. **`--cpu-boost`** covers the
pre-listen phase (migration check, pool warmup, TLS handshake) and is billed only for that
window. **Not** set: `--no-cpu-throttling`, which would allocate and bill CPU continuously —
the opposite of what a scale-to-zero service wants. The only consequence is that background
goroutines such as the rate limiter's cleanup ticker are throttled between requests, which
merely delays bucket eviction.

**Startup probe: Cloud Run's default TCP probe, and no liveness probe.** `main.go` runs
`database.Connect`, `database.Migrate`, and `media.NewStore` *before* `ListenAndServe`, so
"the socket is listening" already means the database is reachable, migrations are applied, and
the media configuration is valid. A TCP probe gates on all of that for free; an
`httpGet /health` probe would add a `pool.Ping` per attempt on top of a connection that
already proved the point. For why there is no liveness probe, see rule 3 at the top.

---

## Failure modes

**Partial R2 configuration fails safe.** `media.NewStore` counts how many of the four `R2_*`
values are non-empty: zero means filesystem, four means R2, anything between is
`log.Fatalf("init media store: r2 is partially configured (N of 4 ...)")`. So a typo in one
`--set-secrets` key name is a crash-looping revision rather than a degraded one — and a
revision whose container exits before listening never passes the startup probe, never becomes
ready, and never receives traffic. `gcloud run deploy` exits non-zero, the workflow goes red,
and the *previous* revision keeps serving 100%. You cannot break production with a partial R2
configuration; you can only fail to deploy.

The failure the all-or-none rule exists to prevent is precisely the Cloud Run one: silently
falling back to `MEDIA_DIR` on a container's ephemeral layer and returning 404s hours later
when the instance recycles.

**Logs do not carry severity yet.** Everything is stdlib `log` to stderr, which Cloud Logging
records as `textPayload` at `DEFAULT` severity — so even `log.Fatalf` does not appear as an
ERROR, and a severity-based alert will miss the exact events worth alerting on. Until
structured logging lands, alert on the message text instead:
`textPayload =~ "^(connect database|migrate database|init media store)"`. Crash-loops are
independently visible in the revision list and in the failing deploy.

---

## Rollback

```bash
gcloud run revisions list --service="$SERVICE" --region="$REGION" \
  --format='table(metadata.name,status.conditions[0].status,spec.containers[0].image,metadata.creationTimestamp)'

# Instant — no rebuild, no image pull.
gcloud run services update-traffic "$SERVICE" --region="$REGION" \
  --to-revisions="oedb-api-a1b2c3d4e5f6-17=100"

gcloud run services describe "$SERVICE" --region="$REGION" \
  --format='value(status.traffic[].revisionName,status.traffic[].percent)'

# Forward again once fixed
gcloud run services update-traffic "$SERVICE" --region="$REGION" --to-latest
```

`deploy.yml` passes `--revision-suffix` built from the short commit SHA, so a revision name
identifies its commit without cross-referencing anything.

Three things a traffic rollback does not do:

1. **It does not roll back the database.** See rule 2 at the top — this is why the
   expand/contract discipline is not optional.
2. **The next deploy resets traffic to the newest revision.** If the bad code is still on
   `main`, the next merge redeploys it. Revert on `main` too, or the rollback is temporary.
3. **A revision that never became ready needs no rollback.** It never received traffic. Check
   `gcloud run revisions describe <rev> --region="$REGION"` and the logs instead.

`deploy.yml` also rolls back automatically: if the post-deploy smoke test fails, it shifts
traffic back to the revision that was serving before the deploy started.

---

## Verifying a deployment

`deploy.yml` runs an abbreviated version of this automatically. Run the full set after a
first deploy or any infrastructure change.

```bash
URL="$(gcloud run services describe "$SERVICE" --region="$REGION" --format='value(status.url)')"
UA='oedb-verify/1.0 (+https://github.com/Cawlumm/open-exercise-db)'
g() { curl -fsS -H "User-Agent: $UA" "$@"; }
```

A word on that `User-Agent`: a missing or generic one demotes the caller to the Unidentified
rate-limit tier (5 rpm, burst 2), so an unadorned `curl` loop will start returning 429 and
look like a broken deployment.

1. **Liveness and database** — `g "$URL/health"` returns `{"status":"ok"}`. A 503 with
   `"reason":"database"` most likely means the project has paused — check rule 3b and the
   keepalive job before assuming a code fault.
2. **The data is really there** — `g "$URL/api/v1/exercises?per_page=1" | jq .total` → **873**.
3. **Taxonomy** — `/api/v1/meta/{muscle-groups,equipment,categories,locales}` all non-empty.
4. **Images resolve** — take `.exercises[0].images[0].url` (relative until `IMAGE_BASE_URL` is
   set, so prefix `$URL`) and `curl -I` it. Expect 200, `content-type: image/jpeg`, and an
   immutable `cache-control`. A **404 means the rows were seeded against a different bucket**;
   a **500 means bad R2 credentials**.
5. **Variants** — `.exercises[0].images[0].variants` is non-empty (migration 0002 backfilled
   one for every image). Fetch a non-`full` variant URL too.
6. **The heavy path** — `time g "$URL/api/v1/exercises/export" -o export.json` → ~1.6 MB. This
   is the one request a mis-set `--timeout` would sever.
7. **Compression and conditional GET** — with `Accept-Encoding: gzip`, expect
   `content-encoding: gzip` and an `etag`; re-request with `If-None-Match:` and expect 304.
   Confirms Cloud Run is neither stripping nor double-encoding.
8. **Rate limiting survives the proxy** — 30 rapid requests should give 200s then 429s, with
   `RateLimit-Remaining` decrementing across them. If it never decrements, `clientKey` is
   bucketing every request separately (check `TRUSTED_PROXY_MODE` resolved to `cloudrun`).
9. **Cold start** — wait 20 minutes, then `time g "$URL/health"`. This is the worst case:
   Cloud Run cold start only — the database is always-on, so it no longer contributes a wake.
   Expect well under a second of database time.
10. **Logs** — `gcloud run services logs read "$SERVICE" --region="$REGION" --limit=50`. Expect
    `media store: r2 bucket "oedb-media-dev"` and the `IMAGE_BASE_URL is unset` warning, which is
    correct and expected until a CDN domain exists.
11. **A week later** — Artifact Registry storage, Cloud Run usage, and Supabase's 500 MB
    database and 5 GB egress limits. Also confirm `oedb-supabase-keepalive` has actually run:
    a keepalive that quietly stopped looks exactly like one that is working, right up until
    the project pauses.

## Static catalog snapshot

`.github/workflows/snapshot.yml` publishes the catalog as static files on the `catalog` branch
(the GitHub Pages source) so bulk readers never touch the API or the database. It runs every 6
hours (`17 */6 * * *`) on `main` only, and can be run by hand: Actions, Snapshot, Run workflow.

It reads the public API with `cmd/build-snapshot` (`-api`, `-out`, `-confirm-removals`), so it
needs no secrets beyond the built-in `GITHUB_TOKEN`. The first run creates the `catalog` branch
as an orphan; after that, enable Pages from the `catalog` branch root. Files are
content-addressed and old ones are kept, so `git log catalog` is the archive of every published
version. A run that finds nothing changed commits nothing.

To try it locally: `cd backend && go run ./cmd/build-snapshot -out /some/dir`; running it twice
should print `unchanged` the second time.

**It refuses to publish** (the job fails and the live snapshot is untouched) on: any HTTP error
after retries, a collected count different from the API's `total`, zero exercises, duplicate
ids, a non-`en` locale whose ids differ from `en`, a locale in the published manifest that is missing
from the new run, an unreadable existing manifest, or an `en` count under half of what the
`catalog` branch's manifest records. A failure is retried once over the whole collection first.

**Confirming removals.** If the catalog really did shrink by more than half, or a locale was
retired on purpose, run the workflow
manually with `confirm_removals` ticked. Do this only after checking the drop is intended.

**Keeping the schedule alive.** GitHub disables scheduled workflows in a public repository after
60 days without repository activity. The manifest's `published_at` heartbeat commits to
`catalog` at least daily, which is expected to count as activity; if it does not, the schedule
stops quietly. Consumers notice: Lyftr falls back to the API once `published_at` is older than
48 hours. To check, look at Actions, Snapshot: a disabled workflow shows a banner, and the
newest commit on `catalog` should be less than a day old. To re-enable, press "Enable
workflow" there (or `gh workflow enable snapshot.yml`) and run it once by hand.

**Protect the branch.** Add a ruleset on `catalog` that blocks force-push and deletion. Its git
history is the archive, and the job only ever fast-forwards.

Image bytes still come from the API (`/api/v1/images/{id}`) until `IMAGE_BASE_URL` points at a
custom domain on the R2 bucket; see [Known gaps](#known-gaps).

---

## Approving an app

Apps are approved by hand ([API_POLICY.md](API_POLICY.md#how-to-get-a-credential) tells
developers how to ask). To approve a request:

1. Check it against the criteria in API_POLICY: a real app, a reachable contact, sensible
   volume, and a CC0 notice to its users if it will submit data.
2. Sign in as an admin: open `$API/api/v1/auth/github` in a browser and keep the `token` from
   the callback response.
3. Register the app. The response contains the app's credential **once**; it is not stored
   and cannot be shown again.

   ```bash
   curl -sS -X POST "$API/api/v1/admin/apps" \
     -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
     -A "oedb-admin/1 (maintainer)" \
     -d '{"name": "SomeApp", "contact_email": "dev@someapp.example"}'
   ```

   `max_pending` and `max_per_day` are optional and cap how many submissions the app can
   have waiting and send per day.
4. Email the `token` to the contact address. Never paste it into the issue. Reply on the issue
   that the credential was sent, and close it.

If a credential leaks, `POST /api/v1/admin/apps/{id}/rotate` issues a new one and voids the
old. `POST /api/v1/admin/apps/{id}/status` suspends or restores an app.

## Known gaps

**Image bytes proxy through the API.** `IMAGE_BASE_URL` is unset, so `media.ImageURL` returns
`/api/v1/images/{id}` and every image is served by the Cloud Run process instead of directly
from object storage. Closing this needs no code change at all — the switch already exists —
only a custom domain attached to the R2 bucket and the variable added to `--set-env-vars`. Do
not shortcut it with the `*.r2.dev` subdomain: Cloudflare rate-limits it and documents it as
development-only, as `.env.example` already notes.

**Client IP depends on `TRUSTED_PROXY_MODE`.** `clientKey` (`internal/api/clientip.go`)
buckets the rate limiter on the client address, and which header may be believed depends on
what is in front of the process:

| `TRUSTED_PROXY_MODE` | Believed | Default when |
|---|---|---|
| `cloudrun` | the **right-most** `X-Forwarded-For` entry, which Google's front end appends | `K_SERVICE` is set (always, on Cloud Run) |
| `cloudflare` | `CF-Connecting-IP` only | never; set it only when Cloudflare is the sole way to reach the origin |
| `direct` | nothing; the TCP peer address | otherwise |

Everything left of the right-most `X-Forwarded-For` entry is client-supplied, and
`CF-Connecting-IP` is ignored outside `cloudflare` mode, so neither buys a fresh bucket. A value
that is not a literal IP falls back to the TCP peer. An unrecognised mode refuses to start.
Production runs on Cloud Run with no CDN in front, so it uses the `cloudrun` default; switch to
`cloudflare` only together with the edge proxy described below. Buckets remain per-instance and
`--max-instances=4` still bounds the blast radius; the origin limiter is a backstop.

An unknown app `key_id` is looked up once and then remembered for 30 seconds, on top of the
per-address flood guard that runs before authentication, so forged credentials do not each
cost a database query.

**No Cloudflare in front.** `docs/DESIGN.md` calls for an edge cache ahead of the Cloud Run
origin, which is also where the real rate limiting in `docs/API_POLICY.md` is meant to live.
Both are blocked on owning a domain. A Cloudflare Worker proxying `api.<domain>` to the
`run.app` URL is the free route; a global external load balancer is roughly $18/month for the
forwarding rule alone and would defeat the free-tier premise.
