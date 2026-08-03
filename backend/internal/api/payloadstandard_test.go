package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/Cawlumm/open-exercise-db/internal/export"
)

// These tests pin the API payload standard documented in docs/CONVENTIONS.md.
// They are deliberately about *shape* rather than behavior: the point is that
// a consumer can learn the envelope once and apply it everywhere, which is a
// property no single endpoint's own tests can establish.

// paginationOf pulls the shared pagination block out of a marshaled body.
func paginationOf(t *testing.T, v any) map[string]int {
	t.Helper()

	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	got := map[string]int{}
	for _, field := range []string{"total", "page", "per_page"} {
		v, ok := generic[field]
		if !ok {
			t.Fatalf("collection body is missing pagination field %q: %s", field, raw)
		}
		var n int
		if err := json.Unmarshal(v, &n); err != nil {
			t.Fatalf("pagination field %q is not a number: %s", field, v)
		}
		got[field] = n
	}
	return got
}

// TestEveryCollectionCarriesPagination is the whole standard in one test: if
// a new collection endpoint ships without the shared envelope, this is what
// catches it. Each case calls the real handler, so a body that merely
// declares the fields without populating them fails too.
func TestEveryCollectionCarriesPagination(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	id, _ := seedExercise(t, ctx, pool, exerciseFixture{
		slug: "paginated-lift", name: "Paginated Lift",
		muscleGroup: "chest", equipment: "barbell", category: "strength",
	})

	cases := map[string]func() any{
		"GET /exercises": func() any {
			out, err := ListExercises(ctx, s, "", &ListExercisesInput{})
			if err != nil {
				t.Fatalf("ListExercises: %v", err)
			}
			return out.Body
		},
		"GET /exercises/{id}/revisions": func() any {
			out, err := ListExerciseRevisions(ctx, s, &ListExerciseRevisionsInput{ID: id})
			if err != nil {
				t.Fatalf("ListExerciseRevisions: %v", err)
			}
			return out.Body
		},
		"GET /exercises/{id}/translations": func() any {
			out, err := ListExerciseTranslations(ctx, s, &ListExerciseTranslationsInput{ID: id})
			if err != nil {
				t.Fatalf("ListExerciseTranslations: %v", err)
			}
			return out.Body
		},
		"GET /meta/muscle-groups": func() any {
			out, err := ListTaxonomyTerms(ctx, s, "muscle_group", &ListTaxonomyTermsInput{Locale: "en"})
			if err != nil {
				t.Fatalf("ListTaxonomyTerms: %v", err)
			}
			return out.Body
		},
		"GET /meta/locales": func() any {
			out, err := ListLocales(ctx, s, &ListLocalesInput{})
			if err != nil {
				t.Fatalf("ListLocales: %v", err)
			}
			return out.Body
		},
	}

	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			page := paginationOf(t, build())
			// A handler called directly gets no huma tag defaults, so these
			// values come from PageParams.Normalize — which is exactly the
			// path that must agree with what huma produces for a real
			// request with no query string.
			if page["page"] != 1 {
				t.Errorf("page = %d, want 1", page["page"])
			}
			if page["per_page"] != defaultPerPage {
				t.Errorf("per_page = %d, want %d", page["per_page"], defaultPerPage)
			}
			if page["total"] < 0 {
				t.Errorf("total = %d, want >= 0", page["total"])
			}
		})
	}
}

// TestExportIsTheOnlyUnpaginatedCollection names the single documented
// exception to the rule above.
//
// GET /exercises/export is a bulk dump: its whole purpose is to return the
// complete dataset in one response, so paginating it would defeat the
// endpoint. That makes it the one collection without page/per_page — and
// without this test, "the rule says every collection, and this one doesn't"
// is indistinguishable from an oversight. Asserting the exception keeps it a
// decision, and fails if anyone later adds pagination without updating
// docs/CONVENTIONS.md section 3a.
func TestExportIsTheOnlyUnpaginatedCollection(t *testing.T) {
	raw, err := json.Marshal(export.Body{})
	if err != nil {
		t.Fatalf("marshal export body: %v", err)
	}

	var generic map[string]json.RawMessage
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// total is still required: a consumer must be able to tell a truncated
	// download from a complete one.
	if _, ok := generic["total"]; !ok {
		t.Errorf("export body is missing \"total\": %s", raw)
	}
	for _, field := range []string{"page", "per_page"} {
		if _, ok := generic[field]; ok {
			t.Errorf("export body carries %q — if this endpoint is now paginated, "+
				"update docs/CONVENTIONS.md section 3a, which documents it as the sole exception", field)
		}
	}
}

