package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
)

func TestGetExercise_NotFound(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	_, err := GetExercise(ctx, s, &GetExerciseInput{ID: "00000000-0000-0000-0000-000000000000"})
	if err == nil {
		t.Fatal("GetExercise(nonexistent): expected error, got nil")
	}

	var statusErr huma.StatusError
	if se, ok := err.(huma.StatusError); ok {
		statusErr = se
	} else {
		t.Fatalf("GetExercise(nonexistent) error type = %T, want huma.StatusError", err)
	}
	if statusErr.GetStatus() != http.StatusNotFound {
		t.Errorf("status = %d, want %d", statusErr.GetStatus(), http.StatusNotFound)
	}
}

func TestGetExercise_ArchivedIsNotFound(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	archivedID, _ := seedExercise(t, ctx, pool, exerciseFixture{
		slug: "archived-lift", name: "Archived Lift",
		muscleGroup: "chest", equipment: "barbell", category: "strength", status: "archived",
	})

	_, err := GetExercise(ctx, s, &GetExerciseInput{ID: archivedID})
	statusErr, ok := err.(huma.StatusError)
	if !ok || statusErr.GetStatus() != http.StatusNotFound {
		t.Fatalf("GetExercise(archived) error = %v, want a 404 huma.StatusError", err)
	}
}

func TestListExerciseRevisions_NotFound(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	_, err := ListExerciseRevisions(ctx, s, &ListExerciseRevisionsInput{ID: "00000000-0000-0000-0000-000000000000"})
	statusErr, ok := err.(huma.StatusError)
	if !ok || statusErr.GetStatus() != http.StatusNotFound {
		t.Fatalf("ListExerciseRevisions(nonexistent) error = %v, want a 404 huma.StatusError", err)
	}
}

func TestListExerciseTranslations_NotFound(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	_, err := ListExerciseTranslations(ctx, s, &ListExerciseTranslationsInput{ID: "00000000-0000-0000-0000-000000000000"})
	statusErr, ok := err.(huma.StatusError)
	if !ok || statusErr.GetStatus() != http.StatusNotFound {
		t.Fatalf("ListExerciseTranslations(nonexistent) error = %v, want a 404 huma.StatusError", err)
	}
}

func TestListExercises_FilterAndPaginationPassthrough(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	seedExercise(t, ctx, pool, exerciseFixture{
		slug: "bench-press", name: "Bench Press", aliases: []string{"Barbell Bench Press"},
		muscleGroup: "chest", equipment: "barbell", category: "strength", mechanic: "compound", level: "beginner",
	})
	seedExercise(t, ctx, pool, exerciseFixture{
		slug: "squat", name: "Squat",
		muscleGroup: "legs", equipment: "barbell", category: "strength", mechanic: "compound", level: "intermediate",
	})
	seedExercise(t, ctx, pool, exerciseFixture{
		slug: "bicep-curl", name: "Bicep Curl",
		muscleGroup: "arms", equipment: "dumbbell", category: "strength", mechanic: "isolation", level: "beginner",
	})

	t.Run("filters pass through to the store unchanged", func(t *testing.T) {
		out, err := ListExercises(ctx, s, &ListExercisesInput{MuscleGroup: "chest"})
		if err != nil {
			t.Fatalf("ListExercises: unexpected error: %v", err)
		}
		if out.Body.Total != 1 || len(out.Body.Exercises) != 1 {
			t.Fatalf("Total/len = %d/%d, want 1/1", out.Body.Total, len(out.Body.Exercises))
		}
		if out.Body.Exercises[0].Translation.Name != "Bench Press" {
			t.Errorf("Name = %q, want Bench Press", out.Body.Exercises[0].Translation.Name)
		}
		if out.CacheControl == "" {
			t.Error("CacheControl header not set")
		}
		if out.ETag == "" {
			t.Error("ETag header not set")
		}
	})

	t.Run("page/per_page pass through to the store unchanged", func(t *testing.T) {
		out, err := ListExercises(ctx, s, &ListExercisesInput{Page: 1, PerPage: 2})
		if err != nil {
			t.Fatalf("ListExercises: unexpected error: %v", err)
		}
		if out.Body.Total != 3 {
			t.Errorf("Total = %d, want 3", out.Body.Total)
		}
		if len(out.Body.Exercises) != 2 {
			t.Fatalf("len(Exercises) = %d, want 2 (per_page=2)", len(out.Body.Exercises))
		}
		if out.Body.Page != 1 || out.Body.PerPage != 2 {
			t.Errorf("Page/PerPage = %d/%d, want 1/2", out.Body.Page, out.Body.PerPage)
		}

		out2, err := ListExercises(ctx, s, &ListExercisesInput{Page: 2, PerPage: 2})
		if err != nil {
			t.Fatalf("ListExercises page 2: unexpected error: %v", err)
		}
		if len(out2.Body.Exercises) != 1 {
			t.Fatalf("len(Exercises) on page 2 = %d, want 1 (3 total, 2 per page)", len(out2.Body.Exercises))
		}
	})

	t.Run("no filter returns every exercise", func(t *testing.T) {
		out, err := ListExercises(ctx, s, &ListExercisesInput{})
		if err != nil {
			t.Fatalf("ListExercises: unexpected error: %v", err)
		}
		if out.Body.Total != 3 {
			t.Errorf("Total = %d, want 3", out.Body.Total)
		}
	})
}

