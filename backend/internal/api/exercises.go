package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/conditional"

	"github.com/Cawlumm/open-exercise-db/internal/contract"
	"github.com/Cawlumm/open-exercise-db/internal/media"
	"github.com/Cawlumm/open-exercise-db/internal/model"
	"github.com/Cawlumm/open-exercise-db/internal/ratelimit"
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// Exercise, Translation, and Image are contract.Exercise/contract.
// Translation/contract.Image — aliased here so every existing reference in
// this file (Exercise{...}, []Exercise, Translation{...}) keeps compiling
// unchanged. They used to be defined directly in this file; they moved to
// internal/contract because internal/export needs the exact same type (not a
// structurally-identical copy) to render GET /exercises/export, and two
// independently declared Go types both named "Exercise" panic huma's schema
// registry the moment both are registered on one huma.API — see
// internal/contract/doc.go for the full incident writeup.
type Exercise = contract.Exercise
type Translation = contract.Translation
type Image = contract.Image

// Revision is one entry in an exercise's append-only history. SubmittedBy
// and ReviewedBy (raw user UUIDs in model.Revision) are deliberately
// omitted: Phase 1 ships no user-facing identity endpoint to resolve them
// against, and Phase 2 (auth) is explicitly out of scope here — an opaque
// UUID with nothing to look it up against isn't a useful public field yet.
type Revision struct {
	ID               string   `json:"id"`
	ExerciseID       string   `json:"exercise_id"`
	Name             string   `json:"name"`
	Aliases          []string `json:"aliases"`
	MuscleGroup      string   `json:"muscle_group"`
	SecondaryMuscles []string `json:"secondary_muscles"`
	Category         string   `json:"category"`
	Equipment        string   `json:"equipment"`
	Mechanic         *string  `json:"mechanic,omitempty"`
	Force            *string  `json:"force,omitempty"`
	Level            *string  `json:"level,omitempty"`
	Instructions     []string `json:"instructions"`
	ImageIDs         []string `json:"image_ids"`
	VideoIDs         []string `json:"video_ids"`
	License          *string  `json:"license,omitempty"`
	AttributionText  *string  `json:"attribution_text,omitempty"`
	SourceURL        *string  `json:"source_url,omitempty"`
	ChangeSummary    *string  `json:"change_summary,omitempty"`
	Status           string   `json:"status" doc:"approved or superseded"`

	CreatedAt time.Time `json:"created_at"`
}

// TranslationSummary mirrors model.TranslationSummary — a listing view for
// GET /exercises/{id}/translations, distinct from the full field-resolved
// Translation served inline on GET /exercises and GET /exercises/{id}.
type TranslationSummary struct {
	Locale     string `json:"locale"`
	Origin     string `json:"origin" doc:"human or machine"`
	Complete   bool   `json:"complete" doc:"True if this locale's own translation supplies both a name and instructions, independent of English fallback"`
	RevisionID string `json:"revision_id"`
}

// ListExercisesInput filters and paginates GET /exercises. Every filter
// field is a taxonomy slug exact-match except Q (substring) — see
// store.ExerciseFilter, which this is passed through to unchanged.
//
// The embedded conditional.Params picks up If-Match/If-None-Match/
// If-Modified-Since/If-Unmodified-Since automatically (huma discovers and
// binds header-tagged fields on embedded structs the same as top-level
// ones, and auto-invokes its Resolve method before the handler runs) — see
// checkNotModified in api.go for how the response side uses it.
type ListExercisesInput struct {
	conditional.Params
	// maxLength is load-bearing, not cosmetic. Tokenized search emits one bind
	// parameter and one ILIKE plus a correlated unnest subquery per word, so an
	// uncapped q is a cost amplifier a stranger controls: ~70,000 words fits in
	// a URL Go accepts by default and exceeds Postgres' 65,535-parameter limit
	// outright (a 500 for malformed input), while a few thousand — comfortably
	// inside any CDN's URL cap — is seconds of database CPU per request on the
	// most permissive rate-limit tier. Before tokenizing, any q was exactly one
	// parameter and one predicate, so this ceiling is what keeps the new
	// behaviour as cheap as the old one. store.maxSearchTerms enforces the same
	// bound again for direct handler calls, which never see this tag.
	Q string `query:"q" maxLength:"200" doc:"Case-insensitive search over an exercise's name, aliases, muscles, equipment, category, mechanic, force and level. Every whitespace-separated word must match somewhere, in any order, so adding a word narrows the results — 'barbell bench' finds barbell bench presses. Rows matching by name rank above rows matching only by taxonomy. Composes with the filters below rather than replacing them. Maximum 200 characters."`
	// Every response carries a slug, so a consumer that stored one had no way
	// to look it back up: the path takes a UUID and there was no slug filter,
	// leaving only a substring search that could match several rows or none.
	Slug        string `query:"slug" doc:"Exact match on an exercise's slug — the stable human-readable identifier returned in every response"`
	MuscleGroup string `query:"muscle_group" doc:"Taxonomy slug filter; see GET /meta/muscle-groups"`
	Equipment   string `query:"equipment" doc:"Taxonomy slug filter; see GET /meta/equipment"`
	Category    string `query:"category" doc:"Taxonomy slug filter; see GET /meta/categories"`
	Mechanic    string `query:"mechanic" doc:"Taxonomy slug filter: compound or isolation"`
	// "expert", not "advanced" — the seeded vocabulary comes from
	// free-exercise-db, which uses expert as its top level. This doc said
	// "advanced" and no such term has ever existed, so /docs and the generated
	// OpenAPI spec were both telling callers to send a value guaranteed to
	// match nothing (and, since filter values are now validated, to be
	// rejected outright).
	Level  string `query:"level" doc:"Taxonomy slug filter: beginner, intermediate, or expert"`
	Locale string `query:"locale" doc:"BCP-47 locale for name/aliases/instructions; falls back to English per-field when untranslated"`
	PageParams
}

