package store

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func approvedTranslation(t *testing.T, ctx context.Context, pool *pgxpool.Pool, revisionID, locale string) (name, origin string, n int) {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT name, origin FROM exercise_translations
		WHERE revision_id = $1 AND locale = $2 AND status = 'approved'`, revisionID, locale)
	if err != nil {
		t.Fatalf("query translations: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := rows.Scan(&name, &origin); err != nil {
			t.Fatalf("scan: %v", err)
		}
		n++
	}
	return name, origin, n
}

func TestImportMachine(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	_, squatRev := seedExercise(t, ctx, pool, exerciseFixture{slug: "squat", sourceID: "Squat", name: "Squat", muscleGroup: "quadriceps", equipment: "barbell", category: "strength", instructions: []string{"Stand.", "Sit down."}})
	_, plankRev := seedExercise(t, ctx, pool, exerciseFixture{slug: "plank", sourceID: "Plank", name: "Plank", muscleGroup: "abdominals", equipment: "body-only", category: "strength", instructions: []string{"Hold."}})
	_, curlRev := seedExercise(t, ctx, pool, exerciseFixture{slug: "curl", sourceID: "Curl", name: "Curl", muscleGroup: "biceps", equipment: "dumbbell", category: "strength", instructions: []string{"Lift."}})
	seedTranslation(t, ctx, pool, curlRev, "es", "Curl humano", nil, []string{"Levanta."}, "human")
	seedExercise(t, ctx, pool, exerciseFixture{slug: "row", sourceID: "Row", name: "Row", muscleGroup: "back", equipment: "cable", category: "strength"})

	items := []MachineTranslation{
		{SourceID: "Squat", EnglishName: "Squat", Name: "Sentadilla", Instructions: []string{"Ponte de pie.", "Siéntate."}},
		{SourceID: "Plank", EnglishName: "Planks", Name: "Plancha", Instructions: []string{"Mantén."}}, // English name moved on
		{SourceID: "Curl", EnglishName: "Curl", Name: "Curl máquina", Instructions: []string{"Sube."}}, // a human already translated it
		{SourceID: "Row", EnglishName: "Row", Name: "Remo", Instructions: []string{"One", "too many"}}, // step count differs
		{SourceID: "Missing", EnglishName: "Missing", Name: "Nada", Instructions: []string{"x"}},       // no such exercise
	}

	t.Run("dry run counts and writes nothing", func(t *testing.T) {
		res, err := s.Translations.ImportMachine(ctx, "es", items, true)
		if err != nil {
			t.Fatalf("import: %v", err)
		}
		if res.Inserted != 1 {
			t.Errorf("dry-run Inserted = %d, want 1", res.Inserted)
		}
		if _, _, n := approvedTranslation(t, ctx, pool, squatRev, "es"); n != 0 {
			t.Errorf("dry run left %d rows", n)
		}
	})

	t.Run("real run", func(t *testing.T) {
		res, err := s.Translations.ImportMachine(ctx, "es", items, false)
		if err != nil {
			t.Fatalf("import: %v", err)
		}
		want := MachineImportResult{Inserted: 1, Stale: 1, KeptHuman: 1, Mismatched: 1, NotFound: 1}
		if res != want {
			t.Errorf("result = %+v, want %+v", res, want)
		}
		if name, origin, n := approvedTranslation(t, ctx, pool, squatRev, "es"); n != 1 || name != "Sentadilla" || origin != "machine" {
			t.Errorf("squat = %q/%q (%d rows), want Sentadilla/machine (1)", name, origin, n)
		}
		if _, _, n := approvedTranslation(t, ctx, pool, plankRev, "es"); n != 0 {
			t.Errorf("stale item was written")
		}
		if name, origin, _ := approvedTranslation(t, ctx, pool, curlRev, "es"); name != "Curl humano" || origin != "human" {
			t.Errorf("human translation overwritten: %q/%q", name, origin)
		}
	})

	t.Run("rerun is a no-op, a changed draft supersedes the old one", func(t *testing.T) {
		res, err := s.Translations.ImportMachine(ctx, "es", items[:1], false)
		if err != nil {
			t.Fatalf("rerun: %v", err)
		}
		if res.Unchanged != 1 || res.Inserted != 0 {
			t.Errorf("rerun = %+v, want one Unchanged", res)
		}

		changed := []MachineTranslation{{SourceID: "Squat", EnglishName: "Squat", Name: "Sentadilla trasera", Instructions: []string{"Ponte de pie.", "Siéntate."}}}
		res, err = s.Translations.ImportMachine(ctx, "es", changed, false)
		if err != nil {
			t.Fatalf("change: %v", err)
		}
		if res.Replaced != 1 {
			t.Errorf("change = %+v, want one Replaced", res)
		}
		if name, _, n := approvedTranslation(t, ctx, pool, squatRev, "es"); n != 1 || name != "Sentadilla trasera" {
			t.Errorf("after replace = %q (%d approved rows), want one Sentadilla trasera", name, n)
		}
		var superseded int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM exercise_translations WHERE revision_id = $1 AND locale = 'es' AND status = 'superseded'`, squatRev).Scan(&superseded); err != nil || superseded != 1 {
			t.Errorf("superseded rows = %d (err %v), want 1", superseded, err)
		}
	})

	t.Run("empty name is an error", func(t *testing.T) {
		_, err := s.Translations.ImportMachine(ctx, "es", []MachineTranslation{{SourceID: "Squat", EnglishName: "Squat", Name: " "}}, false)
		if err == nil {
			t.Fatal("want an error for an empty name")
		}
	})
}
