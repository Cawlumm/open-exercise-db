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