// ListExercisesOutput envelopes the page alongside the shared Pagination
// block (see pagination.go) so a client can compute page count without a
// second request, and so every collection in this API reports its position
// the same way.
type ListExercisesOutput struct {
	CacheControl string `header:"Cache-Control"`
	ETag         string `header:"ETag"`
	Body         struct {
		Exercises []Exercise `json:"exercises"`
		Pagination
	}
}

type GetExerciseInput struct {
	conditional.Params
	ID     string `path:"id" format:"uuid" doc:"Exercise UUID"`
	Locale string `query:"locale" doc:"BCP-47 locale for name/aliases/instructions; falls back to English per-field when untranslated"`
}

type GetExerciseOutput struct {
	CacheControl string `header:"Cache-Control"`
	ETag         string `header:"ETag"`
	Body         Exercise
}

type ListExerciseRevisionsInput struct {
	conditional.Params
	ID string `path:"id" format:"uuid" doc:"Exercise UUID"`
	PageParams
}

type ListExerciseRevisionsOutput struct {
	CacheControl string `header:"Cache-Control"`
	ETag         string `header:"ETag"`
	Body         struct {
		Revisions []Revision `json:"revisions" doc:"Newest first; includes superseded revisions, since history is append-only"`
		Pagination
	}
}

type ListExerciseTranslationsInput struct {
	conditional.Params
	ID string `path:"id" format:"uuid" doc:"Exercise UUID"`
	PageParams
}

type ListExerciseTranslationsOutput struct {
	CacheControl string `header:"Cache-Control"`
	ETag         string `header:"ETag"`
	Body         struct {
		// Named "translations", not "locales", even though each entry is
		// keyed by locale: GET /meta/locales already returns a "locales"
		// array whose items are a completely different type (Locale, just a
		// code), and one field name meaning two shapes across two endpoints
		// is the kind of thing a generated client or a human reading the
		// spec gets wrong exactly once, expensively.
		Translations []TranslationSummary `json:"translations" doc:"One entry per locale with an approved translation of the exercise's current revision"`
		Pagination
	}
}

