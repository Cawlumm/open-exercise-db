package api

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// bodyOf runs h and returns the response recorder plus the body as the client
// would see it — decompressed when the response says it is gzipped, so every
// assertion below is about what the caller actually receives.
func bodyOf(t *testing.T, h http.Handler, req *http.Request) (*httptest.ResponseRecorder, string) {
	t.Helper()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") != "gzip" {
		return rec, rec.Body.String()
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("response claims gzip but is not readable as gzip: %v", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gzip body: %v", err)
	}
	return rec, string(out)
}

// jsonHandler writes n bytes of JSON-ish body with the given content type.
func jsonHandler(contentType string, n int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write([]byte(`{"v":"` + strings.Repeat("a", n) + `"}`))
	})
}

func gzipRequest() *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/exercises", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	return r
}

// TestCompress_SmallResponsesAreNotInflated is the case the first version of
// this middleware got wrong. It read Content-Length at WriteHeader time, but
// net/http computes that itself *after* the handler returns, so the header was
// always absent, the size threshold never fired once, and a 130-byte
// /meta/locales response came back as 132 bytes of gzip — compression making
// the response larger, which is precisely what the threshold exists to prevent.
func TestCompress_SmallResponsesAreNotInflated(t *testing.T) {
	rec, body := bodyOf(t, Compress(jsonHandler("application/json", 32)), gzipRequest())

	if enc := rec.Header().Get("Content-Encoding"); enc != "" {
		t.Errorf("Content-Encoding = %q for a tiny body, want none", enc)
	}
	if rec.Body.Len() > len(body) {
		t.Errorf("wire size %d exceeds body size %d — the response was inflated",
			rec.Body.Len(), len(body))
	}
}

func TestCompress_LargeResponsesAreCompressed(t *testing.T) {
	rec, body := bodyOf(t, Compress(jsonHandler("application/json", 40_000)), gzipRequest())

	if enc := rec.Header().Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", enc)
	}
	if len(body) < 40_000 {
		t.Errorf("decompressed body is %d bytes, want the full payload back", len(body))
	}
	if rec.Body.Len() >= len(body) {
		t.Errorf("compressed size %d is not smaller than %d", rec.Body.Len(), len(body))
	}
	if cl := rec.Header().Get("Content-Length"); cl != "" {
		t.Errorf("Content-Length = %q on a compressed response; a stale length truncates or hangs the client", cl)
	}
}

// TestCompress_BodyIsUnchanged is the property that matters most: compression
// must be invisible. A middleware that drops or duplicates the buffered prefix
// when it crosses the threshold would still produce valid gzip.
func TestCompress_BodyIsUnchanged(t *testing.T) {
	for _, size := range []int{0, 1, compressMinBytes - 1, compressMinBytes, compressMinBytes + 1, 100_000} {
		want := `{"v":"` + strings.Repeat("a", size) + `"}`

		_, got := bodyOf(t, Compress(jsonHandler("application/json", size)), gzipRequest())
		if got != want {
			t.Errorf("size %d: body round-tripped to %d bytes, want %d", size, len(got), len(want))
		}
	}
}

// TestCompress_ManyWritesAcrossThreshold covers the handoff a single-Write test
// cannot: bytes buffered before the threshold must be flushed into the gzip
// stream ahead of everything after it, in order and exactly once.
func TestCompress_ManyWritesAcrossThreshold(t *testing.T) {
	const chunks = 500
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		for i := range chunks {
			_, _ = w.Write([]byte(string(rune('a' + i%26))))
		}
	})

	var want strings.Builder
	for i := range chunks {
		want.WriteRune(rune('a' + i%26))
	}

	_, got := bodyOf(t, Compress(h), gzipRequest())
	if got != want.String() {
		t.Errorf("multi-write body mismatch\n got %d bytes\nwant %d bytes", len(got), want.Len())
	}
}

func TestCompress_SkipsIncompressibleAndBodilessResponses(t *testing.T) {
	cases := []struct {
		name    string
		handler http.Handler
	}{
		{"jpeg is already compressed", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(make([]byte, 50_000))
		})},
		{"304 has no body", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotModified)
		})},
		{"204 has no body", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			Compress(c.handler).ServeHTTP(rec, gzipRequest())
			if enc := rec.Header().Get("Content-Encoding"); enc != "" {
				t.Errorf("Content-Encoding = %q, want none", enc)
			}
		})
	}
}

func TestCompress_HonoursClientPreference(t *testing.T) {
	cases := map[string]bool{
		"":                    false,
		"gzip":                true,
		"GZIP":                true,
		"gzip;q=0":            false, // explicit refusal
		"br, gzip":            true,
		"identity":            false,
		"deflate":             false,
		"gzip;q=0.5, deflate": true,
	}

	for accept, wantGzip := range cases {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/exercises", nil)
		if accept != "" {
			req.Header.Set("Accept-Encoding", accept)
		}

		rec := httptest.NewRecorder()
		Compress(jsonHandler("application/json", 40_000)).ServeHTTP(rec, req)

		gotGzip := rec.Header().Get("Content-Encoding") == "gzip"
		if gotGzip != wantGzip {
			t.Errorf("Accept-Encoding %q: gzip=%v, want %v", accept, gotGzip, wantGzip)
		}
	}
}

// TestCompress_AlwaysVaries guards a cache-poisoning shape rather than a
// correctness one: Vary must be present even when this particular response was
// not compressed, or a shared cache can store an uncompressed copy under a key
// that a gzip-only client later matches — or the reverse, handing a gzipped
// body to a client that cannot decode it.
func TestCompress_AlwaysVaries(t *testing.T) {
	for _, accept := range []string{"", "gzip"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/exercises", nil)
		if accept != "" {
			req.Header.Set("Accept-Encoding", accept)
		}

		rec := httptest.NewRecorder()
		Compress(jsonHandler("application/json", 10)).ServeHTTP(rec, req)

		if !corsVaryContains(rec.Header().Get("Vary"), "Accept-Encoding") {
			t.Errorf("Accept-Encoding %q: Vary = %q, must include Accept-Encoding",
				accept, rec.Header().Get("Vary"))
		}
	}
}
