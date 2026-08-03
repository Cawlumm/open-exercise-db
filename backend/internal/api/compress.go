package api

import (
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// Nothing this API served was compressed. A 50-item page of /exercises is ~87
// KB of JSON and /exercises/export is ~1.6 MB, both sent verbatim — and JSON of
// this shape (heavily repeated field names, long instruction prose) is exactly
// what gzip is good at, typically an 8-10x reduction.
//
// It matters most for the two consumption patterns docs/DESIGN.md names as
// primary. A debounced type-ahead search issues a request per keystroke pause,
// so 87 KB per keystroke is the difference between usable and not on a phone;
// and a mirroring consumer pulling /export repeatedly moves megabytes it does
// not need to. Cloudflare would compress this in the hosted deployment, which
// is presumably why it was never noticed — but that leaves every self-hosted
// instance (a first-class case for this project) and every direct-to-origin
// client paying full freight.
//
// Placed inside CORS and outside ProblemErrors so it compresses real handler
// responses and rewritten problem+json alike, while the CORS headers stay
// readable on the wire regardless of encoding.

// compressMinBytes is the size below which compressing is not worth it: the
// gzip header and trailer alone are ~20 bytes, and for a short error body the
// result can be larger than the input. Only applied when the response declares
// its length up front — a streamed body of unknown size is compressed on the
// assumption that anything worth streaming is worth compressing.
const compressMinBytes = 1024

// gzipWriterPool reuses gzip writers across requests. Allocating one per
// response would put a 64 KB window buffer on the heap for every request on the
// hot search path.
var gzipWriterPool = sync.Pool{
	New: func() any { return gzip.NewWriter(nil) },
}

// compressibleTypes are the content types worth compressing. Deliberately a
// list rather than "everything except images": the only large binary this API
// serves is JPEG (internal/media/encode.go re-encodes every image to it), which
// is already compressed, and gzipping it would burn CPU to make the response
// marginally bigger.
func isCompressible(contentType string) bool {
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	switch ct {
	case "application/json", "application/problem+json", "application/yaml",
		"application/x-yaml", "text/yaml", "text/html", "text/plain", "text/csv":
		return true
	}
	return false
}

func clientAcceptsGzip(r *http.Request) bool {
	for _, enc := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		// Split off any q-value: "gzip;q=0" means the client explicitly does
		// not want it, and treating that as acceptance would send an encoding
		// it refused.
		name, params, _ := strings.Cut(strings.TrimSpace(enc), ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		if strings.ReplaceAll(strings.TrimSpace(params), " ", "") == "q=0" {
			return false
		}
		return true
	}
	return false
}

// gzipResponseWriter buffers the start of the body so the size threshold can be
// applied to the bytes actually produced.
//
// Reading Content-Length at WriteHeader time does not work, which is worth
// recording because it looks like it should: net/http computes Content-Length
// itself for a small response, after the handler returns, so at WriteHeader the
// header is simply absent and every short body looks like an unknown-length
// stream. The first version of this file trusted it, the threshold never fired
// once, and a 130-byte /meta/locales response was gzipped into 132 bytes —
// compression making the response bigger, which is exactly what the threshold
// existed to prevent.
//
// So: hold bytes until either the threshold is crossed (compress, flushing what
// was buffered) or the handler finishes (write through uncompressed). The
// buffer is bounded by compressMinBytes and released immediately after.
type gzipResponseWriter struct {
	http.ResponseWriter

	status      int
	wroteHeader bool // the real writer's WriteHeader has been called
	passthrough bool // decided: never compress this response
	buf         []byte
	gz          *gzip.Writer
}

func (w *gzipResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status

	header := w.Header()
	// A 304 carries no body and a 204 has none by definition; attaching a gzip
	// header to either produces a response that must be empty but is not.
	noBody := status == http.StatusNotModified || status == http.StatusNoContent
	if noBody || !isCompressible(header.Get("Content-Type")) {
		w.passthrough = true
		w.wroteHeader = true
		w.ResponseWriter.WriteHeader(status)
	}
	// Otherwise the status is held back: whether Content-Encoding belongs in
	// the headers is not known until enough body has been seen, and headers
	// cannot be changed once WriteHeader has been called on the real writer.
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.passthrough {
		return w.ResponseWriter.Write(b)
	}
	if w.gz != nil {
		return w.gz.Write(b)
	}

	w.buf = append(w.buf, b...)
	if len(w.buf) < compressMinBytes {
		// Still undecided. Report the write as accepted — it is, into the
		// buffer — so a handler checking the count does not see a short write.
		return len(b), nil
	}

	w.startGzip()
	buffered := w.buf
	w.buf = nil
	if _, err := w.gz.Write(buffered); err != nil {
		return 0, err
	}
	return len(b), nil
}

// startGzip commits to compressing: sets the encoding headers and opens the
// writer. Must run before the real WriteHeader, since headers are frozen after.
func (w *gzipResponseWriter) startGzip() {
	header := w.Header()
	header.Set("Content-Encoding", "gzip")
	// The byte count is about to change, so any declared length is now a lie —
	// and a wrong Content-Length is a truncated or hung response, not a
	// cosmetic error.
	header.Del("Content-Length")

	w.gz = gzipWriterPool.Get().(*gzip.Writer)
	w.gz.Reset(w.ResponseWriter)

	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(w.status)
}

// close finishes the response. Must run before the handler chain returns: a
// buffered body would otherwise never be written at all, and a gzip stream
// would never get its trailer.
func (w *gzipResponseWriter) close() {
	if w.gz != nil {
		_ = w.gz.Close()
		gzipWriterPool.Put(w.gz)
		w.gz = nil
		return
	}
	if w.passthrough {
		return
	}

	// Never reached the threshold: send it uncompressed. This is the path that
	// keeps a short error body from being inflated by a gzip header and
	// trailer that cost more than they save.
	if !w.wroteHeader {
		if w.status == 0 {
			w.status = http.StatusOK
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(w.buf)))
		w.wroteHeader = true
		w.ResponseWriter.WriteHeader(w.status)
	}
	if len(w.buf) > 0 {
		_, _ = w.ResponseWriter.Write(w.buf)
		w.buf = nil
	}
}

// Unwrap keeps http.ResponseController able to reach the real writer. See
// problemInterceptor.Unwrap for why a middleware that omits this silently
// strips capabilities from everything behind it.
func (w *gzipResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Compress wraps h so compressible responses are gzipped for clients that
// accept it.
func Compress(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Vary regardless of whether this particular response ends up
		// compressed: it advertises that the response *depends* on
		// Accept-Encoding, which is what stops a shared cache from serving a
		// gzipped body to a client that cannot decode it. Setting it only on
		// compressed responses is the classic way to poison a CDN.
		w.Header().Add("Vary", "Accept-Encoding")

		if !clientAcceptsGzip(r) {
			h.ServeHTTP(w, r)
			return
		}

		gw := &gzipResponseWriter{ResponseWriter: w}
		defer gw.close()
		h.ServeHTTP(gw, r)
	})
}