// TestPagination_PagesAreDisjoint guards the failure that makes offset
// pagination silently wrong rather than visibly broken: a non-deterministic
// ORDER BY letting a row appear on two pages, or on neither.
func TestPagination_PagesAreDisjoint(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	// Seeds its own terms rather than relying on whatever the fixture
	// happens to contain. An earlier revision of this test skipped when the
	// fixture was too small, which meant the assertion below — the one that
	// catches a non-deterministic ORDER BY duplicating or dropping a row
	// across a page boundary — never actually ran.
	const perPage = 3
	for _, slug := range []string{"aaa", "bbb", "ccc", "ddd", "eee", "fff", "ggg"} {
		seedTaxonomyTerm(t, ctx, pool, "muscle_group", slug, map[string]string{"en": slug})
	}

	page1, err := ListTaxonomyTerms(ctx, s, "muscle_group",
		&ListTaxonomyTermsInput{Locale: "en", PageParams: PageParams{Page: 1, PerPage: perPage}})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	page2, err := ListTaxonomyTerms(ctx, s, "muscle_group",
		&ListTaxonomyTermsInput{Locale: "en", PageParams: PageParams{Page: 2, PerPage: perPage}})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(page1.Body.Terms) != perPage || len(page2.Body.Terms) != perPage {
		t.Fatalf("pages are %d and %d items, want %d each — seeding did not take effect",
			len(page1.Body.Terms), len(page2.Body.Terms), perPage)
	}

	seen := map[string]bool{}
	for _, term := range page1.Body.Terms {
		seen[term.Slug] = true
	}
	for _, term := range page2.Body.Terms {
		if seen[term.Slug] {
			t.Errorf("slug %q appears on both page 1 and page 2", term.Slug)
		}
	}
	if page1.Body.Total != page2.Body.Total {
		t.Errorf("total differs across pages: %d vs %d", page1.Body.Total, page2.Body.Total)
	}
}

// TestPageParams_NormalizeClamps covers the direct-call path huma's tag
// validation never sees. Without clamping, a zero PerPage would issue
// LIMIT 0 and a negative Page a negative OFFSET.
func TestPageParams_NormalizeClamps(t *testing.T) {
	cases := []struct {
		in              PageParams
		wantPage        int
		wantPer         int
		wantOffset      int
		nameForFailures string
	}{
		{PageParams{}, 1, defaultPerPage, 0, "zero value"},
		{PageParams{Page: -5, PerPage: -1}, 1, defaultPerPage, 0, "negatives"},
		{PageParams{Page: 3, PerPage: 10}, 3, 10, 20, "normal"},
		{PageParams{Page: 1, PerPage: 5000}, 1, maxPerPage, 0, "over cap"},
		{PageParams{Page: maxPage + 1, PerPage: 10}, maxPage, 10, (maxPage - 1) * 10, "page over cap"},
		// The case that produced a negative SQL OFFSET and a 500: page was
		// clamped at the low end only, so (page-1)*perPage wrapped int64.
		{PageParams{Page: math.MaxInt64, PerPage: maxPerPage}, maxPage, maxPerPage, (maxPage - 1) * maxPerPage, "max int64 page"},
		{PageParams{Page: math.MaxInt64, PerPage: 50}, maxPage, 50, (maxPage - 1) * 50, "max int64 page, default per page"},
	}
	for _, c := range cases {
		t.Run(c.nameForFailures, func(t *testing.T) {
			page, per := c.in.Normalize()
			if page != c.wantPage || per != c.wantPer {
				t.Errorf("Normalize() = (%d, %d), want (%d, %d)", page, per, c.wantPage, c.wantPer)
			}
			if got := c.in.Offset(); got != c.wantOffset {
				t.Errorf("Offset() = %d, want %d", got, c.wantOffset)
			}
		})
	}
}

