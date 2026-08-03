package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
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

func TestNotFoundHandler_ServesProblemJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/definitely-not-a-route", nil)
	NotFoundHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON (%v): %s", err, rec.Body.String())
	}
	if body["type"] != ErrTypeNotFound {
		t.Errorf("type = %v, want %v", body["type"], ErrTypeNotFound)
	}
	if !strings.Contains(rec.Body.String(), "/docs") {
		t.Errorf("body should point at /docs for the route list: %s", rec.Body.String())
	}
}
