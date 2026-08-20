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
	"github.com/Cawlumm/open-exercise-db/internal/auth"
	"github.com/Cawlumm/open-exercise-db/internal/config"
	"github.com/Cawlumm/open-exercise-db/internal/database"
	"github.com/Cawlumm/open-exercise-db/internal/export"
	"github.com/Cawlumm/open-exercise-db/internal/media"
	"github.com/Cawlumm/open-exercise-db/internal/ratelimit"
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// shutdownGracePeriod bounds how long in-flight requests get to finish once
// Cloud Run sends SIGTERM before the instance is killed outright.
const shutdownGracePeriod = 10 * time.Second

// apiVersion is reported by the OpenAPI document and the discovery index. One
// constant so the two cannot disagree — a spec claiming a different version
// than the index is the kind of contradiction a consumer notices and cannot
// resolve.
const apiVersion = "1.0.0"

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

	// mediaStore backs GET /api/v1/images/{id} — the same storage the
	// bootstrap importer (cmd/import-freeexercisedb) writes re-hosted images
	// to, selected by the identical media.NewStore call so the two commands
	// can never disagree about which backend is in play. R2 when its four
	// credentials are set, otherwise the filesystem at cfg.MediaDir (absolute
	// — see config.Load — so this process serves the same files regardless of
	// the working directory it was started from).
	mediaStore, mediaDesc, err := media.NewStore(media.StoreOptions{
		R2AccountID:       cfg.R2AccountID,
		R2AccessKeyID:     cfg.R2AccessKeyID,
		R2SecretAccessKey: cfg.R2SecretAccessKey,
		R2Bucket:          cfg.R2Bucket,
		MediaDir:          cfg.MediaDir,
	})
	if err != nil {
		log.Fatalf("init media store: %v", err)
	}
	log.Printf("media store: %s (IMAGE_BASE_URL=%q)", mediaDesc, cfg.ImageBaseURL)
	for _, w := range cfg.Warnings() {
		log.Printf("config warning: %s", w)
	}

	// exportCache is constructed here, not inside internal/api, per
	// docs/CONVENTIONS.md section 2's DI pattern — same shape as mediaStore
	// above. It's owned by main so a future Phase 2 wiring (e.g. calling
	// exportCache.Invalidate() from the submission-approval handler) has an
	// obvious place to reach it from.
	exportCache := export.NewCache(export.DefaultCacheTTL, cfg.ImageBaseURL)

	// limiter backs docs/PHASE1_BUILD_PLAN.md WO-7's origin rate-limit
	// backstop (internal/ratelimit's package doc has the honest read on
	// what this can and can't guarantee on autoscaled Cloud Run — the real,
	// globally-accurate enforcement is the Cloudflare edge rules documented
	// in docs/API_POLICY.md, not this process). Built from cfg.RateLimit so
	// every tier's rate/burst is independently overridable per environment
	// without a code change. Closed on shutdown to stop its cleanup
	// goroutine cleanly, same spirit as pool.Close() above.
	limiter := ratelimit.NewLimiter(ratelimit.Config{
		Flood:        ratelimit.Rule{RPM: cfg.RateLimit.FloodRPM, Burst: cfg.RateLimit.FloodBurst},
		App:          ratelimit.Rule{RPM: cfg.RateLimit.AppRPM, Burst: cfg.RateLimit.AppBurst},
		Image:        ratelimit.Rule{RPM: cfg.RateLimit.ImageRPM, Burst: cfg.RateLimit.ImageBurst},
		Read:         ratelimit.Rule{RPM: cfg.RateLimit.ReadRPM, Burst: cfg.RateLimit.ReadBurst},
		Search:       ratelimit.Rule{RPM: cfg.RateLimit.SearchRPM, Burst: cfg.RateLimit.SearchBurst},
		Export:       ratelimit.Rule{RPM: cfg.RateLimit.ExportRPM, Burst: cfg.RateLimit.ExportBurst},
		Unidentified: ratelimit.Rule{RPM: cfg.RateLimit.UnidentifiedRPM, Burst: cfg.RateLimit.UnidentifiedBurst},
	})
	defer limiter.Close()

	mux := http.NewServeMux()
	// Health pings the database rather than answering ok unconditionally.
	// This is what a load balancer routes on, and every useful endpoint here
	// needs Postgres: an instance that cannot reach the database has nothing to
	// serve, so a health check that ignores the database reports ready for
	// an instance that will 500 every request it receives. The ping is
	// bounded well inside any sane probe interval — a health check that
	// hangs is indistinguishable from one that fails, except it ties up a
	// connection while being useless.
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		pingCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		w.Header().Set("Content-Type", "application/json")
		if err := pool.Ping(pingCtx); err != nil {
			log.Printf("health: database ping failed: %v", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"status": "unavailable", "reason": "database"})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// Each entry URL is registered explicitly, with and without its trailing
	// slash. "/api/v1/{$}" alone matches only "/api/v1/", so "/api/v1" — the
	// form someone actually copies out of a URL — got ServeMux's implicit 307
	// and an HTML redirect stub instead of the document, and "/api" was never
	// registered at all. Verified against this Go version that all five
	// patterns coexist without conflicting and that 405 and 404 still behave.
	indexHandler := api.IndexHandler(apiVersion)
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
	humaConfig := huma.DefaultConfig("open-exercise-db API", apiVersion)
	humaConfig.OpenAPIPath = "/api/v1/openapi"
	humaConfig.DocsPath = "/docs"
	humaConfig.Info.License = &huma.License{Name: "MIT", Identifier: "MIT"} // docs/DESIGN.md -> Licensing: code is MIT (data is CC0, tracked separately)
	// Servers is informational for spec consumers/linters (redocly's
	// no-empty-servers rule flags its absence); operation paths already
	// carry the full /api/v1 prefix themselves, so this just documents
	// where "/" resolves to rather than changing any routing.
	humaConfig.Servers = []*huma.Server{{URL: "/", Description: "Same origin as the API"}}

	// Sign-in is optional. A self-hoster who sets no OAuth credentials gets a
	// working read API and a 503 on /auth/github, rather than a service that
	// refuses to start over a feature they may not want.
	authCfg := api.AuthConfig{
		JWTSecret:         cfg.JWTSecret,
		AdminGitHubLogins: cfg.AdminGitHubLogins,
		GitHubOAuth: auth.GitHubOAuth{
			ClientID:     cfg.GitHubClientID,
			ClientSecret: cfg.GitHubClientSecret,
			RedirectURL:  cfg.GitHubRedirectURL,
			Endpoints:    auth.DefaultGitHubEndpoints(),
		},
	}

	hapi := humago.New(mux, humaConfig)
	api.Register(hapi, stores, exportCache, limiter, mediaStore, cfg.ImageBaseURL, authCfg)

	server := &http.Server{
		Addr: ":" + cfg.Port,
		// Wrapped, not routed. An earlier revision mounted a catch-all on the
		// bare "/" pattern to give typo'd paths a problem+json 404; that also
		// made ServeMux's 405 branch unreachable, so every wrong-verb request
		// answered 404 with no Allow header. api.ProblemErrors leaves routing
		// alone and rewrites only the plain-text bodies ServeMux itself
		// produces — see internal/api/errors.go for why no route-shaped fix
		// exists.
		//
		// CORS is outermost so a preflight is answered before routing can 405
		// it, and so the cross-origin headers are present on error responses
		// too — a browser that cannot read a 429 is a browser that cannot obey
		// docs/API_POLICY.md's back-off rules.
		// Compress sits inside CORS and outside ProblemErrors: it must see the
		// final body — including the problem+json ProblemErrors substitutes for
		// ServeMux's plain-text errors — while CORS headers stay plainly
		// readable on the wire whatever encoding the body ends up in.
		Handler: api.CORS(api.Compress(api.ProblemErrors(mux))),

		// Without these, a connection that opens and then dribbles its
		// request line one byte per minute holds a goroutine and a file
		// descriptor indefinitely — the whole of Slowloris, and free to
		// mount against a public zero-auth API. Cloud Run's request timeout
		// does not cover it: that bounds a request once dispatched, while
		// this is about connections that never finish arriving. Neither does
		// the rate limiter, which runs per request — these never become one.
		//
		// ReadHeaderTimeout is the one that closes that hole; ReadTimeout
		// and IdleTimeout are the same argument for the body and for
		// keep-alive connections held open after a response.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,

		// WriteTimeout is deliberately the most generous of the four.
		// GET /exercises/export serves the whole dataset — 1.6MB today and
		// growing — so a limit tuned to the median response would sever
		// exactly the request that endpoint exists for, on exactly the slow
		// connections least able to retry it.
		WriteTimeout: 120 * time.Second,

		// Already the net/http default; stated so the header budget sits
		// beside the timeouts bounding the same resource.
		MaxHeaderBytes: 1 << 20,
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