func TestListExercises_LocaleFallbackSurfacedInResponse(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	_, revisionID := seedExercise(t, ctx, pool, exerciseFixture{
		slug: "leg-press", name: "Leg Press",
		muscleGroup: "legs", equipment: "machine", category: "strength",
		instructions: []string{"Sit down.", "Press the platform."},
	})
	// Name supplied for fr, instructions deliberately left empty: this is
	// WO-2's partial-field fallback case (docs/PHASE1_BUILD_PLAN.md), and
	// the point of this test is that the *response*, not just the store,
	// reports instructions falling back to en while name stays fr/machine.
	seedTranslation(t, ctx, pool, revisionID, "fr", "Presse à cuisses", nil, nil, "machine")

	out, err := ListExercises(ctx, s, &ListExercisesInput{Locale: "fr"})
	if err != nil {
		t.Fatalf("ListExercises: unexpected error: %v", err)
	}
	if len(out.Body.Exercises) != 1 {
		t.Fatalf("len(Exercises) = %d, want 1", len(out.Body.Exercises))
	}

	tr := out.Body.Exercises[0].Translation
	if tr.RequestedLocale != "fr" {
		t.Errorf("RequestedLocale = %q, want fr", tr.RequestedLocale)
	}
	if tr.Name != "Presse à cuisses" || tr.NameLocale != "fr" || !tr.NameMachineOrigin {
		t.Errorf("Name/NameLocale/NameMachineOrigin = %q/%q/%v, want %q/fr/true", tr.Name, tr.NameLocale, tr.NameMachineOrigin, "Presse à cuisses")
	}
	if tr.InstructionsLocale != "en" || tr.InstructionsMachineOrigin {
		t.Errorf("InstructionsLocale/MachineOrigin = %q/%v, want en/false (English fallback)", tr.InstructionsLocale, tr.InstructionsMachineOrigin)
	}
	if len(tr.Instructions) != 2 || tr.Instructions[0] != "Sit down." {
		t.Errorf("Instructions = %v, want the English fallback", tr.Instructions)
	}
}

func TestGetExercise_LocaleFallback(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	exerciseID, revisionID := seedExercise(t, ctx, pool, exerciseFixture{
		slug: "lat-pulldown", name: "Lat Pulldown",
		muscleGroup: "back", equipment: "cable", category: "strength",
		instructions: []string{"Grab the bar.", "Pull to chest."},
	})
	seedTranslation(t, ctx, pool, revisionID, "es",
		"Jalón al pecho", []string{"Dominada asistida"}, []string{"Agarra la barra.", "Tira hacia el pecho."}, "human")

	t.Run("no locale requested behaves like en", func(t *testing.T) {
		out, err := GetExercise(ctx, s, &GetExerciseInput{ID: exerciseID})
		if err != nil {
			t.Fatalf("GetExercise: unexpected error: %v", err)
		}
		if out.Body.Translation.RequestedLocale != "en" || out.Body.Translation.Name != "Lat Pulldown" {
			t.Errorf("Translation = %+v, want en/Lat Pulldown", out.Body.Translation)
		}
	})

	t.Run("es locale fully covered", func(t *testing.T) {
		out, err := GetExercise(ctx, s, &GetExerciseInput{ID: exerciseID, Locale: "es"})
		if err != nil {
			t.Fatalf("GetExercise: unexpected error: %v", err)
		}
		tr := out.Body.Translation
		if tr.Name != "Jalón al pecho" || tr.NameLocale != "es" || tr.NameMachineOrigin {
			t.Errorf("Name/NameLocale/MachineOrigin = %q/%q/%v", tr.Name, tr.NameLocale, tr.NameMachineOrigin)
		}
		if tr.InstructionsLocale != "es" {
			t.Errorf("InstructionsLocale = %q, want es", tr.InstructionsLocale)
		}
		// Non-translatable fields must come through unchanged regardless of
		// locale — they're never part of the Translation block.
		if out.Body.MuscleGroup != "back" || out.Body.Equipment != "cable" {
			t.Errorf("MuscleGroup/Equipment = %q/%q, want back/cable", out.Body.MuscleGroup, out.Body.Equipment)
		}
	})
}

