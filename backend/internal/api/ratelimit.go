package api

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/Cawlumm/open-exercise-db/internal/ratelimit"
)

// rateLimitTierKey is the huma.Operation.Metadata key every rate-limited
// operation in this package sets to a ratelimit.Tier — see exercises.go/
// meta.go/export.go's huma.Register calls. rateLimitMiddleware reads it
// once per request via ctx.Operation().Metadata.
const rateLimitTierKey = "oedb-rate-limit-tier"

// fallbackTier is applied — and logged — when an operation reaches the
// middleware without a rateLimitTierKey entry in its Metadata. That should
// never happen for anything registered in this package today (every GET
// operation tags itself), so hitting this path means a future route was
// added without tagging it. Failing closed to the strictest configured
// cost tier is the deliberate choice for a public rate limiter: silently
// exempting an untagged route by defaulting to the generous read tier
// would be a "quiet success on the wrong path" bug in the request path,
// the same failure mode docs/CONVENTIONS.md section 6 calls out for a
// Postgres test that skips instead of failing — just here it would ship
// silently instead of merely passing CI silently.
const fallbackTier = ratelimit.TierExport

// uaFormatPattern approximates the User-Agent shape
// docs/PHASE1_BUILD_PLAN.md WO-7's table requires, mirroring Open Food
// Facts: "AppName/Version (ContactEmail)" — a slash-separated app/version
// token followed by a parenthesized contact. Deliberately loose about what
// goes inside the parens (a URL or a project name is as legitimate contact
// info as an email address): the thing being enforced is "identifies
// itself and gives the operator a way to reach it," not one specific
// contact format.
var uaFormatPattern = regexp.MustCompile(`^\S+/\S+\s+\([^()]+\)$`)

// genericUAPrefixes are default User-Agent strings from HTTP client
// libraries and browsers — technically non-empty, but they identify a
// library or browser engine, not the consumer using it, so they carry the
// same "who is this" information as no header at all. See isIdentifiedUA.
var genericUAPrefixes = []string{
	"mozilla/", "curl/", "wget/", "python-requests/", "go-http-client/",
	"okhttp/", "postmanruntime/", "insomnia/", "axios/", "node-fetch/",
	"java/", "apache-httpclient/", "libwww-perl/", "scrapy/",
	"python-urllib/", "httpie/", "libcurl/", "restsharp/", "guzzlehttp/",
}

// clientIDHeader is the browser-usable alternative to User-Agent.
//
// The User-Agent policy is unenforceable for exactly the consumer this API
// most wants: a web app calling it from the page. `User-Agent` is a forbidden
// header name in the Fetch spec, so script cannot set it — the browser's own
// string is sent instead, that string starts with "Mozilla/", and
// genericUAPrefixes correctly classifies it as unidentified. The result was
// that a browser client was capped at the Unidentified tier permanently, with
// no action available to its author. The policy said "identify yourself" and
// the platform said "you may not".
//
// So identification accepts this header as an equal alternative, in the same
// `AppName/Version (ContactEmail)` form. Nothing about the trust model changes:
// a User-Agent was always client-supplied and unverified, and this is the same
// claim carried in a header the browser actually permits. It needs no CORS
// allow-list entry because the preflight handler (cors.go) echoes whatever
// headers the caller asks for, which its comment anticipated for precisely
// this case.
const clientIDHeader = "X-OEDB-Client"

// isIdentifiedClient reports whether a request carries a usable identity, from
// either identification channel. X-OEDB-Client wins when both are present: a
// browser cannot suppress its own User-Agent, so requiring UA to be absent
// would make the header unusable in the one place it exists to serve.
func isIdentifiedClient(ctx huma.Context) bool {
	if isIdentifiedUA(ctx.Header(clientIDHeader)) {
		return true
	}
	return isIdentifiedUA(ctx.Header("User-Agent"))
}

// isIdentifiedUA reports whether ua satisfies docs/PHASE1_BUILD_PLAN.md
// WO-7's User-Agent policy, as documented publicly in docs/API_POLICY.md.
//
// Strictness decision: neither hard-rejecting a missing/generic UA nor
// silently accepting one at full tier limits. Rejecting outright (e.g. a
// 400/403 on every request lacking the format) would strand a naive
// consumer — a first-time contributor's curl one-liner, a homelab script —
// with a broken integration and no way to discover why beyond a bare
// error: this is a zero-auth Phase 1 API with no account/dashboard to
// surface the rule through, and docs/PHASE1_BUILD_PLAN.md WO-7 explicitly
// warns that being too strict here "breaks naive consumers with no
// recourse." Accepting a generic UA at the operation's normal tier limit
// would make the requirement purely decorative — indistinguishable from
// not having one, which WO-7 warns about with equal weight ("too lax makes
// the rule pointless").
//
// The middle path implemented here: an unidentified caller still works,
// but rateLimitMiddleware routes it to a single, sharply reduced
// ratelimit.Unidentified budget instead of its endpoint's normal cost
// tier. That's enough to explore the API, read the docs, and discover the
// rule via a 429 carrying Retry-After — not enough to sustain any real
// traffic anonymously. A consumer that cannot set a custom header at all
// (a browser page calling this API directly with fetch/XHR — the Fetch
// spec forbids script-set User-Agent) is expected to proxy through its own
// backend, the same guidance Open Food Facts gives; documented in
// docs/API_POLICY.md rather than solved by relaxing this check.
func isIdentifiedUA(ua string) bool {
	trimmed := strings.TrimSpace(ua)
	if trimmed == "" {
		return false
	}
	lower := strings.ToLower(trimmed)
	for _, prefix := range genericUAPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return false
		}
	}
	return uaFormatPattern.MatchString(trimmed)
}

