package media

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// FilesystemStore implements Store by writing to a local directory. It
// backs local development and Phase 1 (docs/PHASE1_BUILD_PLAN.md WO-4) —
// Cloudflare R2 doesn't land until Phase 2, and Store exists as an
// interface precisely so that swap is a config change, not a rewrite
// (docs/DESIGN.md -> Hosting).
type FilesystemStore struct {
	baseDir string
}

// NewFilesystemStore returns a Store rooted at baseDir, creating it (and any
// missing parents) if it doesn't already exist.
func NewFilesystemStore(baseDir string) (*FilesystemStore, error) {
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, fmt.Errorf("create media directory %s: %w", baseDir, err)
	}
	return &FilesystemStore{baseDir: baseDir}, nil
}

// Put writes r to baseDir/key, creating any parent directories key implies.
// It writes to a temp file and renames into place so a reader can never
// observe a partially-written file, and so a failed write never leaves a
// corrupt file at key's final path. contentType is accepted to satisfy the
// Store interface but isn't persisted here — internal/store's
// exercise_images.content_type column is the source of truth for content
// type (docs/PHASE1_BUILD_PLAN.md's blocker resolution: "store the real
// content_type ... so the format is data, not an assumption"), not the
// filesystem layout.
func (fs *FilesystemStore) Put(_ context.Context, key string, r io.Reader, _ string) error {
	path, err := fs.resolve(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create parent dir for %s: %w", key, err)
	}

	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create %s: %w", key, err)
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write %s: %w", key, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close %s: %w", key, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("finalize %s: %w", key, err)
	}
	return nil
}

// Get opens the file at baseDir/key for reading. The caller owns closing it.
func (fs *FilesystemStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	path, err := fs.resolve(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", key, err)
	}
	return f, nil
}

// resolve joins key onto baseDir and rejects any key that would escape it.
// Every real caller generates keys itself (e.g. "exercises/<source-id>/0.jpg"),
// so this never fires in practice — it's a hard boundary against a future
// caller passing an untrusted key, not a workaround for an observed bug.
func (fs *FilesystemStore) resolve(key string) (string, error) {
	if key == "" || strings.Contains(key, "..") {
		return "", fmt.Errorf("invalid storage key %q", key)
	}
	return filepath.Join(fs.baseDir, filepath.FromSlash(key)), nil
}
