// Package media handles contributor-uploaded images and videos: server-side
// re-encoding (strip EXIF, normalize to JPEG, cap dimensions), duration/size
// validation for video, and the object-storage boundary.
//
// Store is the one genuine interface boundary in this project — it keeps
// Cloudflare R2 a config value rather than a rewrite if storage ever moves
// (docs/DESIGN.md → Hosting).
package media

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// Store abstracts object storage for uploaded media. Implemented by
// R2Store against Cloudflare R2, and by FilesystemStore for local
// development, tests, and self-hosters with no object storage configured.
//
// Both implementations must accept and reject exactly the same keys — see
// ValidateKey — so that a key written through one is always readable
// through the other. That property is what makes cmd/migrate-media a
// straight copy rather than a key-rewriting migration.
type Store interface {
	Put(ctx context.Context, key string, r io.Reader, contentType string) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)

	// Exists reports whether key holds an object, without transferring its
	// bytes. Get would answer the same question, but on R2 it bills and
	// downloads a full object to do it — and internal/seed asks this once
	// per image on every import run, so the difference is the whole dataset
	// over the wire versus a HEAD.
	//
	// Added because exercise_images rows and the objects they name have
	// independent lifetimes: Postgres persists on its own volume while the
	// bytes live in a container layer or an R2 bucket that can be recreated
	// under it. Nothing could ask "are the bytes actually there?" without
	// this, so internal/seed treated the row's existence as proof of the
	// object's and skipped re-downloading images that were already gone —
	// making the missing-bytes state permanent rather than self-healing.
	//
	// A false with a nil error means "definitively absent". An error means
	// the question could not be answered (network, credentials, permissions)
	// and callers must not read that as absence: the repair path in
	// internal/seed treats an error as "leave it alone", since re-uploading
	// on an unreadable answer would rewrite every object in the bucket the
	// first time a token lost its read scope.
	Exists(ctx context.Context, key string) (bool, error)
}

// ValidateKey rejects keys neither Store implementation should accept.
//
// Shared rather than per-implementation because the two backends have
// different reasons to care and would otherwise drift: for FilesystemStore
// a ".." segment is a real path-traversal escape, while for R2Store it is
// merely a legal-but-nonsensical object name. Applying the stricter rule to
// both keeps the key space identical across backends, which is the property
// Store's doc comment depends on — a key that stores on disk but is refused
// by R2 (or vice versa) would turn a backend swap into a silent data gap.
//
// Every real caller generates keys itself (internal/seed's
// "exercises/<source-id>/<n>.jpg"), so this never fires today. It is a
// boundary against a future caller passing an untrusted key — Phase 2's
// contributor uploads — not a workaround for an observed bug.
func ValidateKey(key string) error {
	if key == "" {
		return fmt.Errorf("invalid storage key: empty")
	}
	if strings.Contains(key, "..") {
		return fmt.Errorf("invalid storage key %q: contains %q", key, "..")
	}
	return nil
}
