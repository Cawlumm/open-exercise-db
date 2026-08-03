// Package config loads runtime configuration from environment variables.
package config

import (
	"os"
	"path/filepath"
	"strconv"
)

// Config holds every runtime setting. Populated once at startup and passed
// explicitly to whatever needs it — no package-level access from other packages.
type Config struct {
	Env         string
	Port        string
	DatabaseURL string

	// GitHub OAuth (Phase 2).
	GitHubClientID     string
	GitHubClientSecret string
	JWTSecret          string
	AdminGitHubLogins  string

	// Cloudflare R2 object storage (Phase 2).
	R2AccountID       string
	R2AccessKeyID     string
	R2SecretAccessKey string
	R2Bucket          string

	// MediaDir is where internal/media.FilesystemStore re-hosts images
	// locally in Phase 1 (R2 doesn't land until Phase 2 — see
	// docs/PHASE1_BUILD_PLAN.md WO-4 and internal/media/doc.go). Always
	// resolved to an absolute path by Load (see below) — never used as the
	// raw, possibly-relative env value.
	MediaDir string

	// ImageBaseURL, when set, is the external host contract.Exercise.Images
	// URLs point at instead of this API's own GET /api/v1/images/{id} route
	// — production's Cloudflare R2/CDN host, once Phase 2's migration off
	// FilesystemStore lands, mirroring Open Food Facts' separate
	// images.openfoodfacts.org host so image bytes never touch the API
	// process (docs/DESIGN.md's "Alignment with Open Food Facts"). Empty in
	// local dev and self-host (the Phase 1 default): internal/media.ImageURL
	// falls back to the local route, which serves the exact same bytes via
	// media.Store — the seam that lets production adopt OFF's
	// separate-image-host model as a config change, not a code change.
	//
	// Not the same setting as cmd/import-freeexercisedb's `-image-base` flag
	// (internal/seed.Options.ImageBaseURL): that one resolves
	// free-exercise-db's *upstream* relative image paths at import time: this
	// one resolves *this API's own* re-hosted images at read time. They
	// happen to share a name in different packages; see
	// internal/media.ImageURL's doc comment for the same clarification.
	ImageBaseURL string

	// RateLimit configures internal/ratelimit's per-instance origin
	// backstop (docs/PHASE1_BUILD_PLAN.md WO-7, docs/API_POLICY.md). Every
	// field is independently overridable so a staging/CI instance can run
	// with sharply lower values than production without touching code — see
	// RateLimitConfig's own doc comment for why per-environment
	// configurability matters here specifically (a fixed constant would let
	// a staging instance trivially self-DoS itself, or force tests to sleep
	// through real production-sized windows).
	RateLimit RateLimitConfig
}

// RateLimitConfig holds the tuning knobs for internal/ratelimit.Limiter.
// This is a plain settings struct, not internal/ratelimit.Config itself —
// internal/ratelimit deliberately doesn't import internal/config (or vice
// versa beyond this field), so it stays usable/testable without pulling in
// env-var parsing; cmd/api-server/main.go is what bridges the two, the same
// DI-in-main pattern docs/CONVENTIONS.md section 2 already uses for
// export.Cache.
//
// Defaults are this project's own judgment call, not a copy of Open Food
// Facts' numbers (docs/PHASE1_BUILD_PLAN.md WO-7's table) — OFF's own
// 10 req/min search limit is called out in that same table as too tight for
// naive per-keystroke type-ahead, so Search here is deliberately looser,
// sized for a debounced client per docs/DESIGN.md's "Consuming this API"
// guidance (300-500ms debounce, occasional multi-keystroke bursts) rather
// than for a single lookup. Export is the lowest of the three cost tiers on
// purpose: it's the heaviest single request in the API and is meant to be
// mirrored on a schedule, not polled (docs/PHASE1_BUILD_PLAN.md WO-7).
// Unidentified (missing/generic User-Agent) is deliberately the strictest
// of all — see internal/api/ratelimit.go's isIdentifiedUA for the
// enforcement rationale.
//
// These are *origin* numbers, and this project's honest position (see
// internal/ratelimit's package doc) is that Cloudflare's edge rules
// (docs/API_POLICY.md) are the real, globally-accurate enforcement — these
// defaults exist so the origin still behaves sanely as defense-in-depth,
// not because they're the numbers the public policy promises.
type RateLimitConfig struct {
	ReadRPM   float64
	ReadBurst int

	SearchRPM   float64
	SearchBurst int

	ExportRPM   float64
	ExportBurst int

	UnidentifiedRPM   float64
	UnidentifiedBurst int
}

