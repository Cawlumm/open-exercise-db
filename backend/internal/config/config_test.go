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
