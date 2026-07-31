// Package export builds the full-catalog blob served by GET
// /exercises/export (docs/PHASE1_BUILD_PLAN.md WO-5).
//
// The output shape is this project's own canonical exercise representation —
// the same field names GET /exercises already returns — not shaped to any
// single downstream consumer's schema. A consumer moving between the bulk
// dump and live lookups should not have to rewrite its mapping. (An earlier
// draft of this package's doc comment said the shape matched lyftr's
// models.Exercise field-for-field, including a flattened numbered
// "description" string in place of instructions. That was wrong per
// docs/DESIGN.md's "Consuming this API" section and was never implemented;
// this comment is the correction.)
//
// Body.Exercises is []contract.Exercise (internal/contract) — the literal
// same Go type internal/api renders for GET /exercises and
// GET /exercises/{id}, not a copy. An earlier version of this package
// declared its own Exercise/Translation types, structurally identical but
// independently maintained; the moment internal/api.Register wired both
// packages' routes onto one huma.API, huma's schema registry panicked on
// two distinct Go types both named "Exercise" (registry keys by type name,
// not import path). See internal/contract/doc.go for the full writeup and
// why a shared type — not just a passing test — is what actually makes the
// two endpoints' shapes impossible to drift apart.
//
// Cache, not just Builder: this package owns an in-memory Blob (JSON body +
// precomputed ETag + build time), built from internal/store on first access
// and reused until it's stale, per WO-5's "built once and cached, not
// computed per request." internal/api/export.go only ever reads Cache.Get's
// result; it never queries the store directly for this endpoint.
//
// Phase 1 vs. Phase 2 — the destination boundary: docs/DESIGN.md specifies
// the finished design as a blob regenerated on every approved submission/
// revert and written to R2, served by the CDN, so a full-catalog sync never
// touches Postgres or this process. Phase 2 doesn't exist yet (no
// submissions, no moderation, no R2 client) — see docs/PHASE1_BUILD_PLAN.md's
// guardrails. What's here is a deliberately smaller Phase 1 stand-in: an
// in-memory cache with a bounded TTL (see Cache's doc comment for why TTL,
// not a rebuild signal, is the right Phase 1 choice) served directly by this
// process. Cache.Invalidate exists now specifically so Phase 2's
// submission-approval flow has a call to make without this package's shape
// changing — but nothing wires it yet, and there is no R2 write-through.
// Treat this file as the Phase 1 placeholder DESIGN.md describes, not the
// finished architecture.
package export
