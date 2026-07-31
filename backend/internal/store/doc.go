// Package store owns ALL SQL for this project and returns model types.
//
// Layout: one file per aggregate — exercises.go, revisions.go, translations.go,
// submissions.go, media.go, taxonomy.go, users.go. It never imports
// internal/api; dependencies point downward only.
//
// Conventions enforced here (see docs/CONVENTIONS.md section 4): pgxpool is
// injected rather than global, context.Context is the first parameter of every
// method, SELECTs list columns explicitly, and multi-statement writes run in a
// transaction — submission approval must insert a revision and repoint
// exercises.current_revision_id atomically.
package store
