package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// These tests cover a category the rest of the suite did not: what a caller who
// makes an ordinary mistake gets back. Every case here was found by driving the
// running API the way a confused consumer would, and each one previously
// produced a confident, wrong-looking success rather than an error.

func TestNormalizeLocale(t *testing.T) {
	// BCP-47 tags are case-insensitive (RFC 5646 2.1.1), but every lookup is a
	// literal string comparison, so before normalization a perfectly valid
	// "EN" or "es-mx" matched nothing and fell silently back to English.
	cases := map[string]string{
		"":         "",
		"en":       "en",
		"EN":       "en",
		"Es":       "es",
		"es-MX":    "es-MX",
		"es-mx":    "es-MX",
		"ES-mx":    "es-MX",
		"  es  ":   "es",
		"pt-br":    "pt-BR",
		"zz":       "zz", // not a real language, but canonicalizing is not validating
		"-leading": "-leading",
	}
	for in, want := range cases {
		if got := normalizeLocale(in); got != want {
			t.Errorf("normalizeLocale(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBaseLanguageFallback_ServesLanguageForRegionalRequest(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	id, revID := seedExercise(t, ctx, pool, exerciseFixture{
		slug: "regional-lift", name: "Regional Lift",
		muscleGroup: "chest", equipment: "barbell", category: "strength",
	})
	seedTranslation(t, ctx, pool, revID, "es", "Levantamiento regional", nil, []string{"Paso uno"}, "human")

	// es-MX has no translation of its own. A region subtag is a refinement of
	// a language, not a different one, so Spanish is the right answer — before
	// this, the exact-match lookup missed and the caller silently got English,
	// which is the wrong language rather than a less specific right one.
	out, err := GetExercise(ctx, s, "", &GetExerciseInput{ID: id, Locale: "es-MX"})
	if err != nil {
		t.Fatalf("GetExercise: %v", err)
	}

	tr := out.Body.Translation
	if tr.Name != "Levantamiento regional" {
		t.Errorf("name = %q, want the Spanish translation", tr.Name)
	}
	// Reports the locale actually served, not the one requested: claiming
	// "es-MX" would assert a regional translation that does not exist.
	if tr.NameLocale != "es" {
		t.Errorf("name_locale = %q, want \"es\" (the locale actually served)", tr.NameLocale)
	}
	if tr.RequestedLocale != "es-MX" {
		t.Errorf("requested_locale = %q, want \"es-MX\" echoed back", tr.RequestedLocale)
	}
}

func TestBaseLanguageFallback_ExactRegionalWins(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	id, revID := seedExercise(t, ctx, pool, exerciseFixture{
		slug: "both-locales-lift", name: "Both Locales Lift",
		muscleGroup: "chest", equipment: "barbell", category: "strength",
	})
	seedTranslation(t, ctx, pool, revID, "es", "Genérico", nil, []string{"Genérico"}, "human")
	seedTranslation(t, ctx, pool, revID, "es-MX", "Mexicano", nil, []string{"Mexicano"}, "human")

	// Both exist, so the more specific one must win — otherwise the fallback
	// would actively downgrade a caller who asked for exactly what is there.
	out, err := GetExercise(ctx, s, "", &GetExerciseInput{ID: id, Locale: "es-MX"})
	if err != nil {
		t.Fatalf("GetExercise: %v", err)
	}
	if out.Body.Translation.Name != "Mexicano" {
		t.Errorf("name = %q, want the es-MX translation to beat the es one", out.Body.Translation.Name)
	}
	if got := out.Body.Translation.NameLocale; got != "es-MX" {
		t.Errorf("name_locale = %q, want \"es-MX\"", got)
	}
}

func TestUnknownFilterValue_IsRejectedWithGuidance(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	// Seeds the vocabularies these cases are validated against. Required, not
	// incidental: validation deliberately fails open on a facet with no terms
	// (see store.UnknownSlugs), so without seeding, every case below would
	// "pass" by never rejecting anything.
	seedVocabulary(t, ctx, pool)

	// Each of these previously answered 200 with an empty list, which is
	// indistinguishable from a correct filter that legitimately matches
	// nothing — so a client could not tell "no data" from "you typed it wrong".
	cases := []struct {
		name        string
		input       *ListExercisesInput
		wantInMsg   []string
		wantLoc     string
		wantSuggest bool
	}{
		{
			name:        "wrong case",
			input:       &ListExercisesInput{MuscleGroup: "Chest"},
			wantInMsg:   []string{"unknown muscle_group", "Chest", "/api/v1/meta/muscle-groups"},
			wantLoc:     "query.muscle_group",
			wantSuggest: true,
		},
		{
			name:      "plural guess gets no bogus suggestion",
			input:     &ListExercisesInput{MuscleGroup: "chests"},
			wantInMsg: []string{"unknown muscle_group", "chests"},
			wantLoc:   "query.muscle_group",
		},
		{
			name:      "nonsense equipment",
			input:     &ListExercisesInput{Equipment: "jetpack"},
			wantInMsg: []string{"unknown equipment", "jetpack"},
			wantLoc:   "query.equipment",
		},
		{
			name: "small vocabulary is listed in full",
			// "advanced" is the value this parameter's own doc string used to
			// claim was valid; the real top level is "expert".
			input:     &ListExercisesInput{Level: "advanced"},
			wantInMsg: []string{"unknown level", "advanced", "beginner", "expert", "intermediate"},
			wantLoc:   "query.level",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ListExercises(ctx, s, "", c.input)
			if err == nil {
				t.Fatal("want a validation error, got nil — an unknown filter value must not read as an empty result")
			}

			se, ok := err.(huma.StatusError)
			if !ok {
				t.Fatalf("got %T, want a huma.StatusError", err)
			}
			if se.GetStatus() != http.StatusUnprocessableEntity {
				t.Errorf("status = %d, want 422", se.GetStatus())
			}

			raw, _ := json.Marshal(err)
			body := string(raw)
			for _, want := range c.wantInMsg {
				if !strings.Contains(body, want) {
					t.Errorf("error body does not mention %q\nbody: %s", want, body)
				}
			}
			if !strings.Contains(body, c.wantLoc) {
				t.Errorf("error body does not point at %q\nbody: %s", c.wantLoc, body)
			}
			if c.wantSuggest && !strings.Contains(body, "did you mean") {
				t.Errorf("a case-only mistake should suggest the right slug\nbody: %s", body)
			}
		})
	}
}

