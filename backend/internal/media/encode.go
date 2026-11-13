package media

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // registers gif decoding with image.Decode
	"image/jpeg"
	_ "image/png" // registers png decoding with image.Decode
	"io"

	"golang.org/x/image/draw"
)

// maxDimension caps the longer edge of a re-encoded image, per
// docs/PHASE1_BUILD_PLAN.md WO-4 ("cap ~800px").
const maxDimension = 800

// MaxSourcePixels and MaxSourceSide bound the declared size of an image Encode
// will decode: 40 megapixels (about 160MB as RGBA) and 8000px on either side.
// Phone cameras top out well under both.
const (
	MaxSourcePixels = 40_000_000
	MaxSourceSide   = 8000
)

// ErrImageTooLarge is returned when an image's declared dimensions exceed
// MaxSourcePixels or MaxSourceSide.
var ErrImageTooLarge = errors.New("image dimensions exceed the limit")

// jpegQuality balances re-hosted file size against visual quality. These are
// small reference photos, not a page's visual centerpiece, so 85 — the
// commonly-cited "visually lossless enough" JPEG quality level — is a
// reasonable default rather than a tuned constant; revisit if real output
// looks off.
const jpegQuality = 85

// EncodeResult is what Encode produces: the re-encoded bytes ready to hand
// to a Store.Put, plus the metadata exercise_images needs recorded.
type EncodeResult struct {
	Data        []byte
	ContentType string
	Width       int
	Height      int
}

// Encode decodes an arbitrary jpeg/png/gif image, resizes it so neither
// dimension exceeds maxDimension (aspect ratio preserved, never upscaled),
// and re-encodes it as JPEG.
//
// Why JPEG and not webp, which docs/PHASE1_BUILD_PLAN.md's WO-4 originally
// specified ("normalize to webp"): there is no maintained pure-Go webp
// *encoder*. golang.org/x/image/webp only decodes; every encoder in the Go
// ecosystem (kolesa-team/go-webp, chai2010/webp, etc.) shells out to
// libwebp via cgo. backend/Dockerfile builds with CGO_ENABLED=0 into
// gcr.io/distroless/static-debian12 specifically for Cloud Run's
// cold-start economics (docs/DESIGN.md -> Hosting); a cgo dependency would
// break that build outright and undermine the small-static-binary
// rationale. Re-encoding to JPEG via the stdlib needs zero new
// dependencies, and decode-then-re-encode strips EXIF as a side effect —
// Go's jpeg encoder never writes EXIF back — without a separate scrubbing
// step. The real content_type is recorded on exercise_images rather than
// assumed by callers, so this is data, not a hardcoded format: webp/AVIF
// can be revisited behind the same Store interface later, once a pure-Go or
// WASM encoder exists, or once R2 (Phase 2) makes an image-processing
// sidecar (a small cgo/ffmpeg service outside the CGO_ENABLED=0 binary)
// reasonable.
func Encode(r io.Reader) (EncodeResult, error) {
	// The header is read before any pixels are decoded. A PNG a few hundred
	// bytes long can declare 60000x60000 and make image.Decode allocate
	// gigabytes, so the declared size is judged first. The caller bounds how
	// many bytes r yields, which is what makes buffering it here safe.
	raw, err := io.ReadAll(r)
	if err != nil {
		return EncodeResult{}, fmt.Errorf("read image: %w", err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return EncodeResult{}, fmt.Errorf("decode image: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 ||
		cfg.Width > MaxSourceSide || cfg.Height > MaxSourceSide ||
		int64(cfg.Width)*int64(cfg.Height) > MaxSourcePixels {
		return EncodeResult{}, fmt.Errorf("%w: %dx%d", ErrImageTooLarge, cfg.Width, cfg.Height)
	}

	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return EncodeResult{}, fmt.Errorf("decode image: %w", err)
	}

	img = resizeToFit(img, maxDimension)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return EncodeResult{}, fmt.Errorf("encode jpeg: %w", err)
	}

	bounds := img.Bounds()
	return EncodeResult{
		Data:        buf.Bytes(),
		ContentType: "image/jpeg",
		Width:       bounds.Dx(),
		Height:      bounds.Dy(),
	}, nil
}

// resizeToFit scales img down (never up) so neither dimension exceeds max,
// preserving aspect ratio. golang.org/x/image/draw is pure Go — same
// no-cgo rationale as Encode's doc comment — and is the standard resize
// helper for exactly this case; disintegration/imaging (docs/DESIGN.md's
// Tech stack table) would be a heavier alternative for the one operation
// actually needed here.
func resizeToFit(img image.Image, max int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= max && h <= max {
		return img
	}

	scale := float64(max) / float64(w)
	if h > w {
		scale = float64(max) / float64(h)
	}
	newW := maxInt(1, int(float64(w)*scale))
	newH := maxInt(1, int(float64(h)*scale))

	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Over, nil)
	return dst
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
