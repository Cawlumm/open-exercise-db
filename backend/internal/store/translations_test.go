package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestTranslationStore_GetForRevision(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	_, revisionID := seedExercise(t, ctx, pool, exerciseFixture{
		slug: "lat-pulldown", name: "Lat Pulldown", aliases: []string{"Pulldown"},
		muscleGroup: "back", equipment: "cable", category: "strength",
		instructions: []string{"Grab the bar.", "Pull to chest."},
	})

	// Full Spanish translation: every field supplied, human-origin.
	fullRevisionID := revisionID
	seedTranslation(t, ctx, pool, fullRevisionID, "es",
		"Jalón al pecho", []string{"Dominada asistida"}, []string{"Agarra la barra.", "Tira hacia el pecho."}, "human")

	// A second exercise for the partial-fallback and machine-origin cases,
	// so the three scenarios don't interact through the same revision.
	_, partialRevisionID := seedExercise(t, ctx, pool, exerciseFixture{
		slug: "leg-press", name: "Leg Press",
		muscleGroup: "legs", equipment: "machine", category: "strength",
		instructions: []string{"Sit down.", "Press the platform."},
	})
	// Name supplied, instructions deliberately empty: this is the "partial
	// field" case from docs/PHASE1_BUILD_PLAN.md's WO-2 locale fallback
	// rule — instructions must fall back to English alone, not the record.
	seedTranslation(t, ctx, pool, partialRevisionID, "fr", "Presse à cuisses", nil, nil, "machine")

	// A third exercise with no translation row at all, for the "no
	// translation exists" fallback path.
	_, noTranslationRevisionID := seedExercise(t, ctx, pool, exerciseFixture{
		slug: "plank", name: "Plank",
		muscleGroup: "core", equipment: "bodyweight", category: "strength",
		instructions: []string{"Hold the position."},
	})

	t.Run("locale=en returns the revision's own fields directly", func(t *testing.T) {
		got, err := s.Translations.GetForRevision(ctx, fullRevisionID, "en")
		if err != nil {
			t.Fatalf("GetForRevision: unexpected error: %v", err)
		}
		if got.Name != "Lat Pulldown" || got.NameLocale != "en" {
			t.Errorf("Name = %q (%q), want %q (en)", got.Name, got.NameLocale, "Lat Pulldown")
		}
		if got.NameMachineOrigin || got.InstructionsMachineOrigin || got.AliasesMachineOrigin {
			t.Error("English fallback fields must never report machine origin")
		}
	})

	t.Run("empty locale behaves like en", func(t *testing.T) {
		got, err := s.Translations.GetForRevision(ctx, fullRevisionID, "")
		if err != nil {
			t.Fatalf("GetForRevision: unexpected error: %v", err)
		}
		if got.RequestedLocale != "en" || got.NameLocale != "en" {
			t.Errorf("empty locale did not resolve to en: %+v", got)
		}
	})

	t.Run("full translation: every field served from the requested locale", func(t *testing.T) {
		got, err := s.Translations.GetForRevision(ctx, fullRevisionID, "es")
		if err != nil {
			t.Fatalf("GetForRevision: unexpected error: %v", err)
		}
		if got.Name != "Jalón al pecho" || got.NameLocale != "es" {
			t.Errorf("Name = %q (%q), want %q (es)", got.Name, got.NameLocale, "Jalón al pecho")
		}
		if !reflect.DeepEqual(got.Aliases, []string{"Dominada asistida"}) || got.AliasesLocale != "es" {
			t.Errorf("Aliases = %v (%q), want [Dominada asistida] (es)", got.Aliases, got.AliasesLocale)
		}
		if !reflect.DeepEqual(got.Instructions, []string{"Agarra la barra.", "Tira hacia el pecho."}) || got.InstructionsLocale != "es" {
			t.Errorf("Instructions = %v (%q)", got.Instructions, got.InstructionsLocale)
		}
		if got.NameMachineOrigin {
			t.Error("human-origin translation must not report machine origin")
		}
	})

	t.Run("partial translation: only the populated field is served from the requested locale", func(t *testing.T) {
		got, err := s.Translations.GetForRevision(ctx, partialRevisionID, "fr")
		if err != nil {
			t.Fatalf("GetForRevision: unexpected error: %v", err)
		}
		if got.Name != "Presse à cuisses" || got.NameLocale != "fr" {
			t.Errorf("Name = %q (%q), want %q (fr)", got.Name, got.NameLocale, "Presse à cuisses")
		}
		if !got.NameMachineOrigin {
			t.Error("Name came from a machine-origin row and must report MachineOrigin=true")
		}
		// Instructions and aliases were empty on the fr row, so both must
		// fall back to the revision's English fields individually.
		if got.InstructionsLocale != "en" || !reflect.DeepEqual(got.Instructions, []string{"Sit down.", "Press the platform."}) {
			t.Errorf("Instructions = %v (%q), want English fallback", got.Instructions, got.InstructionsLocale)
		}
		if got.InstructionsMachineOrigin {
			t.Error("English-fallback instructions must not report machine origin")
		}
		if got.AliasesLocale != "en" {
			t.Errorf("AliasesLocale = %q, want en (fell back)", got.AliasesLocale)
		}
	})

	t.Run("no translation row: every field falls back to English", func(t *testing.T) {
		got, err := s.Translations.GetForRevision(ctx, noTranslationRevisionID, "de")
		if err != nil {
			t.Fatalf("GetForRevision: unexpected error: %v", err)
		}
		if got.Name != "Plank" || got.NameLocale != "en" {
			t.Errorf("Name = %q (%q), want %q (en)", got.Name, got.NameLocale, "Plank")
		}
		if got.InstructionsLocale != "en" || !reflect.DeepEqual(got.Instructions, []string{"Hold the position."}) {
			t.Errorf("Instructions = %v (%q), want English fallback", got.Instructions, got.InstructionsLocale)
		}
	})

	t.Run("nonexistent revision returns pgx.ErrNoRows", func(t *testing.T) {
		_, err := s.Translations.GetForRevision(ctx, "00000000-0000-0000-0000-000000000000", "es")
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("error = %v, want pgx.ErrNoRows", err)
		}
	})
}

