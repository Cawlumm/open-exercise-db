// Command api-server serves the open-exercise-db public API.
//
// Per docs/CONVENTIONS.md, main is thin: it loads config, constructs
// dependencies, and starts the server. It owns no business logic.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/Cawlumm/open-exercise-db/internal/api"
	"github.com/Cawlumm/open-exercise-db/internal/config"
	"github.com/Cawlumm/open-exercise-db/internal/database"
	"github.com/Cawlumm/open-exercise-db/internal/export"
	"github.com/Cawlumm/open-exercise-db/internal/ratelimit"
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// shutdownGracePeriod bounds how long in-flight requests get to finish once
// Cloud Run sends SIGTERM before the instance is killed outright.
const shutdownGracePeriod = 10 * time.Second

func main() {
	cfg := config.Load()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("connect database: %v", err)
	}
	defer pool.Close()

	if err := database.Migrate(ctx, pool); err != nil {
		log.Fatalf("migrate database: %v", err)
	}

	stores := store.New(pool)
	// exportCache is constructed here, not inside internal/api, per
	// docs/CONVENTIONS.md section 2's DI pattern — same shape as a
	// hypothetical media.Store parameter. It's owned by main so a future
	// Phase 2 wiring (e.g. calling exportCache.Invalidate() from the
	// submission-approval handler) has an obvious place to reach it from.
	exportCache := export.NewCache(export.DefaultCacheTTL)

	// limiter backs docs/PHASE1_BUILD_PLAN.md WO-7's origin rate-limit
	// backstop (internal/ratelimit's package doc has the honest read on
	// what this can and can't guarantee on autoscaled Cloud Run — the real,
	// globally-accurate enforcement is the Cloudflare edge rules documented
	// in docs/API_POLICY.md, not this process). Built from cfg.RateLimit so
	// every tier's rate/burst is independently overridable per environment
	// without a code change. Closed on shutdown to stop its cleanup
	// goroutine cleanly, same spirit as pool.Close() above.
	limiter := ratelimit.NewLimiter(ratelimit.Config{
		Read:         ratelimit.Rule{RPM: cfg.RateLimit.ReadRPM, Burst: cfg.RateLimit.ReadBurst},
		Search:       ratelimit.Rule{RPM: cfg.RateLimit.SearchRPM, Burst: cfg.RateLimit.SearchBurst},
		Export:       ratelimit.Rule{RPM: cfg.RateLimit.ExportRPM, Burst: cfg.RateLimit.ExportBurst},
		Unidentified: ratelimit.Rule{RPM: cfg.RateLimit.UnidentifiedRPM, Burst: cfg.RateLimit.UnidentifiedBurst},
	})
	defer limiter.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// OpenAPIPath carries the full /api/v1 prefix directly (rather than
	// humago.NewWithPrefix) so the spec and docs UI land exactly where
	// docs/PHASE1_BUILD_PLAN.md's WO-3 specifies: spec at
	// /api/v1/openapi.json, but docs UI unprefixed at /docs. NewWithPrefix
	// would prefix *both* since it wraps every adapter.Handle call,
	// including the ones NewAPI itself uses to register the openapi/docs
	// routes — which would put docs under /api/v1/docs instead.
	humaConfig := huma.DefaultConfig("open-exercise-db API", "1.0.0")
	humaConfig.OpenAPIPath = "/api/v1/openapi"
	humaConfig.DocsPath = "/docs"
	humaConfig.Info.License = &huma.License{Name: "MIT", Identifier: "MIT"} // docs/DESIGN.md -> Licensing: code is MIT (data is CC0, tracked separately)
	// Servers is informational for spec consumers/linters (redocly's
	// no-empty-servers rule flags its absence); operation paths already
	// carry the full /api/v1 prefix themselves, so this just documents
	// where "/" resolves to rather than changing any routing.
	humaConfig.Servers = []*huma.Server{{URL: "/", Description: "Same origin as the API"}}

	hapi := humago.New(mux, humaConfig)
	api.Register(hapi, stores, exportCache, limiter)

	server := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: mux,
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Printf("open-exercise-db API listening on %s (env=%s)", server.Addr, cfg.Env)
		serverErr <- server.ListenAndServe()
	}()

	select {
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	case <-ctx.Done():
		log.Print("shutdown signal received, draining connections")
		stop() // restore default signal handling so a second signal force-kills

		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGracePeriod)
		defer cancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Fatalf("graceful shutdown failed: %v", err)
		}
		log.Print("shutdown complete")
	}
}