// clientKey extracts the identity internal/ratelimit.Limiter buckets on.
// Cloudflare sits in front of the origin in every real deployment
// (docs/DESIGN.md → Hosting) and forwards the real client address in
// CF-Connecting-IP; without honoring that, this process would only ever
// see Cloudflare's own edge IP as the TCP peer and bucket every consumer
// on earth into one counter, which would make the origin backstop
// actively harmful rather than merely coarse. X-Forwarded-For is the
// generic fallback for any other reverse proxy in the chain (or a
// self-hosted deployment without Cloudflare in front); ctx.RemoteAddr() is
// the last resort for a direct connection (local dev, docker compose).
//
// Trusting a client-supplied header for identity would normally be a
// spoofing risk, but it isn't one that matters for what this limiter is:
// internal/ratelimit's package doc is explicit that this is a coarse
// per-instance backstop, not the source of truth for enforcement. A client
// that spoofs CF-Connecting-IP to dodge the origin's bucketing still has
// to get past Cloudflare's own edge rule first (docs/API_POLICY.md), which
// reads the real TCP-level connection, not a header a client controls.
func clientKey(ctx huma.Context) string {
	if v := strings.TrimSpace(ctx.Header("CF-Connecting-IP")); v != "" {
		return v
	}
	if v := ctx.Header("X-Forwarded-For"); v != "" {
		if first := strings.TrimSpace(strings.SplitN(v, ",", 2)[0]); first != "" {
			return first
		}
	}
	if host, _, err := net.SplitHostPort(ctx.RemoteAddr()); err == nil && host != "" {
		return host
	}
	return ctx.RemoteAddr()
}

// rateLimitMiddleware enforces limiter's per-tier, per-client buckets on
// every operation registered by this package, and applies
// docs/API_POLICY.md's User-Agent requirement (via isIdentifiedUA) to
// decide whether an operation's normal cost tier or the strict
// ratelimit.Unidentified tier applies. Registered once via
// hapi.UseMiddleware from Register (api.go), so it runs ahead of every
// operation's handler on that huma.API instance.
//
// RateLimit-Limit/Remaining/Reset are set on every response this
// middleware handles, not only a 429. These header names follow the
// widely-deployed draft-ietf-httpapi-ratelimit-headers convention (the
// same one docs/PHASE1_BUILD_PLAN.md WO-7 specifies), not RFC 9331 — see
// docs/API_POLICY.md's "Standards note" for the citation correction: RFC
// 9331 is an unrelated congestion-control RFC (L4S/ECN), not a rate-limit
// header spec, and WO-7's original citation was wrong. A denied request
// additionally gets Retry-After (RFC 9110 §10.2.3, a real, unrelated-draft
// standard header) and never reaches the handler.
func rateLimitMiddleware(hapi huma.API, limiter *ratelimit.Limiter) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		op := ctx.Operation()
		tier, ok := op.Metadata[rateLimitTierKey].(ratelimit.Tier)
		if !ok {
			log.Printf("rate limit: operation %s has no %s metadata tag; applying %s as a fail-closed default", op.OperationID, rateLimitTierKey, fallbackTier)
			tier = fallbackTier
		}

		key := clientKey(ctx)
		identified := isIdentifiedClient(ctx)
		decision := limiter.Allow(key, tier, identified)

		ctx.SetHeader("RateLimit-Limit", strconv.Itoa(decision.Limit))
		ctx.SetHeader("RateLimit-Remaining", strconv.Itoa(decision.Remaining))
		ctx.SetHeader("RateLimit-Reset", strconv.Itoa(decision.ResetSeconds))

		if !decision.Allowed {
			ctx.SetHeader("Retry-After", strconv.Itoa(decision.RetryAfterSeconds))
			huma.WriteErr(hapi, ctx, http.StatusTooManyRequests, rateLimitMessage(decision, identified))
			return
		}

		next(ctx)
	}
}

// rateLimitMessage is the 429 body's human-readable detail. Distinguishing
// the unidentified case is deliberate — it's the single most actionable
// thing this message can tell a caller who might not otherwise know a
// User-Agent requirement exists at all.
func rateLimitMessage(d ratelimit.Decision, identified bool) string {
	if !identified {
		return fmt.Sprintf(
			"rate limit exceeded for unidentified clients (%d req/min budget); identify yourself in the form \"AppName/Version (ContactEmail)\" via the User-Agent header, or via X-OEDB-Client if you are calling from a browser (script cannot set User-Agent) — see docs/API_POLICY.md",
			d.Limit,
		)
	}
	return fmt.Sprintf(
		"rate limit exceeded for the %s tier (%d req/min); see docs/API_POLICY.md for limits and how to request more",
		d.EffectiveTier, d.Limit,
	)
}
