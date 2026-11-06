package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image/jpeg"
	"io"
	"testing"
)

func TestEncodeThumb_DimensionsAndFormat(t *testing.T) {
	for _, tc := range []struct {
		name         string
		w, h         int
		wantW, wantH int
	}{
		{"landscape full rendition", 800, 600, 128, 96},
		{"portrait full rendition", 600, 800, 96, 128},
		{"already small is not upscaled", 100, 50, 100, 50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EncodeThumb(bytes.NewReader(makeJPEG(t, tc.w, tc.h)))
			if err != nil {
				t.Fatalf("EncodeThumb: %v", err)
			}
			if got.ContentType != "image/jpeg" {
				t.Errorf("ContentType = %q, want image/jpeg", got.ContentType)
			}
			if got.Width != tc.wantW || got.Height != tc.wantH {
				t.Errorf("reported %dx%d, want %dx%d", got.Width, got.Height, tc.wantW, tc.wantH)
			}
			// What is reported must be what was written.
			cfg, err := jpeg.DecodeConfig(bytes.NewReader(got.Data))
			if err != nil {
				t.Fatalf("thumb is not a decodable JPEG: %v", err)
			}
			if cfg.Width != tc.wantW || cfg.Height != tc.wantH {
				t.Errorf("encoded %dx%d, want %dx%d", cfg.Width, cfg.Height, tc.wantW, tc.wantH)
			}
		})
	}
}

// The thumb exists to be small. A gradient is harder to compress than a
// photo of a person on a plain background, so this bound is loose for the
// real dataset rather than tight.
func TestEncodeThumb_IsAFractionOfTheFullRendition(t *testing.T) {
	full, err := Encode(bytes.NewReader(makeJPEG(t, 800, 600)))
	if err != nil {
		t.Fatal(err)
	}
	thumb, err := EncodeThumb(bytes.NewReader(full.Data))
	if err != nil {
		t.Fatal(err)
	}
	if len(thumb.Data) > 10*1024 || len(thumb.Data)*5 > len(full.Data) {
		t.Errorf("thumb is %d bytes against a %d-byte full rendition; want under 10 KB and under a fifth", len(thumb.Data), len(full.Data))
	}
}

func TestEncodeThumb_RejectsNonImage(t *testing.T) {
	if _, err := EncodeThumb(bytes.NewReader([]byte("not an image"))); err == nil {
		t.Error("EncodeThumb accepted bytes that are not an image")
	}
}

func TestContentSHA256(t *testing.T) {
	data := []byte("bytes")
	sum := sha256.Sum256(data)
	if got, want := ContentSHA256(data), hex.EncodeToString(sum[:]); got != want {
		t.Errorf("ContentSHA256 = %s, want %s", got, want)
	}
	if len(ContentSHA256(nil)) != 64 {
		t.Error("ContentSHA256 must always be 64 hex characters (migration 0005's CHECK)")
	}
}

func TestThumbStorageKey(t *testing.T) {
	for in, want := range map[string]string{
		"exercises/Squat/0.jpg":         "exercises/Squat/0.thumb.jpg",
		"uploads/app/1700000000-5.jpg":  "uploads/app/1700000000-5.thumb.jpg",
		"exercises/Dotted.Name/1.jpeg":  "exercises/Dotted.Name/1.thumb.jpg",
		"exercises/no-extension/object": "exercises/no-extension/object.thumb.jpg",
	} {
		if got := ThumbStorageKey(in); got != want {
			t.Errorf("ThumbStorageKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStoreThumb_WritesWhatItReports(t *testing.T) {
	ctx := context.Background()
	fs, err := NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	full := makeJPEG(t, 800, 600)

	thumb, err := StoreThumb(ctx, fs, "exercises/Squat/0.jpg", full)
	if err != nil {
		t.Fatalf("StoreThumb: %v", err)
	}
	if thumb.StorageKey != "exercises/Squat/0.thumb.jpg" {
		t.Errorf("StorageKey = %q", thumb.StorageKey)
	}

	rc, err := fs.Get(ctx, thumb.StorageKey)
	if err != nil {
		t.Fatalf("thumb object not written: %v", err)
	}
	defer rc.Close()
	stored, _ := io.ReadAll(rc)
	if !bytes.Equal(stored, thumb.Data) {
		t.Error("stored bytes differ from the reported encode")
	}
	if thumb.ContentSHA256 != ContentSHA256(stored) {
		t.Error("ContentSHA256 does not describe the stored bytes")
	}
}
