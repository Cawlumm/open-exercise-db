package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/conditional"
	"github.com/jackc/pgx/v5"

	"github.com/Cawlumm/open-exercise-db/internal/export"
	"github.com/Cawlumm/open-exercise-db/internal/media"
	"github.com/Cawlumm/open-exercise-db/internal/ratelimit"
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// cacheControl is applied to every GET response (docs/DESIGN.md ->
// Performance: "Cache-Control/ETag on every GET"). This dataset changes
// rarely and there's no cache-purge mechanism in Phase 1 (that arrives with
// /export's R2 write-through in Phase 2), so a moderate max-age plus a long
// stale-while-revalidate window is what actually saves origin hits: repeat
// reads from many independent consumers land on the Cloudflare edge cache,
// not this process, without risking a stale response outliving a real edit
// for more than a few minutes.
const cacheControl = "public, max-age=300, stale-while-revalidate=86400"

// Register wires every Phase 1 read operation (docs/PHASE1_BUILD_PLAN.md
// WO-3 and WO-5) onto api against the given stores. exportCache, limiter,
// and mediaStore are all constructed and owned by cmd/api-server/main.go,
// matching docs/CONVENTIONS.md section 2's DI pattern (constructors wired
// in main, passed in). Called once from main; main itself stays thin.
//
// mediaStore backs GET /api/v1/images/{id} (internal/api/images.go) — the
// same media.Store implementation (filesystem in Phase 1, R2 in Phase 2)
// cmd/import-freeexercisedb writes images to. imageBaseURL is
// internal/config.Config.ImageBaseURL, threaded down to
// registerExerciseRoutes/registerExportRoutes so GET /exercises, GET
// /exercises/{id}, and GET /exercises/export can resolve
// contract.Exercise.Images[].url — see internal/media.ImageURL for what it
// controls.
//
// limiter's middleware (internal/api/ratelimit.go) is installed via
// hapi.UseMiddleware before any route is registered, so it runs ahead of
// every operation below — docs/PHASE1_BUILD_PLAN.md WO-7's rate limiting
// and User-Agent policy apply uniformly to every GET this package exposes,
// not opted into per-route.
func Register(hapi huma.API, s *store.Store, exportCache *export.Cache, limiter *ratelimit.Limiter, mediaStore media.Store, imageBaseURL string, authCfg AuthConfig) {
	// Rate limiting first. Putting the NUL check ahead of it looked like a
	// courtesy — reject malformed input without spending the caller's budget —
	// but it created an unmetered request path: a client looping on
	// NUL-containing query strings was rejected before the limiter ever saw it,
	// so the requests were free. It also stripped the RateLimit-* headers from
	// that one error response, making it the only 4xx a well-behaved consumer
	// could not read its remaining budget from.
	hapi.UseMiddleware(rateLimitMiddleware(hapi, limiter))
	hapi.UseMiddleware(rejectNulBytesMiddleware(hapi))

	// App authentication runs after rate limiting, deliberately. An
	// unauthenticated flood should be rejected by the limiter without ever
	// reaching a database lookup — putting auth first would make every
	// discarded request cost a query on apps, turning the credential check
	// into the cheapest available amplification vector.
	hapi.UseMiddleware(appAuthMiddleware(hapi, s, authCfg))

	registerSubmissionRoutes(hapi, s)
	registerAuthRoutes(hapi, s, authCfg)
	registerAdminAppRoutes(hapi, s)
	registerModerationRoutes(hapi, s)

	// registerExportRoutes intentionally comes before registerExerciseRoutes.
	// GET /api/v1/exercises/export (a static, literal path) and GET
	// /api/v1/exercises/{id} (a wildcard on the same path depth) would
	// otherwise be ambiguous for a router that resolves conflicts by
	// registration order — and one does: humatest's default adapter
	// (adapters/humaflow/flow, used by internal/api/ratelimit_test.go's
	// full-Register HTTP tests, the first tests in this package to
	// exercise both route groups together over real HTTP — every earlier
	// test called registerExerciseRoutes/registerExportRoutes separately
	// on its own throwaway humatest.API and so never hit this) matches
	// registered patterns in order, so registering {id} first made every
	// request for literal "export" resolve to get-exercise with id="export"
	// instead of export-exercises, a 422 (invalid UUID) instead of the
	// export blob. Production (cmd/api-server/main.go's humago adapter over
	// Go 1.22+'s stdlib http.ServeMux) doesn't have this failure mode —
	// ServeMux resolves a literal segment over a wildcard at the same depth
	// regardless of registration order, verified directly against
	// http.ServeMux — but relying on that being true of every router this
	// project might ever route through (including test doubles) is exactly
	// the kind of order-dependency that's cheap to just not have. Register
	// the more specific path first and neither router's resolution rule
	// matters.
	registerExportRoutes(hapi, s, exportCache)
	registerExerciseRoutes(hapi, s, imageBaseURL)
	registerMetaRoutes(hapi, s)
	registerImageRoutes(hapi, s, mediaStore)
}