// Load reads configuration from the environment, applying defaults for local
// development. Returns the Config rather than setting a global so callers wire
// it explicitly — see docs/CONVENTIONS.md on dependency injection.
func Load() Config {
	return Config{
		Env:         getEnv("ENV", "development"),
		Port:        getEnv("PORT", "8080"),
		DatabaseURL: os.Getenv("DATABASE_URL"),

		GitHubClientID:     os.Getenv("GITHUB_OAUTH_CLIENT_ID"),
		GitHubClientSecret: os.Getenv("GITHUB_OAUTH_CLIENT_SECRET"),
		JWTSecret:          os.Getenv("JWT_SECRET"),
		AdminGitHubLogins:  os.Getenv("ADMIN_GITHUB_LOGINS"),

		R2AccountID:       os.Getenv("R2_ACCOUNT_ID"),
		R2AccessKeyID:     os.Getenv("R2_ACCESS_KEY_ID"),
		R2SecretAccessKey: os.Getenv("R2_SECRET_ACCESS_KEY"),
		R2Bucket:          os.Getenv("R2_BUCKET"),

		MediaDir:     resolveMediaDir(getEnv("MEDIA_DIR", "./data/media")),
		ImageBaseURL: os.Getenv("IMAGE_BASE_URL"),

		// See RateLimitConfig's doc comment for why these particular
		// defaults, and internal/api/ratelimit_test.go /
		// internal/ratelimit/ratelimit_test.go for how tests override them
		// to sub-second windows via the same env vars rather than sleeping
		// through production-sized ones.
		RateLimit: RateLimitConfig{
			ReadRPM:   getEnvFloat("OEDB_RATELIMIT_READ_RPM", 60),
			ReadBurst: getEnvInt("OEDB_RATELIMIT_READ_BURST", 20),

			// Matched to Read rather than set below it: docs/DESIGN.md makes
			// live lookup the *primary* consumption model, which makes search
			// the main endpoint for the integration path we recommend.
			// Throttling it under plain reads would penalize exactly that.
			SearchRPM:   getEnvFloat("OEDB_RATELIMIT_SEARCH_RPM", 60),
			SearchBurst: getEnvInt("OEDB_RATELIMIT_SEARCH_BURST", 20),

			ExportRPM:   getEnvFloat("OEDB_RATELIMIT_EXPORT_RPM", 5),
			ExportBurst: getEnvInt("OEDB_RATELIMIT_EXPORT_BURST", 2),

			UnidentifiedRPM:   getEnvFloat("OEDB_RATELIMIT_UNIDENTIFIED_RPM", 5),
			UnidentifiedBurst: getEnvInt("OEDB_RATELIMIT_UNIDENTIFIED_BURST", 2),
		},
	}
}

// resolveMediaDir converts dir to an absolute path, so where images actually
// land on disk stops depending on the current process's working directory —
// the exact defect this fixes: `MEDIA_DIR=./data/media` (the default)
// resolved to two different real directories depending on whether the
// process was started from the repo root or from backend/, silently
// splitting a single import's output across two locations with nothing to
// flag the mismatch. filepath.Abs can only fail if os.Getwd fails (a
// genuinely broken process environment), in which case dir is returned
// unresolved rather than failing startup outright — main logs whatever this
// returns, so even that degraded case is visible rather than silent.
func resolveMediaDir(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// getEnvFloat parses key as a float64, falling back to fallback if unset or
// unparseable. A malformed value falls back rather than crashing startup —
// consistent with getEnv's own "empty means fallback" behavior — since a
// rate limit misconfiguration should degrade to a safe default, not take
// the whole API down.
func getEnvFloat(key string, fallback float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fallback
	}
	return f
}

// getEnvInt mirrors getEnvFloat for integer-valued settings (burst sizes).
func getEnvInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return i
}