// registerExerciseRoutes wires every /exercises operation. Handlers are
// closures over s rather than methods on a receiver holding s, matching
// docs/CONVENTIONS.md section 3's "plain functions, trivially unit-testable"
// guidance — each closure body is itself the trivially-testable unit; tests
// call it directly with an input struct, no HTTP round trip.
// registerExerciseRoutes wires every /exercises operation. imageBaseURL is
// internal/config.Config.ImageBaseURL, threaded down from Register (api.go)
// so ListExercises/GetExercise can resolve contract.Exercise.Images[].url —
// see internal/media.ImageURL for what it controls.
func registerExerciseRoutes(hapi huma.API, s *store.Store, imageBaseURL string) {
	huma.Register(hapi, huma.Operation{
		OperationID: "list-exercises",
		Method:      http.MethodGet,
		Path:        "/api/v1/exercises",
		Summary:     "List and filter exercises",
		Tags:        []string{"exercises"},
		// TierSearch, not TierRead: this is the endpoint a debounced
		// type-ahead client hits repeatedly (docs/PHASE1_BUILD_PLAN.md
		// WO-7), and it's a filtered/paginated query rather than a single
		// cached row.
		Metadata: map[string]any{rateLimitTierKey: ratelimit.TierSearch},
		Errors:   []int{http.StatusTooManyRequests},
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
	}, func(ctx context.Context, input *ListExercisesInput) (*ListExercisesOutput, error) {
		return ListExercises(ctx, s, imageBaseURL, input)
	})

	huma.Register(hapi, huma.Operation{
		OperationID: "get-exercise",
		Method:      http.MethodGet,
		Path:        "/api/v1/exercises/{id}",
		Summary:     "Get a single exercise",
		Tags:        []string{"exercises"},
		Metadata:    map[string]any{rateLimitTierKey: ratelimit.TierRead},
		Errors:      []int{http.StatusNotFound, http.StatusTooManyRequests},
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
	}, func(ctx context.Context, input *GetExerciseInput) (*GetExerciseOutput, error) {
		return GetExercise(ctx, s, imageBaseURL, input)
	})

	huma.Register(hapi, huma.Operation{
		OperationID: "list-exercise-revisions",
		Method:      http.MethodGet,
		Path:        "/api/v1/exercises/{id}/revisions",
		Summary:     "List an exercise's revision history",
		Tags:        []string{"exercises"},
		Metadata:    map[string]any{rateLimitTierKey: ratelimit.TierRead},
		Errors:      []int{http.StatusNotFound, http.StatusTooManyRequests},
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
	}, func(ctx context.Context, input *ListExerciseRevisionsInput) (*ListExerciseRevisionsOutput, error) {
		return ListExerciseRevisions(ctx, s, input)
	})

	huma.Register(hapi, huma.Operation{
		OperationID: "list-exercise-translations",
		Method:      http.MethodGet,
		Path:        "/api/v1/exercises/{id}/translations",
		Summary:     "List an exercise's available translations",
		Tags:        []string{"exercises"},
		Metadata:    map[string]any{rateLimitTierKey: ratelimit.TierRead},
		Errors:      []int{http.StatusNotFound, http.StatusTooManyRequests},
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
	}, func(ctx context.Context, input *ListExerciseTranslationsInput) (*ListExerciseTranslationsOutput, error) {
		return ListExerciseTranslations(ctx, s, input)
	})
}

// ListExercises is registered as the list-exercises operation. Exported (and
// the sibling handlers below) so handler tests can call it directly with an
// input struct, per docs/CONVENTIONS.md section 3/6 — no HTTP round trip
// needed for logic assertions.
func ListExercises(ctx context.Context, s *store.Store, imageBaseURL string, input *ListExercisesInput) (*ListExercisesOutput, error) {
	// Normalized rather than passed through raw. This was the one collection
	// handler still handing the store its unclamped input while reporting the
	// clamped values back through paginationFor — so a direct call (the
	// invocation path docs/CONVENTIONS.md section 3/6 mandates for tests, which
	// bypasses huma's tag validation entirely) with PerPage: 5000 ran LIMIT
	// 5000 and answered "per_page": 100, and the store's own floor at
	// exercises.go's ExerciseFilter defers the ceiling back to the huma tag
	// that a direct call never applies. Every sibling handler already routed
	// through Normalize; this one silently opted out during the refactor.
	if err := validateTaxonomyFilters(ctx, s, input); err != nil {
		return nil, err
	}

	page, perPage := input.Normalize()
	filter := store.ExerciseFilter{
		Q:           input.Q,
		Slug:        input.Slug,
		MuscleGroup: input.MuscleGroup,
		Equipment:   input.Equipment,
		Category:    input.Category,
		Mechanic:    input.Mechanic,
		Level:       input.Level,
		Page:        page,
		PerPage:     perPage,
	}

	exercises, total, err := s.Exercises.List(ctx, filter)
	if err != nil {
		return nil, mapStoreError(err, "")
	}

	out := &ListExercisesOutput{}
	out.Body.Pagination = paginationFor(input.PageParams, total)
	// Pre-allocated rather than appended onto a nil slice so a zero-result
	// response serializes as `"exercises": []`, not `"exercises": null`.
	// encoding/json renders a nil slice as null, and null here breaks the
	// consumption model docs/DESIGN.md recommends as primary: a type-ahead
	// search sends a request per debounce interval, so the *first* query
	// matching nothing hands every client a null where it expects a list.
	// Same reasoning applies to every list-shaped response body in this
	// package.
	out.Body.Exercises = make([]contract.Exercise, 0, len(exercises))
	for _, ex := range exercises {
		resolved, err := resolveExercise(ctx, s, ex, normalizeLocale(input.Locale), imageBaseURL)
		if err != nil {
			return nil, mapStoreError(err, "exercise not found")
		}
		out.Body.Exercises = append(out.Body.Exercises, resolved)
	}

	tag := etagValue(out.Body)
	if notModified := checkNotModified(&input.Params, tag, cacheControl); notModified != nil {
		return nil, notModified
	}
	out.CacheControl = cacheControl
	out.ETag = etagHeader(tag)
	return out, nil
}

