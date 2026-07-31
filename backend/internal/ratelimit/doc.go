// Package ratelimit implements a coarse, per-process token-bucket rate
// limiter keyed by client identity (an IP-derived string; see
// internal/api/ratelimit.go's clientKey for how that's extracted from an
// actual HTTP request — this package itself never touches net/http) and a
// cost Tier (docs/PHASE1_BUILD_PLAN.md WO-7's "tiered limits by cost, not
// one global number").
//
// # Honest scope — read this before trusting a number out of here
//
// This is a per-INSTANCE limiter, not a global one, and that is a real
// limitation, not an implementation detail to gloss over.
// docs/DESIGN.md → Hosting targets Google Cloud Run, which autoscales
// horizontally and scales to zero. An in-process limiter — this one
// included — enforces its configured limit independently on every live
// instance: with N instances live, the real ceiling a determined client can
// reach is N x the configured value, and every instance's counters reset to
// completely fresh whenever that instance cold-starts (a new process means
// a new, empty map). A limiter that quietly behaves this way while claiming
// to enforce "10 req/min" is worse than no limiter at all, because it
// creates false confidence — docs/PHASE1_BUILD_PLAN.md's WO-7 spec calls
// this out explicitly, and it's why this package's own doc comment leads
// with it instead of burying it near the bottom.
//
// Given that, this package is deliberately positioned as defense-in-depth,
// not the primary control:
//
//   - The primary, globally-accurate enforcement point is Cloudflare, which
//     sits in front of the origin in every real deployment
//     (docs/DESIGN.md → Hosting) and can rate-limit by real client IP
//     across every instance at once, for free, before a request ever
//     reaches this process. docs/API_POLICY.md documents the exact edge
//     rules to configure there — that's the number a consumer should treat
//     as the real published policy.
//   - This package's job is to make sure a request that reaches the origin
//     directly — Cloudflare bypassed, misconfigured, or simply not yet
//     applied to a fresh environment — still meets *some* resistance rather
//     than none. "Some resistance, honestly described" is the design goal;
//     "the same global number Cloudflare enforces" is not a promise this
//     package can keep on Cloud Run, and it does not attempt to.
//
// # Why not a shared datastore
//
// A limiter backed by Redis (or any other shared store) would close the
// per-instance gap above and make the limit genuinely global. Both
// docs/DESIGN.md and docs/PHASE1_BUILD_PLAN.md WO-7 rule that out
// explicitly at this project's traffic scale: it would trade a near-zero-
// cost problem (an approximate, honestly-documented origin backstop) for a
// real one (a new stateful dependency, plus a network round trip on every
// single read — exactly the hot-path cost the CDN caching strategy in
// docs/DESIGN.md → Performance exists to avoid). A Postgres-backed limiter
// that queries or writes per request was considered and rejected for the
// identical reason — it just swaps which "cheap problem" gets traded for an
// "expensive" one.
//
// # What's actually in this file set
//
//   - Tier / Rule / Config: the cost-tier vocabulary and its tunable rate
//     (requests/minute) and burst (immediate allowance) per tier, loaded
//     from internal/config.RateLimitConfig by cmd/api-server/main.go so
//     every value is independently overridable per environment
//     (docs/PHASE1_BUILD_PLAN.md WO-7: "a test/staging instance must not be
//     trivially self-DoSed").
//   - Limiter: owns one golang.org/x/time/rate.Limiter per (key, tier) pair,
//     built lazily and evicted after IdleTTL of inactivity so the map
//     doesn't grow without bound over a long-running process's lifetime
//     against an ever-changing set of client IPs.
//
// golang.org/x/time/rate was chosen over hand-rolling a token bucket: it's
// pure Go (no cgo, preserving the CGO_ENABLED=0 distroless build
// docs/DESIGN.md's Cloud Run cold-start numbers depend on — see
// internal/media/doc.go for the same constraint applied to image encoding),
// small, already one dependency-hop away via golang.org/x/image's own
// module graph, and it's the standard library's own answer to this exact
// problem (originally written for golang.org/x/net/context rate limiting).
// Hand-rolling a token bucket buys nothing here and risks a subtly wrong
// refill calculation that a maintained, widely-used package has already had
// tested for years.
package ratelimit