func TestKnownFilterValues_StillPass(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	seedVocabulary(t, ctx, pool)

	// The guard above must not reject legitimate queries — including a valid
	// slug that happens to match no exercises, which is a real empty result and
	// must stay a 200.
	for _, in := range []*ListExercisesInput{
		{},
		{MuscleGroup: "chest"},
		{MuscleGroup: "chest", Equipment: "barbell", Category: "strength"},
		{Level: "expert"},
	} {
		if _, err := ListExercises(ctx, s, "", in); err != nil {
			t.Errorf("ListExercises(%+v) = %v, want success", in, err)
		}
	}
}

// TestUnseededVocabulary_DoesNotRejectEverything covers the failure mode the
// filter validation itself could introduce.
//
// Validation asks "is this slug in taxonomy_terms?", so against an unseeded or
// partially-seeded table the answer is always no — and every filtered request
// would be rejected. A deployment that had merely skipped taxonomy seeding
// would then answer 422 to all filtering and look comprehensively broken,
// which is strictly worse than the unvalidated behavior it replaced. Fails
// open per facet instead: no vocabulary, no opinion.
//
// Deliberately does NOT call seedVocabulary — an empty taxonomy is the whole
// point of the test.
func TestUnseededVocabulary_DoesNotRejectEverything(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	for _, in := range []*ListExercisesInput{
		{MuscleGroup: "chest"},
		{MuscleGroup: "anything-at-all"},
		{Equipment: "barbell", Category: "strength", Level: "expert"},
	} {
		if _, err := ListExercises(ctx, s, "", in); err != nil {
			t.Errorf("ListExercises(%+v) = %v; with no seeded vocabulary the filter must be accepted, "+
				"not rejected — otherwise an unseeded deployment 422s every filtered request", in, err)
		}
	}
}

func TestCORS_PreflightIsAnswered(t *testing.T) {
	// Before this, OPTIONS matched no operation and ServeMux answered 405, so
	// a browser never sent the real request at all — the API was unusable from
	// any web page regardless of what the endpoints did.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/exercises", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	h := CORS(ProblemErrors(mux))

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/exercises", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Headers", "If-None-Match")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Allow-Origin = %q, want *", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "If-None-Match" {
		t.Errorf("Allow-Headers = %q, want the requested header echoed", got)
	}
	if !corsVaryContains(rec.Header().Get("Vary"), "Access-Control-Request-Headers") {
		t.Errorf("Vary = %q, must include Access-Control-Request-Headers when echoing them",
			rec.Header().Get("Vary"))
	}
}

