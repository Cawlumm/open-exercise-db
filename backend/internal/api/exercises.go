package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/conditional"

	"github.com/Cawlumm/open-exercise-db/internal/contract"
	"github.com/Cawlumm/open-exercise-db/internal/model"
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// Exercise and Translation are contract.Exercise/contract.Translation —
// aliased here so every existing reference in this file (Exercise{...},
// []Exercise, Translation{...}) keeps compiling unchanged. They used to be
// defined directly in this file; they moved to internal/contract because
// internal/export needs the exact same type (not a structurally-identical
// copy) to render GET /exercises/export, and two independently declared Go
// types both named "Exercise" panic huma's schema registry the moment both
// are registered on one huma.API — see internal/contract/doc.go for the
// full incident writeup.
type Exercise = contract.Exercise
type Translation = contract.Translation

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
	Q           string `query:"q" doc:"Case-insensitive substring match against exercise name and aliases"`
	MuscleGroup string `query:"muscle_group" doc:"Taxonomy slug filter; see GET /meta/muscle-groups"`
	Equipment   string `query:"equipment" doc:"Taxonomy slug filter; see GET /meta/equipment"`
	Category    string `query:"category" doc:"Taxonomy slug filter; see GET /meta/categories"`
	Mechanic    string `query:"mechanic" doc:"Taxonomy slug filter: compound or isolation"`
	Level       string `query:"level" doc:"Taxonomy slug filter: beginner, intermediate, or advanced"`
	Locale      string `query:"locale" doc:"BCP-47 locale for name/aliases/instructions; falls back to English per-field when untranslated"`
	Page        int    `query:"page" minimum:"1" default:"1"`
	PerPage     int    `query:"per_page" minimum:"1" maximum:"100" default:"50" doc:"Results per page; capped at 100 since this is a public, unauthenticated endpoint"`
}