// mapStoreError translates a store-layer error into a typed huma error, per
// docs/CONVENTIONS.md section 4/5: the store stays HTTP-unaware and returns
// pgx.ErrNoRows upward; the API layer is what knows that means 404. Any
// other error is logged server-side with full detail and replaced with an
// opaque 500 — raw DB errors never reach a client.
func mapStoreError(err error, notFoundMsg string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return huma.Error404NotFound(notFoundMsg)
	}
	log.Printf("store error: %v", err)
	return huma.Error500InternalServerError("internal error")
}

// etagValue computes a strong-comparison-capable content hash from body's
// JSON encoding, in the *bare* (unquoted) form conditional.PreconditionFailed
// expects as the "current" resource tag — it compares against this directly,
// while stripping quotes/W- prefixes only from the *incoming* header values
// (see huma's conditional/params.go trimETag). etagHeader below adds the
// quoting RFC 9110 §8.8.3 requires for the value actually sent on the wire.
//
// Double marshal, explicitly not optimized away: huma will marshal the same
// Body value again itself once this handler returns a non-error output —
// there's no per-operation hook to compute a tag from bytes huma has already
// produced. Two real hooks do exist for an output's Body field (verified
// against huma v2.39.1's source, huma.go ~line 1282): a `[]byte` field is
// written raw via ctx.BodyWriter() instead of being run through
// api.Transform/Marshal, and a `func(Context)` field hands the handler full
// control. Both were rejected here: `[]byte` falls into schema.go's generic
// byte-slice case (confirmed at schema.go:861-864) and gets documented in
// the OpenAPI spec as an opaque base64 string instead of the real object
// shape — unacceptable for these typed, per-item/per-page responses, where
// an accurate generated schema is the whole point of huma. `func(Context)`
// forfeits huma's status/content-negotiation handling for a bigger rewrite
// than this defect warrants. For WO-3's payload sizes (one exercise, a page
// of <=100, or a short taxonomy list) the second marshal costs
// microseconds, so it's left in place rather than traded for either of
// those. WO-5's /export is the case where this tradeoff should be revisited
// — but the fix there isn't this hook, it's that docs/PHASE1_BUILD_PLAN.md
// already specs /export as built once and cached in memory, so its JSON
// only gets marshaled once *ever*, not once per request; per-request
// double-marshal stops being the right frame entirely once that cache
// exists.
func etagValue(body any) string {
	b, err := json.Marshal(body)
	if err != nil {
		// body is always one of this package's own response structs, so a
		// marshal failure here would mean a genuinely broken type — not a
		// runtime condition worth surfacing to the client as an error, since
		// the same struct is about to be marshaled again by huma itself.
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

// etagHeader quotes tag for the wire, per RFC 9110 §8.8.3's entity-tag
// grammar.
func etagHeader(tag string) string {
	return `"` + tag + `"`
}

// etagValueBytes mirrors etagValue for a raw byte body instead of a
// JSON-marshaled struct — GET /api/v1/images/{id} (internal/api/images.go)
// has no JSON body to hash, just the image bytes themselves. Same truncated
// sha256-hex convention as etagValue, so both share one quoting/comparison
// path (etagHeader, checkNotModified) with no special-casing for the binary
// response.
func etagValueBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

// checkNotModified implements RFC 9110 §13.1.2 conditional-GET
// short-circuiting for every handler in this package: p is the request's
// parsed conditional headers (every Input struct embeds conditional.Params,
// which huma auto-populates from If-Match/If-None-Match/If-Modified-Since/
// If-Unmodified-Since and auto-resolves before the handler runs), and tag is
// the bare ETag just computed for the response body that's about to be
// returned. On a match it returns a 304 error carrying the same
// Cache-Control/ETag headers a 200 would have had — RFC 9110 §15.4.5
// requires a 304 to still report the resource's current metadata, since
// that's how the client learns what to send on its *next* revalidation.
//
// cc is the Cache-Control value to echo on that 304 — parameterized (rather
// than reading the package-level cacheControl constant directly) because
// WO-5's /exercises/export uses a deliberately longer value than every other
// GET in this package (see exportCacheControl in export.go); every other
// caller here still just passes cacheControl.
//
// The actual comparison — weak per RFC 9110 §8.8.3.2 (right for this case:
// these are read-only GETs comparing full-body hashes, not byte-for-byte
// write preconditions), If-None-Match's comma-separated list, and the `*`
// wildcard — is delegated entirely to huma's own conditional package rather
// than hand-rolled, so it can't drift from what huma itself documents in the
// generated spec for these header params.
func checkNotModified(p *conditional.Params, tag, cc string) error {
	// Workaround for a huma v2.39.1 bug, not a fix to our own logic: RFC 9110
	// §5.6.1's list-header grammar is `#element = [element] *( OWS ","
	// OWS [element] )` — optional whitespace (OWS) is explicitly legal
	// around each comma. huma splits If-Match/If-None-Match on "," (huma.go's
	// parseInto, non-Explode reflect.Slice case) but never strips that OWS
	// back off, so any list element after the first that has a leading space
	// (e.g. `If-None-Match: "a", "b"`, which every RFC-conformant sender is
	// entitled to produce, and which a CDN in front of this API — see
	// docs/DESIGN.md's Performance section — is a realistic source of) still
	// carries that space when it reaches conditional.trimETag
	// (conditional/params.go:28). trimETag only strips a `W/` prefix and `"`
	// characters via strings.Trim, which stops at the very first character
	// that isn't in the cutset — a leading space — so the entry never gets
	// unquoted and can never equal a bare tag. Trimming OWS from each element
	// here, before PreconditionFailed's own comparison runs, fixes it without
	// forking or reimplementing that comparison logic. Filed upstream as
	// https://github.com/danielgtaylor/huma/issues/1084 (details in
	// docs/huma-if-none-match-ows-bug.md); delete this block once we're on a
	// huma release that includes the fix.
	p.IfMatch = trimListOWS(p.IfMatch)
	p.IfNoneMatch = trimListOWS(p.IfNoneMatch)

	err := p.PreconditionFailed(tag, time.Time{})
	if err == nil {
		return nil
	}
	return huma.ErrorWithHeaders(err, http.Header{
		"Cache-Control": {cc},
		"ETag":          {etagHeader(tag)},
	})
}

// etagMatches reports whether any entity-tag in an If-None-Match header value
// matches tag, which is quoted as it appears on the wire.
//
// Exists for handlers that sit outside huma and so cannot use
// checkNotModified — IndexHandler is the only one today. It implements the
// same RFC 9110 §13.1.2 rules that package handles for free: the `*` wildcard,
// comma-separated candidate lists, optional whitespace around the commas, and
// weak comparison (a `W/` prefix is stripped rather than treated as a
// mismatch, which is correct here because these are content hashes of
// read-only representations, not byte-for-byte write preconditions).
//
// Takes the header's values as a slice so a request carrying If-None-Match
// more than once — legal, and what some proxies emit instead of one joined
// list — is handled the same as a single combined header.
func etagMatches(headerValues []string, tag string) bool {
	bare := strings.Trim(tag, `"`)

	for _, value := range headerValues {
		for _, candidate := range trimListOWS(strings.Split(value, ",")) {
			if candidate == "*" {
				return true
			}
			candidate = strings.TrimPrefix(candidate, "W/")
			if strings.Trim(candidate, `"`) == bare {
				return true
			}
		}
	}
	return false
}

// trimListOWS strips RFC 9110 optional whitespace (spaces and tabs) from
// each element of an already comma-split list header value. See
// checkNotModified's comment for why this exists.
func trimListOWS(values []string) []string {
	if values == nil {
		return nil
	}
	trimmed := make([]string, len(values))
	for i, v := range values {
		trimmed[i] = strings.Trim(v, " \t")
	}
	return trimmed
}
