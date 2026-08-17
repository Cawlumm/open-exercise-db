package api

import (
	"errors"
	"io"
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
