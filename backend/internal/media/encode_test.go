package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

// makeJPEG builds a synthetic w x h JPEG in memory so tests never need a
// fixture file or network access.
func makeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("makeJPEG: %v", err)
	}
	return buf.Bytes()
}

func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("makePNG: %v", err)
	}
	return buf.Bytes()
}

func TestEncode_SmallImagePassesThroughDimensions(t *testing.T) {
	src := makeJPEG(t, 100, 50)

	result, err := Encode(bytes.NewReader(src))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	if result.ContentType != "image/jpeg" {
		t.Errorf("ContentType = %q, want image/jpeg", result.ContentType)
	}
	if result.Width != 100 || result.Height != 50 {
		t.Errorf("dimensions = %dx%d, want 100x50 (below cap, no resize expected)", result.Width, result.Height)
	}

	// The output must itself decode as a valid JPEG.
	if _, err := jpeg.Decode(bytes.NewReader(result.Data)); err != nil {
		t.Errorf("re-encoded output does not decode as jpeg: %v", err)
	}
}

func TestEncode_LargeImageIsCappedAtMaxDimension(t *testing.T) {
	src := makeJPEG(t, 2000, 1000) // 2:1 aspect ratio, both dimensions over the cap

	result, err := Encode(bytes.NewReader(src))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	if result.Width != maxDimension {
		t.Errorf("Width = %d, want %d (the longer edge, capped)", result.Width, maxDimension)
	}
	wantHeight := maxDimension / 2 // aspect ratio preserved
	if result.Height != wantHeight {
		t.Errorf("Height = %d, want %d (aspect ratio preserved)", result.Height, wantHeight)
	}
}

func TestEncode_PNGInputIsAcceptedAndConvertedToJPEG(t *testing.T) {
	src := makePNG(t, 40, 40)

	result, err := Encode(bytes.NewReader(src))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if result.ContentType != "image/jpeg" {
		t.Errorf("ContentType = %q, want image/jpeg (source format is data, output format is not)", result.ContentType)
	}
}

func TestEncode_GarbageInputIsRejected(t *testing.T) {
	if _, err := Encode(bytes.NewReader([]byte("not an image"))); err == nil {
		t.Fatal("Encode: expected an error for non-image input, got nil")
	}
}

// pngHeaderOnly is a PNG signature plus a valid IHDR declaring w x h and
// nothing else: a few dozen bytes that decode to nothing but claim any size.
func pngHeaderOnly(w, h uint32) []byte {
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8], ihdr[9] = 8, 2 // 8-bit RGB
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	_ = binary.Write(&b, binary.BigEndian, uint32(len(ihdr)))
	chunk := append([]byte("IHDR"), ihdr...)
	b.Write(chunk)
	_ = binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(chunk))
	return b.Bytes()
}

func TestEncode_RejectsOversizedDeclaredDimensions(t *testing.T) {
	cases := map[string][2]uint32{
		"too many pixels": {7000, 7000},
		"wide side":       {8001, 10},
		"tall side":       {10, 60000},
		"huge":            {60000, 60000},
	}
	for name, dim := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Encode(bytes.NewReader(pngHeaderOnly(dim[0], dim[1])))
			if !errors.Is(err, ErrImageTooLarge) {
				t.Fatalf("err = %v, want ErrImageTooLarge", err)
			}
		})
	}
}

func TestEncode_AcceptsDimensionsAtTheLimit(t *testing.T) {
	// Header only, so decoding fails after the size check passes: the error
	// must be a decode error, not the size one.
	_, err := Encode(bytes.NewReader(pngHeaderOnly(8000, 5000)))
	if err == nil || errors.Is(err, ErrImageTooLarge) {
		t.Fatalf("err = %v, want a decode error past the size check", err)
	}
}
