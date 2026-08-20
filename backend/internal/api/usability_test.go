package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
			input:       &ListExercisesInput{MuscleGroup: []string{"Chest"}},
			wantInMsg:   []string{"unknown muscle_group", "Chest", "/api/v1/meta/muscle-groups"},
			wantLoc:     "query.muscle_group",
			wantSuggest: true,
		},
		{
			name:      "plural guess gets no bogus suggestion",
			input:     &ListExercisesInput{MuscleGroup: []string{"chests"}},
			wantInMsg: []string{"unknown muscle_group", "chests"},
			wantLoc:   "query.muscle_group",
		},
		{
			name:      "nonsense equipment",
			input:     &ListExercisesInput{Equipment: []string{"jetpack"}},
			wantInMsg: []string{"unknown equipment", "jetpack"},
			wantLoc:   "query.equipment",
		},
		{
			name: "small vocabulary is listed in full",
			// "advanced" is the value this parameter's own doc string used to
			// claim was valid; the real top level is "expert".
			input:     &ListExercisesInput{Level: []string{"advanced"}},
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
		{MuscleGroup: []string{"chest"}},
		{MuscleGroup: []string{"chest"}, Equipment: []string{"barbell"}, Category: []string{"strength"}},
		{Level: []string{"expert"}},
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
		{MuscleGroup: []string{"chest"}},
		{MuscleGroup: []string{"anything-at-all"}},
		{Equipment: []string{"barbell"}, Category: []string{"strength"}, Level: []string{"expert"}},
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

	// Equipment is per-exercise rather than "barbell" for everything. It was
	// uniform when search only looked at names, so a dumbbell exercise labelled
	// barbell changed nothing; now that a word may match the taxonomy, seeding
	// "Dumbbell Bench Press" as barbell equipment would make it a legitimate hit
	// for `q=barbell bench press` and quietly move what this test measures.
	for _, f := range []struct{ name, equipment string }{
		{"Barbell Bench Press", "barbell"},
		{"Dumbbell Bench Press", "dumbbell"},
		{"Bench Dips", "body-only"},
		{"Barbell Squat", "barbell"},
	} {
		seedExercise(t, ctx, pool, exerciseFixture{
			slug: slugify(f.name), name: f.name,
			muscleGroup: "chest", equipment: f.equipment, category: "strength",
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

	out, err := ListExercises(ctx, s, "", &ListExercisesInput{Slug: []string{"bench-press"}})
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
	empty, err := ListExercises(ctx, s, "", &ListExercisesInput{Slug: []string{"no-such-slug"}})
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

// TestIndexHandler covers discovery: the three URLs a newcomer tries first —
// the bare host, /api, /api/v1 — were all dead ends, answering a correct 404 to
// the only question someone can ask before they know anything about the API.
func TestIndexHandler(t *testing.T) {
	h := IndexHandler("1.2.3")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("index is not valid JSON (%v): %s", err, rec.Body.String())
	}
	if doc["version"] != "1.2.3" {
		t.Errorf("version = %v, want the value passed in — the index and the OpenAPI doc must not disagree", doc["version"])
	}

	// The index exists to be actionable. A discovery document that does not
	// name the docs or list any endpoint is decoration.
	body := rec.Body.String()
	for _, want := range []string{"/docs", "/api/v1/exercises", "/api/v1/meta/muscle-groups", "openapi"} {
		if !strings.Contains(body, want) {
			t.Errorf("index does not mention %q", want)
		}
	}
}

func TestIndexHandler_IsCacheable(t *testing.T) {
	h := IndexHandler("1.0.0")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	tag := rec.Header().Get("ETag")
	if tag == "" {
		t.Fatal("index has no ETag; it is the response most likely to be re-fetched while someone explores")
	}

	// Same binary, same document: the tag must be stable, or revalidation
	// never succeeds and the cache headers are a lie.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := rec2.Header().Get("ETag"); got != tag {
		t.Errorf("ETag changed between identical requests: %q then %q", tag, got)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("If-None-Match", tag)
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, req)
	if rec3.Code != http.StatusNotModified {
		t.Errorf("If-None-Match with the current tag = %d, want 304", rec3.Code)
	}
}

// The tests below pin fixes for defects found by code review of the usability
// work itself — several of them regressions the usability changes introduced.

// TestSearchTermCountIsBounded guards a cost amplifier the tokenized search
// created. Each word costs a bind parameter and a correlated subquery over
// every published row, so an uncapped q let a stranger choose how expensive a
// request was: enough words exceeded Postgres' 65,535-parameter limit outright
// (a 500 for malformed input), and merely thousands meant seconds of database
// CPU on the most permissive rate-limit tier.
func TestSearchTermCountIsBounded(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	seedExercise(t, ctx, pool, exerciseFixture{
		slug: "bounded-press", name: "Bounded Press",
		muscleGroup: "chest", equipment: "barbell", category: "strength",
	})

	// Far more terms than the cap. A direct handler call bypasses huma's
	// maxLength tag entirely, which is exactly the path this bound exists for.
	many := strings.Repeat("bounded ", 5000)
	if _, err := ListExercises(ctx, s, "", &ListExercisesInput{Q: many}); err != nil {
		t.Fatalf("a very long q must be bounded, not fail: %v", err)
	}
}

func TestSearchTerms_Truncates(t *testing.T) {
	if got := len(store.StoreSearchTerms(strings.Repeat("a ", 1000))); got > 16 {
		t.Errorf("searchTerms returned %d terms, want at most 16", got)
	}
	// Ordinary queries are untouched.
	if got := store.StoreSearchTerms("bench press"); len(got) != 2 {
		t.Errorf("searchTerms(\"bench press\") = %v, want 2 terms", got)
	}
}

// TestETagMatches covers the conditional-GET rules IndexHandler previously got
// wrong by comparing strings directly: every legal spelling a real revalidator
// sends would have produced a 200 instead of a 304.
func TestETagMatches(t *testing.T) {
	const tag = `"abc123"`

	cases := []struct {
		header []string
		want   bool
		why    string
	}{
		{[]string{`"abc123"`}, true, "exact"},
		{[]string{`W/"abc123"`}, true, "weak comparison"},
		{[]string{`"other", "abc123"`}, true, "list, second entry"},
		{[]string{`"other","abc123"`}, true, "list without whitespace"},
		{[]string{`"other",  W/"abc123"`}, true, "list with OWS and a weak tag"},
		{[]string{"*"}, true, "wildcard"},
		{[]string{`"other"`, `"abc123"`}, true, "header repeated rather than joined"},
		{[]string{`"nomatch"`}, false, "different tag"},
		{[]string{""}, false, "empty"},
		{nil, false, "absent"},
	}

	for _, c := range cases {
		if got := etagMatches(c.header, tag); got != c.want {
			t.Errorf("etagMatches(%q, %s) = %v, want %v (%s)", c.header, tag, got, c.want, c.why)
		}
	}
}

func TestIsZeroQValue(t *testing.T) {
	// "gzip;q=0" was the only spelling treated as a refusal, but RFC 9110
	// §12.4.2 allows up to three fractional digits — so a client saying
	// q=0.0 was sent an encoding it had explicitly declined.
	cases := map[string]bool{
		"q=0":     true,
		"q=0.0":   true,
		"q=0.00":  true,
		"q=0.000": true,
		" q=0 ":   true,
		"Q=0":     true,
		"q=0.5":   false,
		"q=1":     false,
		"q=1.0":   false,
		"":        false,
		"q=":      false,
		"q=abc":   false,
		"level=1": false,
	}
	for params, want := range cases {
		if got := isZeroQValue(params); got != want {
			t.Errorf("isZeroQValue(%q) = %v, want %v", params, got, want)
		}
	}
}

func TestCompress_WeakensETag(t *testing.T) {
	// The gzipped and identity bodies differ byte-for-byte, so RFC 9110 §8.8.3
	// forbids them sharing a strong validator. Vary already makes a conforming
	// cache key them apart; this is the guard against one that keys on ETag
	// alone and hands a gzipped body to a client that asked for identity.
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"abc123"`)
		_, _ = w.Write([]byte(`{"v":"` + strings.Repeat("a", 40_000) + `"}`))
	})

	rec := httptest.NewRecorder()
	Compress(h).ServeHTTP(rec, gzipRequest())
	if got := rec.Header().Get("ETag"); got != `W/"abc123"` {
		t.Errorf("compressed ETag = %q, want a weak tag", got)
	}

	// Identity keeps the strong tag: nothing about that representation changed.
	plain := httptest.NewRecorder()
	Compress(h).ServeHTTP(plain, httptest.NewRequest(http.MethodGet, "/api/v1/exercises", nil))
	if got := plain.Header().Get("ETag"); got != `"abc123"` {
		t.Errorf("identity ETag = %q, want the original strong tag", got)
	}
}

func TestIsCompressible_CoversOpenAPI(t *testing.T) {
	// The spec is the largest text response after /export and the first thing
	// every generated client fetches; huma serves it as application/openapi+json.
	for _, ct := range []string{
		"application/openapi+json", "application/openapi+yaml",
		"application/json", "application/problem+json",
		"application/openapi+json; charset=utf-8",
	} {
		if !isCompressible(ct) {
			t.Errorf("isCompressible(%q) = false, want true", ct)
		}
	}
	for _, ct := range []string{"image/jpeg", "image/png", "application/octet-stream"} {
		if isCompressible(ct) {
			t.Errorf("isCompressible(%q) = true, want false", ct)
		}
	}
}

// A repeated filter parameter used to be silently reduced to its first value:
// ?muscle_group=chest&muscle_group=lats answered 200 with only the chest rows.
// That is worse than an error — the caller gets a plausible answer and no signal
// that half their filter was discarded.
func TestListExercises_RepeatedFilterValuesAreAllApplied(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	for _, f := range []struct{ slug, name, muscle string }{
		{"bench-press", "Bench Press", "chest"},
		{"pull-up", "Pull-Up", "lats"},
		{"squat", "Squat", "quadriceps"},
	} {
		seedExercise(t, ctx, pool, exerciseFixture{
			slug: f.slug, name: f.name, muscleGroup: f.muscle,
			equipment: "barbell", category: "strength",
		})
	}

	out, err := ListExercises(ctx, s, "", &ListExercisesInput{
		MuscleGroup: []string{"chest", "lats"},
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if out.Body.Total != 2 {
		t.Errorf("chest+lats returned %d, want 2 — the second value was dropped", out.Body.Total)
	}
}

// Every value is validated, not just the first: a typo in the second position
// must be named rather than passing because position one happened to be real.
func TestListExercises_RejectsUnknownValueInAnyPosition(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	// Validation fails open on a facet with no terms, so the vocabulary has to
	// exist for this to be testing anything.
	seedVocabulary(t, ctx, pool)

	_, err := ListExercises(ctx, s, "", &ListExercisesInput{
		MuscleGroup: []string{"chest", "banana"},
	})
	if err == nil {
		t.Fatal("want a 422 naming the unknown value")
	}
	if !strings.Contains(err.Error(), "banana") && !strings.Contains(fmt.Sprint(err), "validation") {
		t.Errorf("error did not name the bad value: %v", err)
	}
}

// A malformed UUID in a batch must be a 422 naming the offender, not a 500 for
// the whole request — e.id is a uuid column, so one bad value fails the query.
func TestListExercises_RejectsMalformedIDs(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	_, err := ListExercises(ctx, s, "", &ListExercisesInput{
		IDs: []string{"3490fcdf-080a-4731-8f29-a59e0adbb85a", "not-a-uuid"},
	})
	if err == nil {
		t.Fatal("want a 422 for the malformed id")
	}
	var se huma.StatusError
	if errors.As(err, &se) && se.GetStatus() != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 (a bad id must not surface as a server error)", se.GetStatus())
	}
}

// The batch is capped so a caller cannot ask for more rows than a page can
// return and have the answer quietly truncated by pagination.
func TestListExercises_RejectsOversizedIDBatch(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	ids := make([]string, maxIDBatch+1)
	for i := range ids {
		ids[i] = "3490fcdf-080a-4731-8f29-a59e0adbb85a"
	}
	if _, err := ListExercises(ctx, s, "", &ListExercisesInput{IDs: ids}); err == nil {
		t.Fatalf("want a 422 for more than %d ids", maxIDBatch)
	}
}