// TestListExercises_PerPageCapEnforced exercises huma's own request
// validation (the `maximum:"100"` struct tag), which only runs on the
// registered HTTP path — calling ListExercises directly, as the other tests
// in this file do, bypasses it entirely. humatest is used here specifically
// because this is the one behavior that isn't the handler's own logic.
func TestListExercises_PerPageCapEnforced(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	seedExercise(t, ctx, pool, exerciseFixture{
		slug: "bench-press", name: "Bench Press",
		muscleGroup: "chest", equipment: "barbell", category: "strength",
	})

	_, testAPI := humatest.New(t)
	registerExerciseRoutes(testAPI, s)

	t.Run("per_page over 100 is rejected", func(t *testing.T) {
		resp := testAPI.Get("/api/v1/exercises?per_page=101")
		if resp.Code != http.StatusUnprocessableEntity {
			t.Errorf("status = %d, want %d; body: %s", resp.Code, http.StatusUnprocessableEntity, resp.Body.String())
		}
	})

	t.Run("per_page at the boundary is accepted", func(t *testing.T) {
		resp := testAPI.Get("/api/v1/exercises?per_page=100")
		if resp.Code != http.StatusOK {
			t.Errorf("status = %d, want %d; body: %s", resp.Code, http.StatusOK, resp.Body.String())
		}
	})

	t.Run("per_page omitted defaults to 50", func(t *testing.T) {
		resp := testAPI.Get("/api/v1/exercises")
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body: %s", resp.Code, http.StatusOK, resp.Body.String())
		}
		if !strings.Contains(resp.Body.String(), `"per_page":50`) {
			t.Errorf("body did not echo per_page:50 default: %s", resp.Body.String())
		}
	})
}

// TestListExercises_ConditionalRequestReturns304 covers the defect flagged
// against the first WO-3 pass: etag() computed and set the ETag header but
// nothing ever compared it against an incoming If-None-Match, so every
// revalidation transferred the full body instead of getting a 304. Like the
// per_page cap test above, this needs a real HTTP round trip — the
// short-circuit lives in checkNotModified (api.go), which reads conditional
// headers huma parses onto the embedded conditional.Params before the
// handler runs, so calling ListExercises directly (bypassing huma's request
// pipeline) can't exercise it.
func TestListExercises_ConditionalRequestReturns304(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	seedExercise(t, ctx, pool, exerciseFixture{
		slug: "bench-press", name: "Bench Press",
		muscleGroup: "chest", equipment: "barbell", category: "strength",
	})

	_, testAPI := humatest.New(t)
	registerExerciseRoutes(testAPI, s)

	first := testAPI.Get("/api/v1/exercises")
	if first.Code != http.StatusOK {
		t.Fatalf("initial request status = %d, want 200; body: %s", first.Code, first.Body.String())
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("initial 200 response had no ETag header")
	}

	t.Run("matching If-None-Match short-circuits to a bodyless 304", func(t *testing.T) {
		resp := testAPI.Get("/api/v1/exercises", "If-None-Match: "+etag)
		if resp.Code != http.StatusNotModified {
			t.Fatalf("status = %d, want %d; body: %s", resp.Code, http.StatusNotModified, resp.Body.String())
		}
		if resp.Body.Len() != 0 {
			t.Errorf("304 response had a %d-byte body, want none: %s", resp.Body.Len(), resp.Body.String())
		}
		// RFC 9110 §15.4.5: a 304 must still report the resource's current
		// ETag/Cache-Control, since that's how the client knows what to send
		// on its *next* revalidation.
		if got := resp.Header().Get("ETag"); got != etag {
			t.Errorf("304 ETag = %q, want %q", got, etag)
		}
		if resp.Header().Get("Cache-Control") == "" {
			t.Error("304 response had no Cache-Control header")
		}
	})

	t.Run("stale If-None-Match still returns 200 with the current body", func(t *testing.T) {
		resp := testAPI.Get("/api/v1/exercises", `If-None-Match: "not-the-real-etag"`)
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", resp.Code, resp.Body.String())
		}
	})

	t.Run("comma-separated If-None-Match list matches per RFC 9110 §13.1.1", func(t *testing.T) {
		// No space after the comma here deliberately, to isolate list
		// handling itself from the OWS bug covered separately by
		// TestListExercises_ConditionalRequestHandlesListOWS below.
		resp := testAPI.Get("/api/v1/exercises", `If-None-Match: "not-it",`+etag)
		if resp.Code != http.StatusNotModified {
			t.Fatalf("status = %d, want %d; body: %s", resp.Code, http.StatusNotModified, resp.Body.String())
		}
	})

	t.Run("wildcard If-None-Match matches any existing resource", func(t *testing.T) {
		resp := testAPI.Get("/api/v1/exercises", `If-None-Match: *`)
		if resp.Code != http.StatusNotModified {
			t.Fatalf("status = %d, want %d; body: %s", resp.Code, http.StatusNotModified, resp.Body.String())
		}
	})
}

