package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/conditional"

	"github.com/Cawlumm/open-exercise-db/internal/model"
	"github.com/Cawlumm/open-exercise-db/internal/ratelimit"
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// TaxonomyTerm mirrors model.TaxonomyTerm — a controlled-vocabulary value
// with its label resolved for the requested locale (falling back to
// English, then the slug itself, per store.TaxonomyStore.ListTerms).
type TaxonomyTerm struct {
	ID     string `json:"id"`
	Facet  string `json:"facet"`
	Slug   string `json:"slug"`
	Label  string `json:"label"`
	Locale string `json:"locale" doc:"Locale the label was actually served in; falls back to en, then to the slug itself"`
}

// Locale mirrors model.Locale for GET /meta/locales.
type Locale struct {
	Code string `json:"code" doc:"BCP-47 locale code"`
}

// See ListExercisesInput's doc comment (exercises.go) for why
// conditional.Params is embedded rather than wired by hand.
type ListTaxonomyTermsInput struct {
	conditional.Params
	Locale string `query:"locale" doc:"BCP-47 locale for the label; falls back to English, then the slug itself"`
	PageParams
}

type ListTaxonomyTermsOutput struct {
	CacheControl string `header:"Cache-Control"`
	ETag         string `header:"ETag"`
	Body         struct {
		Terms []TaxonomyTerm `json:"terms"`
		Pagination
	}
}

type ListLocalesInput struct {
	conditional.Params
	PageParams
}

type ListLocalesOutput struct {
	CacheControl string `header:"Cache-Control"`
	ETag         string `header:"ETag"`
	Body         struct {
		Locales []Locale `json:"locales" doc:"Every locale with translated content in the dataset; always includes en"`
		Pagination
	}
}

// Facet slugs match the CHECK constraint on exercise_revisions/taxonomy_terms
// (docs/DESIGN.md's ER diagram); only the four DESIGN.md names as /meta
// endpoints (muscle-groups, equipment, categories, locales) — mechanic,
// force, and level are taxonomy facets too but aren't listed as their own
// /meta routes there, so WO-3 doesn't add routes for them.
const (
	facetMuscleGroup = "muscle_group"
	facetEquipment   = "equipment"
	facetCategory    = "category"
)

func registerMetaRoutes(hapi huma.API, s *store.Store) {
	huma.Register(hapi, huma.Operation{
		OperationID: "list-muscle-groups",
		Method:      http.MethodGet,
		Path:        "/api/v1/meta/muscle-groups",
		Summary:     "List muscle group taxonomy terms",
		Tags:        []string{"meta"},
		Metadata:    map[string]any{rateLimitTierKey: ratelimit.TierRead},
		Errors:      []int{http.StatusTooManyRequests},
		// Unknown query parameters are rejected on this operation rather than
		// server-wide. The flag is per-operation precisely because the two
		// endpoints without query parameters — get-image and export-exercises —
		// must not have it: an unknown parameter there cannot be a typo of a
		// real one, and the realistic sender is a cache-buster
		// (`<img src=".../images/{id}?v=2">`), a `?utm_source=` that rode in on
		// a link, or a mirroring job stamping `?ts=`. Rejecting those breaks the
		// image or the sync and teaches the caller nothing.
		//
		// Not SkipValidateParams on those two instead: that switch disables
		// *all* parameter validation including the path UUID, which turned a
		// malformed image id from a 422 into a 500.
		RejectUnknownQueryParameters: true,
	}, func(ctx context.Context, input *ListTaxonomyTermsInput) (*ListTaxonomyTermsOutput, error) {
		return ListTaxonomyTerms(ctx, s, facetMuscleGroup, input)
	})

	huma.Register(hapi, huma.Operation{
		OperationID: "list-equipment",
		Method:      http.MethodGet,
		Path:        "/api/v1/meta/equipment",
		Summary:     "List equipment taxonomy terms",
		Tags:        []string{"meta"},
		Metadata:    map[string]any{rateLimitTierKey: ratelimit.TierRead},
		Errors:      []int{http.StatusTooManyRequests},
		// Unknown query parameters are rejected on this operation rather than
		// server-wide. The flag is per-operation precisely because the two
		// endpoints without query parameters — get-image and export-exercises —
		// must not have it: an unknown parameter there cannot be a typo of a
		// real one, and the realistic sender is a cache-buster
		// (`<img src=".../images/{id}?v=2">`), a `?utm_source=` that rode in on
		// a link, or a mirroring job stamping `?ts=`. Rejecting those breaks the
		// image or the sync and teaches the caller nothing.
		//
		// Not SkipValidateParams on those two instead: that switch disables
		// *all* parameter validation including the path UUID, which turned a
		// malformed image id from a 422 into a 500.
		RejectUnknownQueryParameters: true,
	}, func(ctx context.Context, input *ListTaxonomyTermsInput) (*ListTaxonomyTermsOutput, error) {
		return ListTaxonomyTerms(ctx, s, facetEquipment, input)
	})

	huma.Register(hapi, huma.Operation{
		OperationID: "list-categories",
		Method:      http.MethodGet,
		Path:        "/api/v1/meta/categories",
		Summary:     "List category taxonomy terms",
		Tags:        []string{"meta"},
		Metadata:    map[string]any{rateLimitTierKey: ratelimit.TierRead},
		Errors:      []int{http.StatusTooManyRequests},
		// Unknown query parameters are rejected on this operation rather than
		// server-wide. The flag is per-operation precisely because the two
		// endpoints without query parameters — get-image and export-exercises —
		// must not have it: an unknown parameter there cannot be a typo of a
		// real one, and the realistic sender is a cache-buster
		// (`<img src=".../images/{id}?v=2">`), a `?utm_source=` that rode in on
		// a link, or a mirroring job stamping `?ts=`. Rejecting those breaks the
		// image or the sync and teaches the caller nothing.
		//
		// Not SkipValidateParams on those two instead: that switch disables
		// *all* parameter validation including the path UUID, which turned a
		// malformed image id from a 422 into a 500.
		RejectUnknownQueryParameters: true,
	}, func(ctx context.Context, input *ListTaxonomyTermsInput) (*ListTaxonomyTermsOutput, error) {
		return ListTaxonomyTerms(ctx, s, facetCategory, input)
	})

	huma.Register(hapi, huma.Operation{
		OperationID: "list-locales",
		Method:      http.MethodGet,
		Path:        "/api/v1/meta/locales",
		Summary:     "List locales with translated content",
		Tags:        []string{"meta"},
		Metadata:    map[string]any{rateLimitTierKey: ratelimit.TierRead},
		Errors:      []int{http.StatusTooManyRequests},
		// Unknown query parameters are rejected on this operation rather than
		// server-wide. The flag is per-operation precisely because the two
		// endpoints without query parameters — get-image and export-exercises —
		// must not have it: an unknown parameter there cannot be a typo of a
		// real one, and the realistic sender is a cache-buster
		// (`<img src=".../images/{id}?v=2">`), a `?utm_source=` that rode in on
		// a link, or a mirroring job stamping `?ts=`. Rejecting those breaks the
		// image or the sync and teaches the caller nothing.
		//
		// Not SkipValidateParams on those two instead: that switch disables
		// *all* parameter validation including the path UUID, which turned a
		// malformed image id from a 422 into a 500.
		RejectUnknownQueryParameters: true,
	}, func(ctx context.Context, input *ListLocalesInput) (*ListLocalesOutput, error) {
		return ListLocales(ctx, s, input)
	})
}

