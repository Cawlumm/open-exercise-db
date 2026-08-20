# API usage policy

This is the public policy for `open-exercise-db`'s API: rate limits, the `User-Agent`
requirement, how to consume the API without tripping either, and what happens if you do
anyway. It exists because [`docs/PHASE1_BUILD_PLAN.md`](PHASE1_BUILD_PLAN.md) WO-7 gates
going genuinely public on this policy existing and being enforced — see that document for
the full engineering rationale; this page is the consumer-facing summary.

This is a community project, not an adversarial one. Nothing below is designed to catch
people out — it exists so one misbehaving client (usually by accident: a missing debounce, a
retry loop with no backoff) can't degrade the API for everyone else on a single-maintainer,
free-tier budget. If a limit is genuinely blocking a legitimate use case, see
[Requesting more](#requesting-more) below — the answer is much more likely to be "sure" than
"no."

## Rate limits

Limits are tiered by cost, not one global number — a single cached row costs almost nothing
to serve; the full-catalog export costs the most.

| Tier | Applies to | Limit | Why |
|---|---|---|---|
| **App** | Any read, once a valid app credential is presented | **600 req/min/app** (burst 600) | Billed to the app, not to its address — see [Register an app for a bigger budget](#register-an-app-for-a-bigger-budget) |
| Read | `GET /exercises/{id}`, `GET /meta/*`, `GET /images/{id}` | 120 req/min/IP (burst 120) | Cheap, cached, single-row/short-list reads — an id-keyed image fetch is one more instance of the same shape, and its response carries a long `immutable` `Cache-Control` on top |
| Search | `GET /exercises` | 120 req/min/IP (burst 120) | Matched to Read rather than set below it — live lookup is the primary integration model, so search is *its* main endpoint. Still assumes a *debounced* client, not a per-keystroke one — see [Debounce your search](#debounce-your-search-not-optional) |
| Export | `GET /exercises/export`, `POST /images` | 5 req/min/IP | The two heaviest requests in the API. The export is the largest response; an upload is the largest *cost* — it buffers megabytes, decodes them, re-encodes them and writes to object storage, which no cache can absorb. See [Mirror the dump, don't poll it](#mirror-the-dump-dont-poll-it). Uploading a batch is meant to be paced: back off on `429` rather than retrying immediately, and note that the per-app pending-upload quota, not this limit, is what actually bounds how much you can have in review at once |
| Unidentified | Any request that identifies itself with neither a conformant `User-Agent` nor `X-OEDB-Client` | 5 req/min/IP, replacing whatever tier the endpoint would otherwise use | See [User-Agent requirement](#user-agent-requirement) |

These numbers are enforced in two places, and it matters which one you're actually being
limited by:

- **Cloudflare, in front of the origin** — this is the real, globally-accurate enforcement.
  It sees every request across every backend instance and applies the limit consistently no
  matter how the origin has scaled. **Treat the table above as Cloudflare's policy** — that's
  the number a well-behaved client should plan around.
- **The origin API itself** — a coarse, honest backstop, not a second copy of the same
  guarantee. The origin runs on Google Cloud Run, which autoscales horizontally and scales to
  zero. An in-process limiter (this project uses `golang.org/x/time/rate`, one token bucket
  per client per tier) can only ever enforce its configured number *per running instance* —
  with two instances live, a client that reaches the origin directly (bypassing Cloudflare) can
  in principle see roughly double the configured number, and every instance's counters reset
  whenever that instance cold-starts. This is documented here, not hidden: the origin limiter's
  job is to make sure a request that reaches the backend directly still meets *some*
  resistance, not to promise the same global accuracy Cloudflare provides. See
  `backend/internal/ratelimit`'s package doc comment for the full engineering reasoning,
  including why a shared datastore (Redis, or a per-request Postgres check) was deliberately
  not used to close this gap — at this project's scale, that would trade a near-zero-cost
  problem for a real one.

If you're building against this API, the practical takeaway is: respect the table above and
you'll never notice which layer stopped you.

## User-Agent requirement

Every request must send a `User-Agent` identifying your application, in the format Open Food
Facts also uses:

```
User-Agent: AppName/Version (ContactEmail)
```

Example: `User-Agent: gym-buddy-app/2.1 (maintainer@example.com)`

What counts as "conformant": an app name, a `/`, a version, and a parenthesized way to reach
you — an email address, a project URL, whatever actually gets a message to a human. What
doesn't count: no header at all, or a default header from an HTTP library or browser
(`curl/8.x`, `python-requests/...`, `Go-http-client/1.1`, `PostmanRuntime/...`, a bare
`Mozilla/5.0 (...)` browser string, etc.) — those identify the tool making the request, not
who's actually operating it.

### Calling from a browser: use `X-OEDB-Client`

Script running in a page **cannot** set `User-Agent` — the Fetch spec lists it as a forbidden
header name, so the browser sends its own `Mozilla/5.0 (...)` string no matter what your code
asks for. That string is a generic one, which means a web app following the rule above would
still land in the Unidentified tier with nothing it could do about it. The requirement said
"identify yourself" and the platform said "you may not".

So browser clients send the same identity in a header they are allowed to set:

```
X-OEDB-Client: AppName/Version (ContactEmail)
```

Same format, same rules, same effect on your budget. Send it *instead of* worrying about
`User-Agent`; you will be sending both, because the browser adds its own, and
`X-OEDB-Client` wins when both are present. No CORS configuration is needed on your side — the
preflight response echoes whatever request headers you ask for.

This is not a way around the requirement. A malformed `X-OEDB-Client` (`Mozilla/5.0`, an app
name with no contact, an empty value) fails exactly as the same string would fail as a
`User-Agent`, and you stay on the reduced budget.

Non-browser clients should keep using `User-Agent`. It is the convention every other HTTP tool
already understands, and there is no reason to send a second header when the first one works.

**A request without a conformant `User-Agent` is not rejected outright.** It's routed to a
single, sharply reduced budget (5 req/min/IP) instead of the endpoint's normal tier. This is a
deliberate middle ground:

- Rejecting outright (a hard 4xx on every unidentified request) would strand a consumer with
  no way to discover the rule — this is a zero-auth API with no account or dashboard to surface
  a warning through, so a first "how do I fix this" moment would just be a wall of errors.
- Accepting a generic `User-Agent` at full tier limits would make the requirement purely
  decorative.

The reduced budget threads that needle: enough to explore the API and discover the rule via a
`429` (see [Rate-limit response format](#rate-limit-response-format) below, which tells you
exactly what to fix), not enough to sustain real traffic anonymously.

**If you're calling this API directly from browser JavaScript** (`fetch`/`XHR`), you can't set
this header yourself — the Fetch spec forbids scripts from overriding `User-Agent`. Proxy your
requests through your own backend and set the header there, the same guidance Open Food Facts
gives its own browser-based consumers.

## Fetching a known set, and filtering on several values

**Re-read specific rows** with `?ids=` (UUIDs) or `?source_ids=` (upstream
identifiers), comma-separated or repeated, up to 100 per request:

```
GET /exercises?ids=3490fcdf-...,7c1b2e44-...
GET /exercises?source_ids=3_4_Sit-Up,Barbell_Bench_Press
```

This is what a consumer keeping its own copy should use to refresh it. Before it
existed the only route was pulling the whole export and intersecting locally — a
300 KB response to refresh a handful of rows. `source_ids` answers the migration
case: if you are moving off free-exercise-db you already hold its identifiers, and
every response has always carried `source_id`.

Prefer this to polling the export. The export is for mirroring the *whole*
catalog on a schedule; `?ids=` is for refreshing what you actually hold.

**Every taxonomy filter takes several values**, comma-separated or repeated.
Values within one filter are OR'd, and the filters are AND'd together:

```
GET /exercises?muscle_group=chest,lats&equipment=barbell
```

reads as "chest or lats, with a barbell" — the query a faceted picker actually
means.

Both spellings work and may be mixed — `?muscle_group=chest,lats&muscle_group=quadriceps`
is three values.

> **Behaviour change.** These filters used to accept a single value, and a
> repeated parameter was silently reduced to its first:
> `?muscle_group=chest&muscle_group=lats` answered `200` with only the chest rows.
> If you worked around that by issuing one request per value, you can now send one.

Every value is validated, so a typo is reported wherever it appears rather than
passing because the first value happened to be spelled right.

## What search matches

`?q=` is one search box, not a name lookup. Each whitespace-separated word is matched
against an exercise's **name, aliases, muscles (primary and secondary), equipment,
category, mechanic, force and level** — so `?q=kettlebells` returns every exercise that
uses them, not only the ones with the word in their name.

Every word must match somewhere, in any order, so adding a word narrows the result:
`?q=barbell bench` finds barbell bench presses because one word matched the equipment and
the other the name.

Two things worth knowing:

- **Name matches rank first.** A row that matched only through its taxonomy sorts below
  every row whose name matched, so `?q=press` still puts "Bench Press" above everything
  that merely shares a category.
- **This composes with the filters, it does not replace them.** `?q=press&equipment=kettlebells`
  narrows within the filter. When you know the exact taxonomy slug, the filter is cheaper
  and exact — use it. `?q=` is for the case where a person typed something and you do not
  know which dimension they meant.

Matching is substring-based against the taxonomy slugs, which is deliberate: nobody types
`olympic-weightlifting`, so `?q=olympic` finds it. There is no stemming or fuzzy matching —
`?q=squats` will not find "Squat". Ranking is exact name, then name prefix, then name
substring, then taxonomy-only.

## Register an app for a bigger budget

Send a registered app credential and your reads are billed to **your app at 600 req/min**
instead of to your IP address at 120:

```
Authorization: Bearer oedb_<key_id>.<secret>
```

Two things change when you do:

- **The budget is an order of magnitude larger.** An app is accountable in a way an address
  is not — it has a contact address on file and a credential that can be suspended — so it
  is trusted further.
- **You stop sharing a bucket with strangers.** Anonymous limits are per-IP, so everyone
  behind one office NAT or mobile carrier address shares a budget (see
  [Known limitation: shared IPs](#known-limitation-shared-ips)). An app's budget is its own
  wherever its requests come from, which makes that limitation disappear for you.

The credential is verified against the database before any of this applies. Presenting a
well-formed but invalid credential earns nothing — it is billed exactly as an anonymous
request, so a wrong or rotated key shows up as the anonymous limit rather than as a 401 on a
public read.

**The export is deliberately excluded.** `GET /exercises/export` and `POST /images` stay on
their own tier for an app exactly as for anyone else: they are the heaviest requests in the
API, and registering does not make polling them reasonable. Mirror the dump on a schedule.

Anonymous access is not going away and needs no key. This exists so the consumers who *are*
accountable are not held to a limit sized for anonymous traffic.

## Debounce your search (not optional)

`GET /exercises` is the endpoint a live type-ahead search hits, and it's easy to blow through
60 req/min without meaning to: a single user typing one 10-character query at normal speed,
firing a request per keystroke, is 10 requests in a couple of seconds on its own — and a
handful of users behind one IP (see [Known limitation: shared IPs](#known-limitation-shared-ips))
multiplies that.

**Debounce 300-500ms before firing a search request**, or fetch a candidate set once per
session/page-load and filter it client-side instead of re-querying on every keystroke. Either
approach comfortably fits inside the Search tier's burst allowance (120 requests available
immediately, refilling at 2/sec) for real usage patterns; a per-keystroke integration will
not.

## Mirror the dump, don't poll it

If you need the whole catalog — offline support, a local cache, a self-hosted mirror — use
`GET /exercises/export` **on a schedule** (once every few hours is almost always enough; this
dataset changes rarely), not in a loop, and not as a substitute for `GET /exercises` lookups.
It's cached in-process and served with a long `Cache-Control`/`ETag`
(`stale-while-revalidate`), so a conditional request (`If-None-Match`) against an unchanged
catalog costs almost nothing on either side — use that instead of re-downloading the full body
every time.

If your access pattern is "poll `/export` frequently to notice changes quickly," talk to us
first (see [Requesting more](#requesting-more)) — there's likely a better fit than raising this
tier's limit.

## Rate-limit response format

A request that exceeds its bucket gets `429 Too Many Requests` with:

```
RateLimit-Limit: 60
RateLimit-Remaining: 0
RateLimit-Reset: 12
Retry-After: 12
Content-Type: application/problem+json

{
  "title": "Too Many Requests",
  "status": 429,
  "detail": "rate limit exceeded for the search tier (60 req/min); see docs/API_POLICY.md for limits and how to request more"
}
```

`RateLimit-Limit`/`RateLimit-Remaining`/`RateLimit-Reset` are also sent on successful
responses from a rate-limited endpoint, so a well-behaved client can back off *before* hitting
`429` by watching `RateLimit-Remaining` approach zero.

### Standards note

These header names follow the **IETF `draft-ietf-httpapi-ratelimit-headers` Internet-Draft**
(`RateLimit-Limit`/`RateLimit-Remaining`/`RateLimit-Reset`, the form used by early drafts of
that document and still the most widely deployed convention in practice, including by Open
Food Facts). As of this writing that draft has not been published as an RFC, and its most
recent revisions have moved toward consolidating the three headers into two (`RateLimit` +
`RateLimit-Policy`) — this API uses the three-header form because it's what's actually
deployed and documented elsewhere, and will revisit if/when the draft's newer shape becomes the
de facto standard.

An earlier version of this project's build plan cited **RFC 9331** as the source for this
header naming. That citation was wrong and has been corrected: RFC 9331 is *"The Explicit
Congestion Notification (ECN) Protocol for Low Latency, Low Loss, and Scalable Throughput
(L4S)"* — a transport-layer congestion-control spec with no connection to HTTP rate-limit
headers. `Retry-After` is a real, unrelated standard header, defined in
[RFC 9110 §10.2.3](https://www.rfc-editor.org/rfc/rfc9110.html#section-10.2.3).

## Requesting more

If a documented limit is genuinely blocking a legitimate use case — a higher-volume mirror
sync, a search-heavy integration that's already debounced and still tight — open an issue on
[the repo](https://github.com/Cawlumm/open-exercise-db/issues) describing what you're building
and what you need. This is a single-maintainer background project, so response time is
best-effort, but raising it is very likely to get a "sure, here's a bump" rather than a "no" —
there's no monetization angle here that a higher limit threatens.

There is no API-key or paid-tier system in Phase 1 (see
[`docs/PHASE1_BUILD_PLAN.md`](PHASE1_BUILD_PLAN.md) WO-7's explicitly-out-of-scope list —
per-user auth quotas are Phase 2 territory, once accounts exist at all). If you're behind a
shared IP (corporate NAT, CGNAT, a university or office network) and that's causing collisions
with the per-IP limit, say so in the issue — it's flagged as a known limitation below, not a
solved problem.

## Known limitation: shared IPs

Limits here are enforced per client IP. Open Food Facts' own policy scopes limits per
*end user* rather than per app specifically to handle this; this project doesn't have an
equivalent mechanism yet (that would mean some form of per-consumer identity, which is
Phase 2's territory — see [`docs/DESIGN.md`](DESIGN.md)'s "Explicitly skipped" list and
`docs/PHASE1_BUILD_PLAN.md` WO-7's "explicitly not in scope"). Multiple self-hosted instances
or users behind the same corporate/CGNAT IP currently share one bucket. If this is a real
problem for you, [ask](#requesting-more) — it's the single most likely thing to prompt Phase 2
scoping earlier than planned.

## Abuse runbook

For the maintainer (or anyone helping operate this API) to identify, block, and unblock a
consumer.

### Identify

1. Cloudflare's dashboard (Analytics → Security, or Firewall Events if a rule already fired) is
   the first stop — it sees traffic across every origin instance and shows IP, request rate,
   and matched rule in one place.
2. If the origin itself is logging `429`s (`rate limit: operation ... has no ... metadata tag`
   is a *different*, code-bug log line — don't confuse the two), `cmd/api-server`'s stdout logs
   each `429` implicitly via the standard access-log pattern; grep Cloud Run's log stream for
   `429` responses and the offending IP/`User-Agent` pair.
3. A consumer that's merely unidentified (no conformant `User-Agent`) but otherwise low-volume
   isn't "abuse" — it's the documented [Unidentified](#user-agent-requirement) tier working as
   intended. Real abuse looks like: sustained traffic well above any tier's limit from one IP,
   ignoring repeated `429`s with no backoff, or traffic clearly scraping `/exercises/export` in
   a loop instead of on a schedule.

### Block

1. **Preferred: a Cloudflare block or a tighter rate-limit rule scoped to the offending IP or
   IP range**, in the Cloudflare dashboard (or via the API/Terraform if this project ever
   automates its Cloudflare config) — see [Cloudflare configuration](#cloudflare-configuration)
   below for the base rules this sits on top of. This stops the traffic before it reaches
   Cloud Run at all, which matters for both cost and the origin's own limited backstop.
2. If Cloudflare access isn't available in the moment, the origin's per-instance backstop
   (`internal/ratelimit`) still applies without any manual action — it's just coarser, per the
   honest-scope note in [Rate limits](#rate-limits) above.
3. Document the block: IP/range, date, reason, and any correspondence, so it can be reviewed
   or reverted later without having to reconstruct the reasoning from scratch.

### Unblock

This is a community project — a block should always be appealable, the same framing Open Food
Facts uses for its own IP bans:

1. The blocked party opens an issue (or, if their access is blocked because they can't reach
   GitHub either for some reason, any other channel the maintainer publishes) explaining what
   they were doing and, ideally, committing to the [User-Agent](#user-agent-requirement) and
   [debounce](#debounce-your-search-not-optional) guidance above.
2. The maintainer removes the Cloudflare rule/block for that IP once satisfied it was a
   misconfiguration or misunderstanding rather than intentional abuse.
3. No penalty period, no "three strikes" — the goal is a working integration, not punishment.

## Cloudflare configuration

Reproducible reference for the edge rules this policy assumes are in place — write these down
here rather than leaving them as tribal knowledge in a dashboard only one person has looked at.
Configure via the Cloudflare dashboard (Security → WAF → Rate limiting rules) or equivalent
Terraform/API automation if this project adds infrastructure-as-code later.

Create one rate-limiting rule per tier, all scoped to this zone/hostname:

| Rule name | Match | Rate | Action |
|---|---|---|---|
| `oedb-read` | `http.request.uri.path matches "^/api/v1/(exercises/[^/]+|meta/.*|images/[^/]+)$"` and method `GET` | 60 requests / 60s, per IP | Block for 60s |
| `oedb-search` | `http.request.uri.path eq "/api/v1/exercises"` and method `GET` | 30 requests / 60s, per IP | Block for 60s |
| `oedb-export` | `http.request.uri.path eq "/api/v1/exercises/export"` and method `GET` | 5 requests / 60s, per IP | Block for 300s |
| `oedb-upload` | `http.request.uri.path eq "/api/v1/images"` and method `POST` | 5 requests / 60s, per IP | Block for 300s |
| `oedb-global-fallback` | `http.request.uri.path matches "^/api/v1/.*"` | 300 requests / 60s, per IP | Block for 60s (catches anything the per-tier rules above don't match, e.g. a future endpoint added without an accompanying rule) |

Notes:

- These numbers **match** the origin's configured defaults (see
  [Rate limits](#rate-limits)) deliberately — Cloudflare is the real enforcement point, so its
  numbers are what this whole document publishes; the origin's matching defaults exist so the
  origin backstop doesn't trip *before* Cloudflare's real rule does under normal traffic
  (it would if the origin were configured stricter than the edge), while still doing something
  useful if a request reaches the origin directly. Cloudflare's own free-tier rate-limiting
  rules are documented as counting requests globally across Cloudflare's network, unlike the
  origin's necessarily per-instance count — this is the concrete difference that makes
  Cloudflare's number the accurate one.
- Set the WAF rate-limiting "Response" to a `429` with the same `Retry-After` semantics this
  API's own `429` uses, so a client sees a consistent contract whichever layer stopped it.
  Cloudflare's own dashboard exposes this as a per-rule "Response" configuration; it doesn't
  need to byte-for-byte match this API's JSON body, but should return `429` and a
  `Retry-After` header at minimum.
- A general Cloudflare "I'm Under Attack" / bot-fight mode is not enabled by default for this
  project — it would add friction (CAPTCHA-style challenges) inappropriate for a
  machine-consumed API. Enable it manually, temporarily, only during an active volumetric
  attack that the rate-limiting rules above aren't containing.

## Where this is enforced in code

- `backend/internal/ratelimit` — the token-bucket limiter itself (package doc has the full
  per-instance-vs-global honesty discussion).
- `backend/internal/api/ratelimit.go` — the HTTP-layer middleware: client-IP extraction
  (honoring Cloudflare's `CF-Connecting-IP`), the `User-Agent` policy, and the `429` response.
- `backend/internal/config` (`RateLimitConfig`) — every tier's rate and burst is configurable
  per environment via env vars (`OEDB_RATELIMIT_*`), so a staging/CI instance can run with much
  lower values than production without a code change.