// TestEveryCollectionClampsPerPage pins that the cap is enforced on the query
// itself, not merely echoed in the response. ListExercises was the one handler
// that passed its raw input straight into the store filter while reporting the
// clamped value back, so a direct call — the invocation path
// docs/CONVENTIONS.md section 3/6 mandates for tests, which never sees huma's
// tag validation — issued an effectively unbounded LIMIT and then claimed
// per_page was 100. Written as a loop over every collection so a future
// endpoint cannot quietly opt out the same way.
func TestEveryCollectionClampsPerPage(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	id, _ := seedExercise(t, ctx, pool, exerciseFixture{
		slug: "clamped-lift", name: "Clamped Lift",
		muscleGroup: "chest", equipment: "barbell", category: "strength",
	})

	// Far above maxPerPage, and above any plausible row count, so "returned
	// fewer than requested" cannot be mistaken for "the cap worked".
	over := PageParams{Page: 1, PerPage: 5000}

	cases := map[string]func() (int, any){
		"GET /exercises": func() (int, any) {
			out, err := ListExercises(ctx, s, "", &ListExercisesInput{PageParams: over})
			if err != nil {
				t.Fatalf("ListExercises: %v", err)
			}
			return len(out.Body.Exercises), out.Body
		},
		"GET /exercises/{id}/revisions": func() (int, any) {
			out, err := ListExerciseRevisions(ctx, s, &ListExerciseRevisionsInput{ID: id, PageParams: over})
			if err != nil {
				t.Fatalf("ListExerciseRevisions: %v", err)
			}
			return len(out.Body.Revisions), out.Body
		},
		"GET /exercises/{id}/translations": func() (int, any) {
			out, err := ListExerciseTranslations(ctx, s, &ListExerciseTranslationsInput{ID: id, PageParams: over})
			if err != nil {
				t.Fatalf("ListExerciseTranslations: %v", err)
			}
			return len(out.Body.Translations), out.Body
		},
		"GET /meta/muscle-groups": func() (int, any) {
			out, err := ListTaxonomyTerms(ctx, s, "muscle_group", &ListTaxonomyTermsInput{Locale: "en", PageParams: over})
			if err != nil {
				t.Fatalf("ListTaxonomyTerms: %v", err)
			}
			return len(out.Body.Terms), out.Body
		},
		"GET /meta/locales": func() (int, any) {
			out, err := ListLocales(ctx, s, &ListLocalesInput{PageParams: over})
			if err != nil {
				t.Fatalf("ListLocales: %v", err)
			}
			return len(out.Body.Locales), out.Body
		},
	}

	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			n, body := call()
			if n > maxPerPage {
				t.Errorf("returned %d items for per_page=5000, want at most %d — the cap is not reaching the query", n, maxPerPage)
			}
			if got := paginationOf(t, body)["per_page"]; got != maxPerPage {
				t.Errorf("per_page = %d, want %d", got, maxPerPage)
			}
		})
	}
}

// TestPageParams_OffsetIsNeverNegative is the property the clamping exists to
// guarantee, asserted directly rather than through the table above: a negative
// OFFSET is rejected by Postgres, which mapStoreError turns into a 500, so any
// page value that produces one is a way for an unauthenticated caller to fault
// four endpoints with a single query parameter.
func TestPageParams_OffsetIsNeverNegative(t *testing.T) {
	pages := []int{
		math.MaxInt64, math.MaxInt64 - 1, math.MaxInt64 / 2,
		1 << 62, 1 << 40, 1_000_000_000_000_000_000,
		maxPage, maxPage + 1, 1, 0, -1, math.MinInt64,
	}
	perPages := []int{-1, 0, 1, 50, maxPerPage, maxPerPage + 1, math.MaxInt64}

	for _, page := range pages {
		for _, perPage := range perPages {
			p := PageParams{Page: page, PerPage: perPage}
			if got := p.Offset(); got < 0 {
				t.Errorf("PageParams{Page: %d, PerPage: %d}.Offset() = %d, want >= 0", page, perPage, got)
			}
		}
	}
}