func GetExercise(ctx context.Context, s *store.Store, imageBaseURL string, input *GetExerciseInput) (*GetExerciseOutput, error) {
	ex, err := s.Exercises.Get(ctx, input.ID)
	if err != nil {
		return nil, mapStoreError(err, "exercise not found")
	}

	resolved, err := resolveExercise(ctx, s, ex, normalizeLocale(input.Locale), imageBaseURL)
	if err != nil {
		return nil, mapStoreError(err, "exercise not found")
	}

	tag := etagValue(resolved)
	if notModified := checkNotModified(&input.Params, tag, cacheControl); notModified != nil {
		return nil, notModified
	}
	out := &GetExerciseOutput{CacheControl: cacheControl, ETag: etagHeader(tag), Body: resolved}
	return out, nil
}

// ListExerciseRevisions first confirms id names a published exercise (the
// same existence check GetExercise makes) before listing its history. Every
// published exercise has at least one revision by construction, so an empty
// result from store.Exercises.ListRevisions alone can't be distinguished
// from "id doesn't exist" — the existence check is what makes the 404 vs.
// empty-body distinction honest.
func ListExerciseRevisions(ctx context.Context, s *store.Store, input *ListExerciseRevisionsInput) (*ListExerciseRevisionsOutput, error) {
	if _, err := s.Exercises.Get(ctx, input.ID); err != nil {
		return nil, mapStoreError(err, "exercise not found")
	}

	total, err := s.Exercises.CountRevisions(ctx, input.ID)
	if err != nil {
		return nil, mapStoreError(err, "exercise not found")
	}

	_, perPage := input.Normalize()
	revisions, err := s.Exercises.ListRevisions(ctx, input.ID, perPage, input.Offset())
	if err != nil {
		return nil, mapStoreError(err, "exercise not found")
	}

	out := &ListExerciseRevisionsOutput{}
	out.Body.Pagination = paginationFor(input.PageParams, total)
	// Empty slice, not nil — see ListExercises for why.
	out.Body.Revisions = make([]Revision, 0, len(revisions))
	for _, rev := range revisions {
		out.Body.Revisions = append(out.Body.Revisions, revisionFromModel(rev))
	}

	tag := etagValue(out.Body)
	if notModified := checkNotModified(&input.Params, tag, cacheControl); notModified != nil {
		return nil, notModified
	}
	out.CacheControl = cacheControl
	out.ETag = etagHeader(tag)
	return out, nil
}

// ListExerciseTranslations exists check mirrors ListExerciseRevisions' for
// the same reason: a locale-less exercise (nothing translated yet) and a
// nonexistent one both yield an empty locales list from the store method
// alone.
func ListExerciseTranslations(ctx context.Context, s *store.Store, input *ListExerciseTranslationsInput) (*ListExerciseTranslationsOutput, error) {
	if _, err := s.Exercises.Get(ctx, input.ID); err != nil {
		return nil, mapStoreError(err, "exercise not found")
	}

	total, err := s.Translations.CountLocalesForExercise(ctx, input.ID)
	if err != nil {
		return nil, mapStoreError(err, "exercise not found")
	}

	_, perPage := input.Normalize()
	summaries, err := s.Translations.ListLocalesForExercise(ctx, input.ID, perPage, input.Offset())
	if err != nil {
		return nil, mapStoreError(err, "exercise not found")
	}

	out := &ListExerciseTranslationsOutput{}
	out.Body.Pagination = paginationFor(input.PageParams, total)
	// Empty slice, not nil — see ListExercises. This endpoint hits the case
	// most often: an exercise with no translations beyond its revision's own
	// content is the norm today, so `null` here was the shipped behavior for
	// every exercise in the dataset.
	out.Body.Translations = make([]TranslationSummary, 0, len(summaries))
	for _, sum := range summaries {
		out.Body.Translations = append(out.Body.Translations, TranslationSummary{
			Locale:     sum.Locale,
			Origin:     sum.Origin,
			Complete:   sum.Complete,
			RevisionID: sum.RevisionID,
		})
	}

	tag := etagValue(out.Body)
	if notModified := checkNotModified(&input.Params, tag, cacheControl); notModified != nil {
		return nil, notModified
	}
	out.CacheControl = cacheControl
	out.ETag = etagHeader(tag)
	return out, nil
}

