// Package media handles contributor-uploaded images and videos: server-side
// re-encoding (strip EXIF, normalize to webp, cap dimensions), duration/size
// validation for video, and the object-storage boundary.
//
// Store is the one genuine interface boundary in this project — it keeps
// Cloudflare R2 a config value rather than a rewrite if storage ever moves
// (docs/DESIGN.md → Hosting).
package media

import (
	"context"
	"io"
)

// Store abstracts object storage for uploaded media. Implemented by R2 in
// production; a filesystem implementation backs local development and tests.
type Store interface {
	Put(ctx context.Context, key string, r io.Reader, contentType string) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
}
