// Package api holds the HTTP surface: huma operation registrations, their
// input/output types, and the handlers themselves.
//
// Built on github.com/danielgtaylor/huma/v2 over stdlib net/http, so the
// OpenAPI 3.1 spec is generated from these types and cannot drift from the
// handlers (docs/CONVENTIONS.md section 3).
//
// Handlers never write SQL — they call internal/store. Response types are
// defined here rather than reusing store rows directly, because the published
// shape is this project's own public API contract and must not change just
// because a column was renamed. GET /exercises/export (WO-5,
// internal/export + export.go) reuses those same field names deliberately,
// so a consumer moving between the bulk dump and live lookups doesn't
// rewrite its mapping — it is not shaped to any single downstream
// consumer's schema (an earlier draft of this comment said otherwise,
// naming lyftr specifically; that was wrong, see docs/DESIGN.md's
// "Consuming this API" section).
package api