// ListTaxonomyTerms backs list-muscle-groups/list-equipment/list-categories,
// parameterized on facet since the three routes differ only in which
// taxonomy facet they query. Exported so handler tests can call it directly
// with an input struct (docs/CONVENTIONS.md section 3/6).
func ListTaxonomyTerms(ctx context.Context, s *store.Store, facet string, input *ListTaxonomyTermsInput) (*ListTaxonomyTermsOutput, error) {
	total, err := s.Taxonomy.CountTerms(ctx, facet)
	if err != nil {
		return nil, mapStoreError(err, "")
	}

	_, perPage := input.Normalize()
	terms, err := s.Taxonomy.ListTerms(ctx, facet, normalizeLocale(input.Locale), perPage, input.Offset())
	if err != nil {
		return nil, mapStoreError(err, "")
	}

	out := &ListTaxonomyTermsOutput{}
	out.Body.Pagination = paginationFor(input.PageParams, total)
	// Empty slice, not nil — see ListExercises (exercises.go). Masked today
	// because every facet is seeded non-empty, which is exactly why it's
	// worth fixing now rather than when a new facet ships empty.
	out.Body.Terms = make([]TaxonomyTerm, 0, len(terms))
	for _, t := range terms {
		out.Body.Terms = append(out.Body.Terms, taxonomyTermFromModel(t))
	}

	tag := etagValue(out.Body)
	if notModified := checkNotModified(&input.Params, tag, cacheControl); notModified != nil {
		return nil, notModified
	}
	out.CacheControl = cacheControl
	out.ETag = etagHeader(tag)
	return out, nil
}

func ListLocales(ctx context.Context, s *store.Store, input *ListLocalesInput) (*ListLocalesOutput, error) {
	total, err := s.Taxonomy.CountLocales(ctx)
	if err != nil {
		return nil, mapStoreError(err, "")
	}

	_, perPage := input.Normalize()
	locales, err := s.Taxonomy.ListLocales(ctx, perPage, input.Offset())
	if err != nil {
		return nil, mapStoreError(err, "")
	}

	out := &ListLocalesOutput{}
	out.Body.Pagination = paginationFor(input.PageParams, total)
	// Empty slice, not nil — see ListExercises (exercises.go).
	out.Body.Locales = make([]Locale, 0, len(locales))
	for _, l := range locales {
		out.Body.Locales = append(out.Body.Locales, Locale{Code: l.Code})
	}

	tag := etagValue(out.Body)
	if notModified := checkNotModified(&input.Params, tag, cacheControl); notModified != nil {
		return nil, notModified
	}
	out.CacheControl = cacheControl
	out.ETag = etagHeader(tag)
	return out, nil
}

func taxonomyTermFromModel(t model.TaxonomyTerm) TaxonomyTerm {
	return TaxonomyTerm{
		ID:     t.ID,
		Facet:  t.Facet,
		Slug:   t.Slug,
		Label:  t.Label,
		Locale: t.Locale,
	}
}