func TestCORS_ExposesProtocolHeadersToScripts(t *testing.T) {
	// A browser script can only read headers named in Access-Control-Expose-
	// Headers. ETag drives conditional requests and the RateLimit-* family
	// drives the back-off docs/API_POLICY.md asks consumers to implement, so
	// omitting them would leave browser clients unable to follow the rules the
	// API sets for them.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/exercises", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/exercises", nil)
	req.Header.Set("Origin", "https://app.example.com")
	CORS(mux).ServeHTTP(rec, req)

	exposed := rec.Header().Get("Access-Control-Expose-Headers")
	for _, want := range []string{"ETag", "RateLimit-Limit", "RateLimit-Remaining", "RateLimit-Reset", "Retry-After"} {
		if !strings.Contains(exposed, want) {
			t.Errorf("Expose-Headers %q does not include %q", exposed, want)
		}
	}
}

func TestCORS_HeadersPresentOnErrors(t *testing.T) {
	// A 429 a browser cannot read is a 429 a browser cannot obey.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/exercises", func(w http.ResponseWriter, _ *http.Request) {})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/nope", nil)
	req.Header.Set("Origin", "https://app.example.com")
	CORS(ProblemErrors(mux)).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Allow-Origin on an error = %q, want *", got)
	}
}

// seedVocabulary installs the subset of the real taxonomy these tests filter
// on. Mirrors the seeded values the running API has (verified against the
// database: levels are beginner/expert/intermediate — notably *not*
// "advanced", which this API's own parameter documentation used to claim).
func seedVocabulary(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()

	vocab := map[string][]string{
		"muscle_group": {"abdominals", "abductors", "adductors", "biceps", "calves", "chest",
			"forearms", "glutes", "hamstrings", "lats", "lower-back", "middle-back",
			"neck", "quadriceps", "shoulders", "traps", "triceps"},
		"equipment": {"bands", "barbell", "body-only", "cable", "dumbbell", "e-z-curl-bar",
			"exercise-ball", "foam-roll", "kettlebells", "machine", "medicine-ball",
			"other", "none"},
		"category": {"cardio", "olympic-weightlifting", "plyometrics", "powerlifting",
			"strength", "stretching", "strongman"},
		"level":    {"beginner", "expert", "intermediate"},
		"mechanic": {"compound", "isolation"},
	}

	for facet, slugs := range vocab {
		for _, slug := range slugs {
			seedTaxonomyTerm(t, ctx, pool, facet, slug, map[string]string{"en": slug})
		}
	}
}

