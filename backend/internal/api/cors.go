package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// This API had no CORS headers at all, which silently excluded an entire class
// of consumer: any browser page calling it with fetch/XHR. The request reaches
// the server and is answered normally, but the browser then refuses to hand the
// response to the script, so the failure surfaces to a developer as an opaque
// console error rather than as anything this API said. A preflight was worse —
// OPTIONS matches no registered operation, so it answered 405 and the real
// request was never sent.
//
// That class of consumer is not hypothetical here. internal/api/ratelimit.go's
// User-Agent policy already reasons about "a browser page calling this API
// directly with fetch/XHR (the Fetch spec forbids script-set User-Agent)", so
// the design anticipated browser callers while the server made them impossible.
//
// Wildcard origin is the correct policy for this API rather than a lax default:
// every endpoint is public, unauthenticated, read-only CC0 data, and there are
// no cookies or credentials to protect. `Access-Control-Allow-Origin: *` is
// also incompatible with credentialed requests by specification, so it cannot
// later become an accidental credential leak — a browser refuses to send
// credentials to a wildcard origin at all.

// corsMaxAge is how long a browser may cache a preflight result. Long because
// the answer is a constant: this API's allowed methods and headers do not vary
// by origin, path, or time, so re-asking is pure latency. Chromium caps this at
// 2 hours regardless, which is what the value is chosen to match.
const corsMaxAge = 2 * time.Hour

// corsAllowMethods is every method any route serves. GET and HEAD come from the
// operations themselves; OPTIONS is here because the preflight handler below
// answers it.
const corsAllowMethods = "GET, HEAD, OPTIONS"

// corsExposeHeaders lists the response headers a browser script is allowed to
// read. Without this the fetch spec exposes only a handful of "simple" headers,
// and everything this API uses to communicate protocol state would be invisible
// to exactly the clients most likely to need it:
//
//   - ETag: a browser client cannot do conditional requests it cannot read the
//     tag for, so the whole If-None-Match story (api.go's checkNotModified)
//     silently degrades to full refetches.
//   - RateLimit-*: docs/API_POLICY.md tells consumers to back off using these.
//     A script that cannot read them cannot comply.
//   - Retry-After: same, for the 429 case specifically.
const corsExposeHeaders = "ETag, Cache-Control, RateLimit-Limit, RateLimit-Remaining, RateLimit-Reset, Retry-After"

// CORS wraps h with cross-origin headers and answers preflight requests.
//
// Mounted outside the mux (cmd/api-server/main.go) rather than as huma
// middleware for the same reason ProblemErrors is: a preflight OPTIONS matches
// no registered operation, so anything installed at the huma layer never runs
// for it and ServeMux answers 405 first.
func CORS(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Headers go on every response, not only cross-origin ones. Emitting
		// them unconditionally keeps a cached response valid for both same- and
		// cross-origin readers; varying by request would mean a CDN could serve
		// a cross-origin client a copy cached for a same-origin one, missing
		// the headers it needs — the classic CORS-behind-a-CDN failure.
		header := w.Header()
		header.Set("Access-Control-Allow-Origin", "*")
		header.Set("Access-Control-Expose-Headers", corsExposeHeaders)

		if r.Method != http.MethodOptions || r.Header.Get("Access-Control-Request-Method") == "" {
			h.ServeHTTP(w, r)
			return
		}

		// Preflight. Answered here and never passed down: OPTIONS matches no
		// operation, so forwarding it would produce a 405 that aborts the real
		// request the browser was asking permission for.
		header.Set("Access-Control-Allow-Methods", corsAllowMethods)
		header.Set("Access-Control-Max-Age", strconv.Itoa(int(corsMaxAge.Seconds())))

		// Echo whatever headers the client asked about rather than publishing a
		// fixed allow-list. The set a caller legitimately needs is open-ended —
		// If-None-Match today, a tracing or client-identification header
		// tomorrow — and a fixed list silently breaks each new one at the
		// browser, far from any code that could explain why. Echoing is safe
		// precisely because this API has no credentialed or state-changing
		// requests for a header to unlock.
		if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
			header.Set("Access-Control-Allow-Headers", req)
			// The response now depends on a request header, so it is not
			// interchangeable between callers asking for different header sets.
			header.Add("Vary", "Access-Control-Request-Headers")
		}

		w.WriteHeader(http.StatusNoContent)
	})
}

// corsVaryValues is used by tests to assert the Vary handling above; kept as a
// helper so the expectation lives next to the behavior it describes.
func corsVaryContains(vary, want string) bool {
	for _, v := range strings.Split(vary, ",") {
		if strings.EqualFold(strings.TrimSpace(v), want) {
			return true
		}
	}
	return false
}