func TestTranslationStore_ListLocalesForExercise(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	exerciseID, currentRevisionID := seedExercise(t, ctx, pool, exerciseFixture{
		slug: "row", name: "Barbell Row",
		muscleGroup: "back", equipment: "barbell", category: "strength",
		instructions: []string{"Bend over.", "Pull to torso."},
	})

	// Complete: name + instructions both populated.
	seedTranslation(t, ctx, pool, currentRevisionID, "es", "Remo con barra", []string{"Remo"}, []string{"Inclínate.", "Tira al torso."}, "human")
	// Incomplete: instructions left empty.
	seedTranslation(t, ctx, pool, currentRevisionID, "fr", "Rowing barre", nil, nil, "machine")

	// A superseded revision's translation must not appear: it belongs to
	// history, not to what a consumer of the live exercise sees.
	oldRevisionID := seedRevision(t, ctx, pool, exerciseID, "Barbell Row (old)", time.Now().Add(-time.Hour), "superseded")
	seedTranslation(t, ctx, pool, oldRevisionID, "de", "Altes Rudern", nil, nil, "human")

	summaries, err := s.Translations.ListLocalesForExercise(ctx, exerciseID, 1000, 0)
	if err != nil {
		t.Fatalf("ListLocalesForExercise: unexpected error: %v", err)
	}

	if len(summaries) != 2 {
		t.Fatalf("len(summaries) = %d, want 2 (es, fr only — not de from the superseded revision): %+v", len(summaries), summaries)
	}

	byLocale := map[string]struct {
		origin   string
		complete bool
	}{}
	for _, sum := range summaries {
		byLocale[sum.Locale] = struct {
			origin   string
			complete bool
		}{sum.Origin, sum.Complete}
	}

	es, ok := byLocale["es"]
	if !ok || !es.complete || es.origin != "human" {
		t.Errorf("es summary = %+v, want complete=true origin=human", es)
	}
	fr, ok := byLocale["fr"]
	if !ok || fr.complete || fr.origin != "machine" {
		t.Errorf("fr summary = %+v, want complete=false origin=machine", fr)
	}
	if _, ok := byLocale["de"]; ok {
		t.Error("de (superseded revision's translation) must not be listed")
	}
}

func TestTranslationStore_ListLocalesForExercise_NoTranslations(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	exerciseID, _ := seedExercise(t, ctx, pool, exerciseFixture{
		slug: "sit-up", name: "Sit-up",
		muscleGroup: "core", equipment: "bodyweight", category: "strength",
	})

	summaries, err := s.Translations.ListLocalesForExercise(ctx, exerciseID, 1000, 0)
	if err != nil {
		t.Fatalf("ListLocalesForExercise: unexpected error: %v", err)
	}
	if len(summaries) != 0 {
		t.Errorf("len(summaries) = %d, want 0", len(summaries))
	}
}