// TestErrorsCarryProblemType pins the machine-readable identifier consumers
// are documented to branch on. Constructed through huma's own constructors,
// which is the path every handler and huma's internal validation both take.
func TestErrorsCarryProblemType(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"404", huma.Error404NotFound("exercise not found"), ErrTypeNotFound},
		{"422", huma.Error422UnprocessableEntity("validation failed"), ErrTypeValidation},
		{"429", huma.Error429TooManyRequests("slow down"), ErrTypeRateLimited},
		{"500", huma.Error500InternalServerError("internal error"), ErrTypeInternal},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			model, ok := c.err.(*huma.ErrorModel)
			if !ok {
				t.Fatalf("got %T, want *huma.ErrorModel", c.err)
			}
			if model.Type != c.want {
				t.Errorf("Type = %q, want %q", model.Type, c.want)
			}
			// Detail is for humans and may be reworded freely; Type is the
			// contract. Assert they are distinct so nobody "simplifies" by
			// making the type mirror the prose.
			if model.Type == model.Detail {
				t.Errorf("Type and Detail are identical (%q); Type must be a stable URI", model.Type)
			}
		})
	}
}

// problemErrorsMux builds the smallest thing that reproduces production's
// routing shape: a ServeMux carrying one method-scoped route, wrapped exactly
// as cmd/api-server/main.go wraps the real one.
func problemErrorsMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/exercises", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"exercises":[],"total":0,"page":1,"per_page":50}`))
	})
	return ProblemErrors(mux)
}

// TestProblemErrors_RoutingErrorsAreProblemJSON covers both routing-level
// errors together because they are one contract: every error this API emits,
// including the ones the router raises before huma is involved, arrives in the
// documented shape.
func TestProblemErrors_RoutingErrorsAreProblemJSON(t *testing.T) {
	cases := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantType   string
	}{
		{"unknown path", http.MethodGet, "/api/v1/definitely-not-a-route", http.StatusNotFound, ErrTypeNotFound},
		{"wrong method on a real path", http.MethodPost, "/api/v1/exercises", http.StatusMethodNotAllowed, ErrTypeMethodNotAllowed},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			problemErrorsMux().ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))

			if rec.Code != c.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, c.wantStatus)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Errorf("Content-Type = %q, want application/problem+json", ct)
			}

			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not JSON (%v): %s", err, rec.Body.String())
			}
			if body["type"] != c.wantType {
				t.Errorf("type = %v, want %v", body["type"], c.wantType)
			}
			if !strings.Contains(rec.Body.String(), "/docs") {
				t.Errorf("body should point at /docs for the route list: %s", rec.Body.String())
			}
		})
	}
}

// TestProblemErrors_PreservesAllowHeader is the regression this whole wrapper
// exists for. A catch-all mounted on "/" makes ServeMux's 405 branch
// unreachable, so a wrong-verb request silently becomes a 404 with no Allow
// header — the API telling an integrator the resource does not exist when only
// their method was wrong. Asserting on Allow rather than just the status is
// what pins it: Allow can only come from ServeMux's own 405 path.
func TestProblemErrors_PreservesAllowHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	problemErrorsMux().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/exercises", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405 — a catch-all route would make this 404", rec.Code)
	}
	allow := rec.Header().Get("Allow")
	if !strings.Contains(allow, http.MethodGet) {
		t.Errorf("Allow = %q, want it to list GET", allow)
	}
}

// TestProblemErrors_LeavesRealResponsesAlone guards the discriminator. The
// wrapper keys off the text/plain content type net/http's own http.Error sets,
// so a handler's own response — including a legitimate problem+json 404 from
// huma, which is already correctly shaped and carries an operation-specific
// detail — must pass through byte-for-byte.
func TestProblemErrors_LeavesRealResponsesAlone(t *testing.T) {
	const handlerBody = `{"type":"` + ErrTypeNotFound + `","title":"Not Found","status":404,"detail":"exercise not found"}`

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/exercises/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(handlerBody))
	})

	rec := httptest.NewRecorder()
	ProblemErrors(mux).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/exercises/abc", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if got := rec.Body.String(); got != handlerBody {
		t.Errorf("handler's own 404 was rewritten\n got: %s\nwant: %s", got, handlerBody)
	}
}

// TestProblemErrors_PassesSuccessThrough exists because the wrapper sits in
// front of every response, not just errors: a bug in its Write path would
// corrupt or truncate ordinary 200s, which no error-focused test would catch.
func TestProblemErrors_PassesSuccessThrough(t *testing.T) {
	rec := httptest.NewRecorder()
	problemErrorsMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/exercises", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != `{"exercises":[],"total":0,"page":1,"per_page":50}` {
		t.Errorf("body = %s, want it passed through unchanged", got)
	}
}