// ListExercisesOutput envelopes the page alongside Total (the full matching
// count, ignoring pagination) so a client can compute page count without a
// second request.
type ListExercisesOutput struct {
	CacheControl string `header:"Cache-Control"`
	ETag         string `header:"ETag"`
	Body         struct {
		Exercises []Exercise `json:"exercises"`
		Total     int        `json:"total" doc:"Total matching exercises across all pages"`
		Page      int        `json:"page"`
		PerPage   int        `json:"per_page"`
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
}

type ListExerciseRevisionsOutput struct {
	CacheControl string `header:"Cache-Control"`
	ETag         string `header:"ETag"`
	Body         struct {
		Revisions []Revision `json:"revisions" doc:"Newest first; includes superseded revisions, since history is append-only"`
	}
}

type ListExerciseTranslationsInput struct {
	conditional.Params
	ID string `path:"id" format:"uuid" doc:"Exercise UUID"`
}

type ListExerciseTranslationsOutput struct {
	CacheControl string `header:"Cache-Control"`
	ETag         string `header:"ETag"`
	Body         struct {
		Locales []TranslationSummary `json:"locales" doc:"One entry per locale with an approved translation of the exercise's current revision"`
	}
}

// registerExerciseRoutes wires every /exercises operation. Handlers are
// closures over s rather than methods on a receiver holding s, matching
// docs/CONVENTIONS.md section 3's "plain functions, trivially unit-testable"
// guidance — each closure body is itself the trivially-testable unit; tests
// call it directly with an input struct, no HTTP round trip.
func registerExerciseRoutes(hapi huma.API, s *store.Store) {
	huma.Register(hapi, huma.Operation{
		OperationID: "list-exercises",
		Method:      http.MethodGet,
		Path:        "/api/v1/exercises",
		Summary:     "List and filter exercises",
		Tags:        []string{"exercises"},
	}, func(ctx context.Context, input *ListExercisesInput) (*ListExercisesOutput, error) {
		return ListExercises(ctx, s, input)
	})

	huma.Register(hapi, huma.Operation{
		OperationID: "get-exercise",
		Method:      http.MethodGet,
		Path:        "/api/v1/exercises/{id}",
		Summary:     "Get a single exercise",
		Tags:        []string{"exercises"},
		Errors:      []int{http.StatusNotFound},
	}, func(ctx context.Context, input *GetExerciseInput) (*GetExerciseOutput, error) {
		return GetExercise(ctx, s, input)
	})

	huma.Register(hapi, huma.Operation{
		OperationID: "list-exercise-revisions",
		Method:      http.MethodGet,
		Path:        "/api/v1/exercises/{id}/revisions",
		Summary:     "List an exercise's revision history",
		Tags:        []string{"exercises"},
		Errors:      []int{http.StatusNotFound},
	}, func(ctx context.Context, input *ListExerciseRevisionsInput) (*ListExerciseRevisionsOutput, error) {
		return ListExerciseRevisions(ctx, s, input)
	})

	huma.Register(hapi, huma.Operation{
		OperationID: "list-exercise-translations",
		Method:      http.MethodGet,
		Path:        "/api/v1/exercises/{id}/translations",
		Summary:     "List an exercise's available translations",
		Tags:        []string{"exercises"},
		Errors:      []int{http.StatusNotFound},
	}, func(ctx context.Context, input *ListExerciseTranslationsInput) (*ListExerciseTranslationsOutput, error) {
		return ListExerciseTranslations(ctx, s, input)
	})
}

// ListExercises is registered as the list-exercises operation. Exported (and
// the sibling handlers below) so handler tests can call it directly with an
// input struct, per docs/CONVENTIONS.md section 3/6 — no HTTP round trip
// needed for logic assertions.
func ListExercises(ctx context.Context, s *store.Store, input *ListExercisesInput) (*ListExercisesOutput, error) {
	filter := store.ExerciseFilter{
		Q:           input.Q,
		MuscleGroup: input.MuscleGroup,
		Equipment:   input.Equipment,
		Category:    input.Category,
		Mechanic:    input.Mechanic,
		Level:       input.Level,
		Page:        input.Page,
		PerPage:     input.PerPage,
	}

	exercises, total, err := s.Exercises.List(ctx, filter)
	if err != nil {
		return nil, mapStoreError(err, "")
	}

	out := newListExercisesEnvelope(input, total)
	for _, ex := range exercises {
		resolved, err := resolveExercise(ctx, s, ex, input.Locale)
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

// newListExercisesEnvelope builds the envelope shell (page/per_page echoed
// back, total set) before the caller appends resolved exercises — split out
// only to keep ListExercises' body readable. CacheControl/ETag are set
// afterward, once, by the caller via checkNotModified — not here.
func newListExercisesEnvelope(input *ListExercisesInput, total int) *ListExercisesOutput {
	out := &ListExercisesOutput{}
	page := input.Page
	if page < 1 {
		page = 1
	}
	perPage := input.PerPage
	if perPage < 1 {
		perPage = 50
	}
	out.Body.Page = page
	out.Body.PerPage = perPage
	out.Body.Total = total
	return out
}

func GetExercise(ctx context.Context, s *store.Store, input *GetExerciseInput) (*GetExerciseOutput, error) {
	ex, err := s.Exercises.Get(ctx, input.ID)
	if err != nil {
		return nil, mapStoreError(err, "exercise not found")
	}

	resolved, err := resolveExercise(ctx, s, ex, input.Locale)
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

	revisions, err := s.Exercises.ListRevisions(ctx, input.ID)
	if err != nil {
		return nil, mapStoreError(err, "exercise not found")
	}

	out := &ListExerciseRevisionsOutput{}
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

	summaries, err := s.Translations.ListLocalesForExercise(ctx, input.ID)
	if err != nil {
		return nil, mapStoreError(err, "exercise not found")
	}

	out := &ListExerciseTranslationsOutput{}
	for _, sum := range summaries {
		out.Body.Locales = append(out.Body.Locales, TranslationSummary{
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
// contract), so a page of N exercises costs N additional round trips here.
// Acceptable for Phase 1: the CDN cache in front (docs/DESIGN.md ->
// Performance) is what absorbs repeat-read volume, not per-request query
// count, and per_page is capped at 100.
func resolveExercise(ctx context.Context, s *store.Store, ex model.Exercise, locale string) (Exercise, error) {
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
		ImageIDs:         rev.ImageIDs,
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
