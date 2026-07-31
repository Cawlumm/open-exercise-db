// Package seed holds bootstrap data logic: the free-exercise-db Day-1 import
// (driven by cmd/import-freeexercisedb) and the Phase 1.5 gap-fill enrichment
// that diffs wger and exercemus against what is already imported.
//
// Licensing matters here — see docs/DESIGN.md, Bootstrap data. free-exercise-db
// is Unlicense and imports cleanly; wger is CC-BY-SA 4.0 and is used as a
// gap-finder rather than a bulk import so the corpus stays CC0.
package seed
