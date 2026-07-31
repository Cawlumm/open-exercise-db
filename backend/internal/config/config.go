// Package config loads runtime configuration from environment variables.
package config

import "os"

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
	// docs/PHASE1_BUILD_PLAN.md WO-4 and internal/media/doc.go).
	MediaDir string
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

		MediaDir: getEnv("MEDIA_DIR", "./data/media"),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
