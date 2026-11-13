package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
)

// ContentSHA256 is the hash recorded in exercise_image_variants.content_sha256:
// sha256 of the stored bytes, 64 lowercase hex characters.
func ContentSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ThumbStorageKey derives the thumb's key from the full rendition's:
// "exercises/Squat/0.jpg" becomes "exercises/Squat/0.thumb.jpg".
//
// Derived once, at write time, and then recorded in the variant row; no
// reader computes it (migration 0002). Deterministic so a re-run of the
// backfill overwrites its own earlier object rather than leaving orphans.
func ThumbStorageKey(fullKey string) string {
	return strings.TrimSuffix(fullKey, path.Ext(fullKey)) + ".thumb.jpg"
}

// Thumb is one stored thumb rendition, ready to record as a variant row.
type Thumb struct {
	StorageKey    string
	ContentSHA256 string
	EncodeResult
}

// StoreThumb encodes a thumb from the full rendition's bytes and writes it
// next to them. The caller records the variant row afterwards: the object
// lands first so a row never names bytes that were not written.
func StoreThumb(ctx context.Context, s Store, fullKey string, full []byte) (Thumb, error) {
	encoded, err := EncodeThumb(bytes.NewReader(full))
	if err != nil {
		return Thumb{}, err
	}
	key := ThumbStorageKey(fullKey)
	if err := s.Put(ctx, key, bytes.NewReader(encoded.Data), encoded.ContentType); err != nil {
		return Thumb{}, fmt.Errorf("store thumb %s: %w", key, err)
	}
	return Thumb{StorageKey: key, ContentSHA256: ContentSHA256(encoded.Data), EncodeResult: encoded}, nil
}