// resolveExercise joins an exercise's static (never-translated) fields to
// its locale-resolved content. Called once per exercise for both List and
// Get — store.Translations.GetForRevision is a per-revision call (WO-2's
// contract), so a page of N exercises costs N additional round trips here,
// plus one more for s.Media.ListImagesByIDs below. Acceptable for Phase 1:
// the CDN cache in front (docs/DESIGN.md -> Performance) is what absorbs
// repeat-read volume, not per-request query count, and per_page is capped
// at 100.
func resolveExercise(ctx context.Context, s *store.Store, ex model.Exercise, locale, imageBaseURL string) (Exercise, error) {
	if ex.CurrentRevision == nil {
		// store.Exercises.List/Get always join to current_revision_id;
		// nil here means that join produced no row, which is a store
		// invariant violation, not a client-facing condition.
		return Exercise{}, fmt.Errorf("exercise %s: current revision not populated", ex.ID)
	}
	rev := ex.CurrentRevision

	resolved, err := s.Translations.GetForRevision(ctx, rev.ID, locale)
	if err != nil {
		return Exercise{}, fmt.Errorf("resolve translation for exercise %s: %w", ex.ID, err)
	}

	images, err := s.Media.ListImagesByIDs(ctx, rev.ImageIDs)
	if err != nil {
		return Exercise{}, fmt.Errorf("resolve images for exercise %s: %w", ex.ID, err)
	}

	return Exercise{
		ID:               ex.ID,
		Slug:             ex.Slug,
		SourceID:         ex.SourceID,
		Status:           ex.Status,
		CreatedAt:        ex.CreatedAt,
		UpdatedAt:        ex.UpdatedAt,
		RevisionID:       rev.ID,
		MuscleGroup:      rev.MuscleGroup,
		SecondaryMuscles: rev.SecondaryMuscles,
		Category:         rev.Category,
		Equipment:        rev.Equipment,
		Mechanic:         rev.Mechanic,
		Force:            rev.Force,
		Level:            rev.Level,
		Images:           media.BuildImages(images, imageBaseURL),
		VideoIDs:         rev.VideoIDs,
		License:          rev.License,
		AttributionText:  rev.AttributionText,
		SourceURL:        rev.SourceURL,
		Translation:      translationFromResolved(resolved),
	}, nil
}

func translationFromResolved(rt model.ResolvedTranslation) Translation {
	return Translation{
		RequestedLocale:           rt.RequestedLocale,
		Name:                      rt.Name,
		NameLocale:                rt.NameLocale,
		NameMachineOrigin:         rt.NameMachineOrigin,
		Aliases:                   rt.Aliases,
		AliasesLocale:             rt.AliasesLocale,
		AliasesMachineOrigin:      rt.AliasesMachineOrigin,
		Instructions:              rt.Instructions,
		InstructionsLocale:        rt.InstructionsLocale,
		InstructionsMachineOrigin: rt.InstructionsMachineOrigin,
	}
}

func revisionFromModel(rev model.Revision) Revision {
	return Revision{
		ID:               rev.ID,
		ExerciseID:       rev.ExerciseID,
		Name:             rev.Name,
		Aliases:          rev.Aliases,
		MuscleGroup:      rev.MuscleGroup,
		SecondaryMuscles: rev.SecondaryMuscles,
		Category:         rev.Category,
		Equipment:        rev.Equipment,
		Mechanic:         rev.Mechanic,
		Force:            rev.Force,
		Level:            rev.Level,
		Instructions:     rev.Instructions,
		ImageIDs:         rev.ImageIDs,
		VideoIDs:         rev.VideoIDs,
		License:          rev.License,
		AttributionText:  rev.AttributionText,
		SourceURL:        rev.SourceURL,
		ChangeSummary:    rev.ChangeSummary,
		Status:           rev.Status,
		CreatedAt:        rev.CreatedAt,
	}
}
