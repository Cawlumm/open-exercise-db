package config_test

import (
	"strings"
	"testing"

	"github.com/Cawlumm/open-exercise-db/internal/config"
)

// fullR2 is a Config with all four object-storage settings populated —
// the shape UsesR2 is meant to recognize.
func fullR2() config.Config {
	return config.Config{
		R2AccountID:       "acct",
		R2AccessKeyID:     "key",
		R2SecretAccessKey: "secret",
		R2Bucket:          "bucket",
		MediaDir:          "/var/lib/oedb/media",
	}
}

func TestUsesR2(t *testing.T) {
	if !fullR2().UsesR2() {
		t.Error("UsesR2 with all four settings = false, want true")
	}

	// Each field blanked in turn: UsesR2 must be all-or-nothing, since
	// internal/media.NewStore treats a partial configuration as a hard error
	// rather than a backend choice, and a UsesR2 that answered true for a
	// partial config would let callers describe a backend that cannot be
	// built.
	for _, blank := range []string{"account", "key", "secret", "bucket"} {
		t.Run("missing_"+blank, func(t *testing.T) {
			c := fullR2()
			switch blank {
			case "account":
				c.R2AccountID = ""
			case "key":
				c.R2AccessKeyID = ""
			case "secret":
				c.R2SecretAccessKey = ""
			case "bucket":
				c.R2Bucket = ""
			}
			if c.UsesR2() {
				t.Errorf("UsesR2 with %s blank = true, want false", blank)
			}
		})
	}
}

func TestWarnings_AgreeingConfigsAreSilent(t *testing.T) {
	// The two configurations an operator is supposed to be running. Silence
	// is the actual contract here: Warnings exists so that seeing nothing
	// means the storage backend and IMAGE_BASE_URL agree, which is only
	// meaningful if these cases really do produce nothing.
	cases := map[string]config.Config{
		"filesystem with no IMAGE_BASE_URL": {MediaDir: "/var/lib/oedb/media"},
		"r2 with IMAGE_BASE_URL": func() config.Config {
			c := fullR2()
			c.ImageBaseURL = "https://images.example.org"
			return c
		}(),
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if w := c.Warnings(); len(w) != 0 {
				t.Errorf("Warnings() = %v, want none", w)
			}
		})
	}
}

func TestWarnings_FilesystemWithImageBaseURL(t *testing.T) {
	c := config.Config{
		MediaDir:     "/var/lib/oedb/media",
		ImageBaseURL: "https://images.example.org",
	}

	w := c.Warnings()
	if len(w) != 1 {
		t.Fatalf("Warnings() = %v, want exactly 1", w)
	}
	// The warning has to name both halves of the disagreement — an operator
	// reading it at 2am needs to know which directory holds the bytes and
	// which URL clients are being sent to, or it is just noise.
	if !strings.Contains(w[0], "https://images.example.org") {
		t.Errorf("warning omits IMAGE_BASE_URL: %q", w[0])
	}
	if !strings.Contains(w[0], "/var/lib/oedb/media") {
		t.Errorf("warning omits MediaDir: %q", w[0])
	}
}

func TestWarnings_R2WithoutImageBaseURL(t *testing.T) {
	w := fullR2().Warnings()
	if len(w) != 1 {
		t.Fatalf("Warnings() = %v, want exactly 1", w)
	}
	if !strings.Contains(w[0], "bucket") {
		t.Errorf("warning omits the bucket name: %q", w[0])
	}
	if !strings.Contains(w[0], "IMAGE_BASE_URL") {
		t.Errorf("warning omits IMAGE_BASE_URL: %q", w[0])
	}
}

func TestValidate_JWTSecret(t *testing.T) {
	long := strings.Repeat("x", config.MinJWTSecretBytes)
	cases := []struct {
		name    string
		env     string
		secret  string
		wantErr bool
	}{
		{"production placeholder", "production", config.PlaceholderJWTSecret, true},
		{"production short only warns", "production", strings.Repeat("x", config.MinJWTSecretBytes-1), false},
		{"production exact minimum", "production", long, false},
		{"production unset disables sign-in", "production", "", false},
		{"staging placeholder", "staging", config.PlaceholderJWTSecret, true},
		{"development placeholder", "development", config.PlaceholderJWTSecret, false},
		{"test short", "test", "abc", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := config.Config{Env: tc.env, JWTSecret: tc.secret}.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestWarnings_ShortJWTSecret(t *testing.T) {
	jwtWarning := func(c config.Config) bool {
		for _, w := range c.Warnings() {
			if strings.HasPrefix(w, "JWT_SECRET is") {
				return true
			}
		}
		return false
	}
	if !jwtWarning(config.Config{Env: "production", JWTSecret: strings.Repeat("x", config.MinJWTSecretBytes-1)}) {
		t.Fatal("no JWT_SECRET warning for a short production secret")
	}
	for _, c := range []config.Config{
		{Env: "production", JWTSecret: strings.Repeat("x", config.MinJWTSecretBytes)},
		{Env: "production", JWTSecret: ""},
		{Env: "development", JWTSecret: "abc"},
	} {
		if jwtWarning(c) {
			t.Errorf("JWT_SECRET warning for env %q with a %d-byte secret, want none", c.Env, len(c.JWTSecret))
		}
	}
}

func TestTrustedProxyMode(t *testing.T) {
	t.Setenv("TRUSTED_PROXY_MODE", "")
	t.Setenv("K_SERVICE", "")
	if got := config.Load().TrustedProxyMode; got != config.ProxyDirect {
		t.Errorf("default off Cloud Run = %q, want direct", got)
	}
	t.Setenv("K_SERVICE", "oedb-api")
	if got := config.Load().TrustedProxyMode; got != config.ProxyCloudRun {
		t.Errorf("default on Cloud Run = %q, want cloudrun", got)
	}
	t.Setenv("TRUSTED_PROXY_MODE", " Cloudflare ")
	if got := config.Load().TrustedProxyMode; got != config.ProxyCloudflare {
		t.Errorf("explicit mode = %q, want cloudflare", got)
	}
	if err := (config.Config{Env: "development", TrustedProxyMode: "bogus"}).Validate(); err == nil {
		t.Error("Validate accepted an unknown TRUSTED_PROXY_MODE")
	}
}

func TestAdminGitHubIDs(t *testing.T) {
	if err := (config.Config{AdminGitHubIDs: "123, 456"}).Validate(); err != nil {
		t.Errorf("numeric ids rejected: %v", err)
	}
	for _, bad := range []string{"octocat", "123,abc", "12 3", "-5"} {
		if err := (config.Config{AdminGitHubIDs: bad}).Validate(); err == nil {
			t.Errorf("Validate accepted ADMIN_GITHUB_IDS=%q", bad)
		}
	}
	has := func(c config.Config, sub string) bool {
		for _, w := range c.Warnings() {
			if strings.Contains(w, sub) {
				return true
			}
		}
		return false
	}
	if !has(config.Config{AdminGitHubLogins: "a"}, "ADMIN_GITHUB_LOGINS is deprecated:") {
		t.Error("no deprecation warning for logins alone")
	}
	if !has(config.Config{AdminGitHubLogins: "a", AdminGitHubIDs: "1"}, "is ignored because ADMIN_GITHUB_IDS is set") {
		t.Error("no ignored warning when both are set")
	}
	if has(config.Config{AdminGitHubIDs: "1"}, "deprecated") {
		t.Error("warned with only ids set")
	}
}
