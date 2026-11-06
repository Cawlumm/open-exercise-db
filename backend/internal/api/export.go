package api

import (
	"context"
	"log"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/conditional"

	"github.com/Cawlumm/open-exercise-db/internal/export"
	"github.com/Cawlumm/open-exercise-db/internal/ratelimit"
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// exportCacheControl is deliberately longer than cacheControl (api.go).
// docs/DESIGN.md's "Consuming this API" section frames GET /exercises/export
// as the endpoint for mirrors doing a periodic full-catalog sync, not a
// per-request read — docs/PHASE1_BUILD_PLAN.md's WO-7 also plans this
// endpoint's rate limit as the lowest of any GET for the same reason
// ("meant to be cached/mirrored, not polled"). max-age is deliberately
// shorter than export.DefaultCacheTTL: the in-memory cache is invalidated
// the moment a moderator publishes, so a client that revalidates every 15
// minutes picks up an approval within that window, and the revalidation
// itself is answered from memory without touching the database.
const exportCacheControl = "public, max-age=900, stale-while-revalidate=86400"

// ExportExercisesInput takes no filters — the export is the full catalog,
// unconditionally. See ListExercisesInput's doc comment for why
// conditional.Params is embedded rather than wired by hand.
type ExportExercisesInput struct {
	conditional.Params
}

// ExportExercisesOutput's Body is export.Body directly (not a wrapper type
// defined here) — there is nothing for internal/api to add to the cached
// blob's shape, and defining a parallel struct would just be one more place
// for the two to drift.
type ExportExercisesOutput struct {
	CacheControl string `header:"Cache-Control"`
	ETag         string `header:"ETag"`
	Body         export.Body
}

func registerExportRoutes(hapi huma.API, s *store.Store, cache *export.Cache) {
	huma.Register(hapi, huma.Operation{
		OperationID: "export-exercises",
		Method:      http.MethodGet,
		Path:        "/api/v1/exercises/export",
		Summary:     "Full-catalog bulk export for mirroring and offline-first consumers",
		Tags:        []string{"exercises"},
		// TierExport: the lowest-throughput tier of the three
		// (docs/PHASE1_BUILD_PLAN.md WO-7) — this is the heaviest single
		// request in the API and is meant to be mirrored on a schedule,
		// not polled; see docs/API_POLICY.md.
		Metadata: map[string]any{rateLimitTierKey: ratelimit.TierExport},
		Errors:   []int{http.StatusTooManyRequests},
	}, func(ctx context.Context, input *ExportExercisesInput) (*ExportExercisesOutput, error) {
		return ExportExercises(ctx, s, cache, input)
	})
}

// ExportExercises serves cache's current blob. Exported, like every other
// handler in this package, so tests can call it directly with an input
// struct (docs/CONVENTIONS.md section 3/6).
//
// Unlike every other handler here, the ETag is not computed in this
// function — cache.Get returns one already computed at build time
// (export.Blob), per docs/PHASE1_BUILD_PLAN.md WO-5: "compute the ETag when
// the blob is built, not per request." checkNotModified itself is reused
// unchanged from WO-3, just fed that precomputed tag instead of one from
// etagValue.
func ExportExercises(ctx context.Context, s *store.Store, cache *export.Cache, input *ExportExercisesInput) (*ExportExercisesOutput, error) {
	blob, err := cache.Get(ctx, s)
	if err != nil {
		// A store error here means the DB was unreachable during a rebuild —
		// not a client-facing condition, and not pgx.ErrNoRows (ListAll
		// returns an empty slice, not an error, when there's nothing
		// published), so this always maps to a 500 rather than going through
		// mapStoreError.
		log.Printf("build export blob: %v", err)
		return nil, huma.Error500InternalServerError("internal error")
	}

	if notModified := checkNotModified(&input.Params, blob.ETag, exportCacheControl); notModified != nil {
		return nil, notModified
	}
	return &ExportExercisesOutput{
		CacheControl: exportCacheControl,
		ETag:         etagHeader(blob.ETag),
		Body:         blob.Body,
	}, nil
}
