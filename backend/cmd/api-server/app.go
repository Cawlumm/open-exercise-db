package main

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/Cawlumm/open-exercise-db/internal/api"
	"github.com/Cawlumm/open-exercise-db/internal/export"
	"github.com/Cawlumm/open-exercise-db/internal/media"
	"github.com/Cawlumm/open-exercise-db/internal/ratelimit"
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// registerAPI mounts the discovery index and every huma operation on mux. The
// server and the -openapi dump both call it, so the spec a release publishes
// is the spec the running server would serve; there is no second registration
// to drift.
func registerAPI(mux *http.ServeMux, version string, stores *store.Store, exportCache *export.Cache, limiter *ratelimit.Limiter, mediaStore media.Store, imageBaseURL string, authCfg api.AuthConfig) huma.API {
	// Each entry URL is registered explicitly, with and without its trailing
	// slash. "/api/v1/{$}" alone matches only "/api/v1/", so "/api/v1" — the
	// form someone actually copies out of a URL — got ServeMux's implicit 307
	// and an HTML redirect stub instead of the document, and "/api" was never
	// registered at all. Verified against this Go version that all five
	// patterns coexist without conflicting and that 405 and 404 still behave.
	indexHandler := api.IndexHandler(version)
	for _, pattern := range []string{
		"GET /{$}",
		"GET /api",
		"GET /api/{$}",
		"GET /api/v1",
		"GET /api/v1/{$}",
	} {
		mux.Handle(pattern, indexHandler)
	}

	// OpenAPIPath carries the full /api/v1 prefix directly (rather than
	// humago.NewWithPrefix) so the spec and docs UI land exactly where
	// docs/PHASE1_BUILD_PLAN.md's WO-3 specifies: spec at
	// /api/v1/openapi.json, but docs UI unprefixed at /docs. NewWithPrefix
	// would prefix *both* since it wraps every adapter.Handle call,
	// including the ones NewAPI itself uses to register the openapi/docs
	// routes — which would put docs under /api/v1/docs instead.
	humaConfig := huma.DefaultConfig("open-exercise-db API", version)
	humaConfig.OpenAPIPath = "/api/v1/openapi"
	humaConfig.DocsPath = "/docs"
	humaConfig.Info.License = &huma.License{Name: "MIT", Identifier: "MIT"} // docs/DESIGN.md -> Licensing: code is MIT (data is CC0, tracked separately)
	// Servers is informational for spec consumers/linters (redocly's
	// no-empty-servers rule flags its absence); operation paths already
	// carry the full /api/v1 prefix themselves, so this just documents
	// where "/" resolves to rather than changing any routing.
	humaConfig.Servers = []*huma.Server{{URL: "/", Description: "Same origin as the API"}}

	hapi := humago.New(mux, humaConfig)
	api.Register(hapi, stores, exportCache, limiter, mediaStore, imageBaseURL, authCfg)
	return hapi
}

// writeOpenAPI prints the OpenAPI document without a database: Register only
// builds closures over its dependencies, and nothing here issues a request, so
// zero values are enough (internal/api's TestRegister_DoesNotPanic relies on
// the same fact).
func writeOpenAPI(w io.Writer, version string) error {
	limiter := ratelimit.NewLimiter(ratelimit.DefaultConfig())
	defer limiter.Close()

	hapi := registerAPI(http.NewServeMux(), version, store.New(nil), export.NewCache(export.DefaultCacheTTL, ""), limiter, nil, "", api.AuthConfig{})

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(hapi.OpenAPI())
}