// TestSearchTreatsWildcardsLiterally covers the second round of misuse probing.
//
// The search term was interpolated straight into a LIKE pattern, so LIKE's
// metacharacters were live in user input: `?q=%` produced the pattern `%%%` and
// returned all 873 exercises, and `?q=_` returned everything at least one
// character long. Parameterization already prevented injection — this was never
// a security hole — but a search for a literal "%" answering with the entire
// dataset is a confidently wrong 200, which is the failure mode this project
// keeps finding.
func TestSearchTreatsWildcardsLiterally(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	seedExercise(t, ctx, pool, exerciseFixture{
		slug: "plain-press", name: "Plain Press",
		muscleGroup: "chest", equipment: "barbell", category: "strength",
	})
	// Names carrying the metacharacters themselves, so "matched literally" is
	// distinguishable from "matched nothing".
	seedExercise(t, ctx, pool, exerciseFixture{
		slug: "hundred-percent", name: "100% Effort Row",
		muscleGroup: "chest", equipment: "barbell", category: "strength",
	})
	seedExercise(t, ctx, pool, exerciseFixture{
		slug: "under-score", name: "Under_score Curl",
		muscleGroup: "chest", equipment: "barbell", category: "strength",
	})

	total := func(t *testing.T, q string) int {
		t.Helper()
		out, err := ListExercises(ctx, s, "", &ListExercisesInput{Q: q})
		if err != nil {
			t.Fatalf("ListExercises(q=%q): %v", q, err)
		}
		return out.Body.Total
	}

	// A bare wildcard must match only the rows that literally contain it, not
	// every row in the table.
	if got := total(t, "%"); got != 1 {
		t.Errorf("q=%% matched %d exercises, want 1 (only \"100%% Effort Row\") — "+
			"a literal percent must not behave as a wildcard", got)
	}
	if got := total(t, "_"); got != 1 {
		t.Errorf("q=_ matched %d exercises, want 1 (only \"Under_score Curl\")", got)
	}
	// A term a user would plausibly type, where the metacharacter is incidental.
	if got := total(t, "100%"); got != 1 {
		t.Errorf("q=100%% matched %d, want 1", got)
	}
	// Wildcards the user typed around a real word are now literal, so they
	// match nothing rather than silently working.
	if got := total(t, "%Plain%"); got != 0 {
		t.Errorf("q=%%Plain%% matched %d, want 0 — the wildcards are part of the search term now", got)
	}
	// Ordinary search is unaffected, and stays case-insensitive.
	if got := total(t, "plain"); got != 1 {
		t.Errorf("q=plain matched %d, want 1", got)
	}
	if got := total(t, "PLAIN"); got != 1 {
		t.Errorf("q=PLAIN matched %d, want 1 — search must stay case-insensitive", got)
	}
	// A backslash must not escape the character after it, nor break the query.
	if got := total(t, `\`); got != 0 {
		t.Errorf(`q=\ matched %d, want 0`, got)
	}
}

func TestEscapeLikePattern_EscapesBackslashFirst(t *testing.T) {
	// Order matters and is easy to get backwards: escaping % before \ would
	// re-escape the backslashes just introduced, turning "%" into a literal
	// backslash followed by a still-live wildcard — worse than not escaping at
	// all.
	cases := map[string]string{
		"plain": "plain",
		"%":     `\%`,
		"_":     `\_`,
		`\`:     `\\`,
		`\%`:    `\\\%`,
		"100%":  `100\%`,
		"a_b%c": `a\_b\%c`,
	}
	for in, want := range cases {
		if got := store.StoreEscapeLikePattern(in); got != want {
			t.Errorf("escapeLikePattern(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSearchIsWordOrderIndependent covers the biggest usability gap found by
// probing: the whole query was matched as one substring, so word order was
// significant. `q=bench press` found 21 exercises and `q=press bench` found
// none — the same intent, and the empty answer indistinguishable from "we have
// nothing like that".
func TestSearchIsWordOrderIndependent(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	for _, name := range []string{
		"Barbell Bench Press", "Dumbbell Bench Press", "Bench Dips", "Barbell Squat",
	} {
		seedExercise(t, ctx, pool, exerciseFixture{
			slug: slugify(name), name: name,
			muscleGroup: "chest", equipment: "barbell", category: "strength",
		})
	}

	total := func(q string) int {
		t.Helper()
		out, err := ListExercises(ctx, s, "", &ListExercisesInput{Q: q})
		if err != nil {
			t.Fatalf("ListExercises(q=%q): %v", q, err)
		}
		return out.Body.Total
	}

	if a, b := total("bench press"), total("press bench"); a != b {
		t.Errorf("q=%q found %d but q=%q found %d; word order must not matter",
			"bench press", a, "press bench", b)
	}
	if got := total("bench press"); got != 2 {
		t.Errorf("q=\"bench press\" matched %d, want 2 (both bench presses)", got)
	}
	// Adding a word must narrow, never widen — the AND semantics a type-ahead
	// depends on as the user keeps typing.
	if narrow, broad := total("barbell bench press"), total("bench press"); narrow > broad {
		t.Errorf("adding a word widened the search: %d > %d", narrow, broad)
	}
	if got := total("barbell bench press"); got != 1 {
		t.Errorf("q=\"barbell bench press\" matched %d, want 1", got)
	}
	// Extra whitespace is not a term.
	if a, b := total("  bench   press  "), total("bench press"); a != b {
		t.Errorf("whitespace changed the result: %d vs %d", a, b)
	}
}

// TestSearchRanksBestMatchFirst pins the ordering a type-ahead depends on. With
// plain alphabetical order, `q=bench` put "Barbell Bench Press" ahead of
// "Bench Dips", so a suggestion list showing three results never showed the
// ones whose names actually start with what was typed.
func TestSearchRanksBestMatchFirst(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	for _, name := range []string{
		"Zercher Squat",       // contains, alphabetically last
		"Squat",               // exact match
		"Squat Jerk",          // prefix match
		"Barbell Squat Press", // contains, alphabetically first
	} {
		seedExercise(t, ctx, pool, exerciseFixture{
			slug: slugify(name), name: name,
			muscleGroup: "chest", equipment: "barbell", category: "strength",
		})
	}

	out, err := ListExercises(ctx, s, "", &ListExercisesInput{Q: "squat"})
	if err != nil {
		t.Fatalf("ListExercises: %v", err)
	}
	if len(out.Body.Exercises) < 3 {
		t.Fatalf("got %d results, want at least 3", len(out.Body.Exercises))
	}

	got := []string{}
	for _, e := range out.Body.Exercises {
		got = append(got, e.Translation.Name)
	}
	if got[0] != "Squat" {
		t.Errorf("first result is %q, want the exact match %q (order: %v)", got[0], "Squat", got)
	}
	if got[1] != "Squat Jerk" {
		t.Errorf("second result is %q, want the prefix match %q (order: %v)", got[1], "Squat Jerk", got)
	}
}

// TestSearchPagesAreStableAndComplete guards the property relevance ordering
// could quietly break: a non-total ORDER BY lets rows shift between the query
// for page 1 and the query for page 2, silently duplicating or dropping them.
func TestSearchPagesAreStableAndComplete(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	// Deliberately many identical names, so the CASE rank and the length
	// tiebreak are both ties and only the final e.id tiebreak separates them.
	for i := range 12 {
		seedExercise(t, ctx, pool, exerciseFixture{
			slug: "tied-press-" + string(rune('a'+i)), name: "Tied Press",
			muscleGroup: "chest", equipment: "barbell", category: "strength",
		})
	}

	seen := map[string]bool{}
	const perPage = 5
	for page := 1; page <= 3; page++ {
		out, err := ListExercises(ctx, s, "", &ListExercisesInput{
			Q: "tied press", PageParams: PageParams{Page: page, PerPage: perPage},
		})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, e := range out.Body.Exercises {
			if seen[e.ID] {
				t.Errorf("exercise %s (%s) appeared on more than one page", e.Slug, e.ID)
			}
			seen[e.ID] = true
		}
	}
	if len(seen) != 12 {
		t.Errorf("paged through %d distinct exercises, want 12 — rows were dropped across page boundaries", len(seen))
	}
}

// TestSlugFilter closes a gap rather than fixing a wrong answer: every response
// carries a slug, but there was no way to look one back up. The path takes a
// UUID, so a consumer holding a slug had only substring search, which can match
// several rows or none.
func TestSlugFilter(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	seedExercise(t, ctx, pool, exerciseFixture{
		slug: "bench-press", name: "Bench Press",
		muscleGroup: "chest", equipment: "barbell", category: "strength",
	})
	// A second slug that *contains* the first, so an exact match is
	// distinguishable from a substring one.
	seedExercise(t, ctx, pool, exerciseFixture{
		slug: "bench-press-wide", name: "Bench Press Wide",
		muscleGroup: "chest", equipment: "barbell", category: "strength",
	})

	out, err := ListExercises(ctx, s, "", &ListExercisesInput{Slug: "bench-press"})
	if err != nil {
		t.Fatalf("ListExercises: %v", err)
	}
	if out.Body.Total != 1 {
		t.Fatalf("total = %d, want exactly 1 — slug is an exact match, not a prefix", out.Body.Total)
	}
	if got := out.Body.Exercises[0].Slug; got != "bench-press" {
		t.Errorf("slug = %q, want %q", got, "bench-press")
	}

	// An unknown slug is a legitimate empty result, not a validation error:
	// unlike a taxonomy filter, slugs are not a closed vocabulary published by
	// a /meta endpoint that a caller could have consulted first.
	empty, err := ListExercises(ctx, s, "", &ListExercisesInput{Slug: "no-such-slug"})
	if err != nil {
		t.Fatalf("unknown slug should be an empty result, got error: %v", err)
	}
	if empty.Body.Total != 0 {
		t.Errorf("total = %d, want 0", empty.Body.Total)
	}
}

// slugify is a test helper only — the real slugs come from the importer.
func slugify(name string) string {
	out := []rune{}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out = append(out, r)
		case r >= 'A' && r <= 'Z':
			out = append(out, r+32)
		default:
			if len(out) > 0 && out[len(out)-1] != '-' {
				out = append(out, '-')
			}
		}
	}
	return string(out)
}
