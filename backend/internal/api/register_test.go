package api

import (
	"testing"

	"github.com/danielgtaylor/huma/v2/humatest"

	"github.com/Cawlumm/open-exercise-db/internal/export"
	"github.com/Cawlumm/open-exercise-db/internal/media"
	"github.com/Cawlumm/open-exercise-db/internal/ratelimit"
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// TestRegister_DoesNotPanic calls the real Register — not
// registerExerciseRoutes/registerMetaRoutes/registerExportRoutes
// individually the way every other test in this package does — against a
// real huma.API instance, so huma's own OpenAPI schema-registry validation
// actually runs.
//
// This is the test that was missing when WO-5 shipped with a registration
// panic: internal/export originally declared its own Exercise/Translation
// types, structurally identical to but independently declared from this
// package's, and huma's component-schema registry keys schemas by Go type
// name (not full import path) — the instant Register wired both packages'
// routes onto one huma.API, it panicked with "duplicate name: Exercise, new
// type: export.Exercise, existing type: api.Exercise" and
// cmd/api-server/main.go could not start. Every other test in this file
// registers at most one route group per humatest.New call (a fresh
// registry each time), so none of them could ever have caught a collision
// that only exists once every route group shares one registry — which is
// exactly what main.go does in production. A fully green `go test ./...`
// coexisting with a server that panics on boot is the failure mode this
// test exists to close.
//
// No database is needed: Register only builds closures over s during
// registration and huma only validates/derives schemas from the Go types in
// each operation's Input/Output structs — nothing here executes a query, so
// a nil pool is enough and this runs in milliseconds.
func TestRegister_DoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Register panicked: %v", r)
		}
	}()

	s := store.New(nil)
	cache := export.NewCache(export.DefaultCacheTTL, "")
	limiter := ratelimit.NewLimiter(ratelimit.DefaultConfig())
	t.Cleanup(limiter.Close)
	_, hapi := humatest.New(t)

	// mediaStore only needs to be non-nil enough to satisfy the parameter —
	// like s's nil pool above, registration never calls into it (no request
	// is made in this test), only huma's schema derivation over GetImageInput/
	// GetImageOutput's Go types.
	mediaStore, err := media.NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatalf("init media store: %v", err)
	}

	Register(hapi, s, cache, limiter, mediaStore, "", AuthConfig{})
}