// TestListExercises_ConditionalRequestHandlesListOWS covers the upstream
// huma v2.39.1 bug documented on checkNotModified (api.go) and reported at
// docs/huma-if-none-match-ows-bug.md: huma splits a comma-separated
// If-None-Match value without trimming the RFC 9110 §5.6.1 OWS (optional
// whitespace) the grammar explicitly permits around each element, so any
// list element after the first with a leading space could never match
// before checkNotModified's trimListOWS workaround. Every case here is a
// list a conformant client is entitled to send.
func TestListExercises_ConditionalRequestHandlesListOWS(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	seedExercise(t, ctx, pool, exerciseFixture{
		slug: "bench-press", name: "Bench Press",
		muscleGroup: "chest", equipment: "barbell", category: "strength",
	})

	_, testAPI := humatest.New(t)
	registerExerciseRoutes(testAPI, s)

	first := testAPI.Get("/api/v1/exercises")
	if first.Code != http.StatusOK {
		t.Fatalf("initial request status = %d, want 200; body: %s", first.Code, first.Body.String())
	}
	etag := first.Header().Get("ETag") // quoted, e.g. `"09b6afd..."`
	if etag == "" {
		t.Fatal("initial 200 response had no ETag header")
	}
	bareTag := strings.Trim(etag, `"`)
	weakTag := `W/"` + bareTag + `"` // a weak validator for the same tag

	tests := []struct {
		name   string
		header string
	}{
		{"single space after comma, target last", `If-None-Match: "other", ` + etag},
		{"multiple spaces after comma, target last", `If-None-Match: "other",    ` + etag},
		{"tab after comma, target last", "If-None-Match: \"other\",\t" + etag},
		{"space after comma, target first", `If-None-Match: ` + etag + `, "other"`},
		{"space after comma, target middle", `If-None-Match: "a", ` + etag + `, "other"`},
		{"space after comma, weak tag for the same target", `If-None-Match: "other", ` + weakTag},
		{"tab and space mixed, target last", "If-None-Match: \"other\", \t " + etag},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := testAPI.Get("/api/v1/exercises", tt.header)
			if resp.Code != http.StatusNotModified {
				t.Fatalf("status = %d, want %d (%s); body: %s", resp.Code, http.StatusNotModified, tt.header, resp.Body.String())
			}
		})
	}

	t.Run("OWS around a non-matching list still returns 200", func(t *testing.T) {
		resp := testAPI.Get("/api/v1/exercises", `If-None-Match: "one",  "two",   "three"`)
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", resp.Code, resp.Body.String())
		}
	})
}

// TestGetExercise_ConditionalRequestReturns304 confirms the fix isn't
// specific to the list endpoint — every handler in this package routes
// through the same checkNotModified helper.
func TestGetExercise_ConditionalRequestReturns304(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	exerciseID, _ := seedExercise(t, ctx, pool, exerciseFixture{
		slug: "squat", name: "Squat",
		muscleGroup: "legs", equipment: "barbell", category: "strength",
	})

	_, testAPI := humatest.New(t)
	registerExerciseRoutes(testAPI, s)

	first := testAPI.Get("/api/v1/exercises/" + exerciseID)
	if first.Code != http.StatusOK {
		t.Fatalf("initial request status = %d, want 200; body: %s", first.Code, first.Body.String())
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("initial 200 response had no ETag header")
	}

	resp := testAPI.Get("/api/v1/exercises/"+exerciseID, "If-None-Match: "+etag)
	if resp.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want %d; body: %s", resp.Code, http.StatusNotModified, resp.Body.String())
	}
	if resp.Body.Len() != 0 {
		t.Errorf("304 response had a body: %s", resp.Body.String())
	}
}
