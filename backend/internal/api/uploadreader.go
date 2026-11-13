package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
)

// errUploadTooLarge is returned by limitedReader once a body exceeds its cap.
//
// Distinct from io.EOF on purpose: truncating silently at the limit would hand
// media.Encode a partial image, which decodes to a corrupt-but-valid picture
// far more often than it errors — and the caller would be told their upload
// succeeded while a broken image went into the queue.
var errUploadTooLarge = errors.New("upload exceeds the size limit")

// limitedReader fails a stream that runs past max, rather than truncating it.
//
// Content-Length is checked before this, but a declared length is a claim. A
// chunked upload need not declare one at all, and one that does can lie, so the
// cap has to hold while the bytes are actually read.
type limitedReader struct {
	r         io.Reader
	remaining int64
}

func newLimitedReader(r io.Reader, max int64) io.Reader {
	return &limitedReader{r: r, remaining: max}
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.remaining <= 0 {
		return 0, errUploadTooLarge
	}
	if int64(len(p)) > l.remaining {
		p = p[:l.remaining]
	}
	n, err := l.r.Read(p)
	l.remaining -= int64(n)
	return n, err
}

// uploadRoute is the one route that accepts a large body.
const uploadRoute = "/api/v1/images"

// uploadBodySlack covers multipart framing and the small form fields around the
// file, so a file at exactly maxUploadBytes is not rejected for its envelope.
const uploadBodySlack = 64 << 10

// LimitUploadBody caps the request body of POST /api/v1/images while it is read.
//
// The size check in the handler runs after huma has parsed the multipart form,
// which buffers (and spills to disk) the whole body first — so by then an
// oversized upload has already cost its memory and disk. Wrapping the body here
// makes the read itself fail, and a declared Content-Length over the cap is
// refused before a byte is read.
func LimitUploadBody(h http.Handler) http.Handler {
	const limit = maxUploadBytes + uploadBodySlack
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == uploadRoute {
			if r.ContentLength > limit {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				fmt.Fprintf(w, `{"title":"Request Entity Too Large","status":413,"detail":"request body exceeds the %d byte limit"}`, limit)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		h.ServeHTTP(w, r)
	})
}
