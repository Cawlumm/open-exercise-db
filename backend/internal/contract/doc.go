// Package contract holds the canonical wire shape for a published exercise —
// the one Go type both GET /exercises (internal/api) and GET
// /exercises/export (internal/export) render, so the two literally cannot
// drift in field names or JSON tags: there is exactly one struct, not two
// independently maintained copies.
//
// This package was extracted after WO-5 initially shipped with its own
// independently declared export.Exercise duplicating internal/api.Exercise
// field-for-field. Both types were named "Exercise", and huma's OpenAPI
// component-schema registry keys schemas by Go type name (not full import
// path) — the moment internal/api.Register wired both packages' routes into
// one huma.API, registration panicked with "duplicate name: Exercise, new
// type: export.Exercise, existing type: api.Exercise" and the server could
// not start. A key-set-equality test between the two endpoints (still in
// internal/api/export_test.go) caught the *symptom* — it diffs two JSON
// shapes and would fail if they diverged — but it could not catch this,
// because it never called huma's own registration path; both types could be
// individually correct and JSON-identical while still being two distinct Go
// types that panic huma on contact. That gap is also why
// internal/api/register_test.go now calls the real api.Register against a
// real huma.API: it's the only test that exercises schema registration at
// all.
//
// Why a new package rather than one of internal/api or internal/export
// owning the type outright: internal/api already imports internal/export
// (to reach export.Cache for GET /exercises/export), so internal/export
// importing internal/api back would be a cycle — that rules out "export
// consumes api.Exercise." The reverse, "api consumes export.Exercise," would
// compile, but every non-export read endpoint (GET /exercises, GET
// /exercises/{id}) would then depend on a type owned by the package whose
// entire job (per internal/export's own doc comment) is the pre-generated
// bulk-dump cache — a confusing ownership story for a type three other
// unrelated handlers also return. Neither package actually "owns" this
// shape conceptually; both are consumers of one public contract. A small
// package holding just that contract, importable by both without a cycle,
// matches docs/CONVENTIONS.md section 2's one-way dependency rule the same
// way internal/model does for store-layer types — except this is
// deliberately not internal/model, because docs/CONVENTIONS.md section 2 and
// internal/api/doc.go are explicit that the published API shape must stay
// decoupled from database/store types precisely so a column rename doesn't
// silently change the public contract; folding this into internal/model
// would blur exactly the line that rule exists to keep sharp.
//
// Plain structs only, no behavior, no imports from this project — same rule
// internal/model documents for itself.
package contract
