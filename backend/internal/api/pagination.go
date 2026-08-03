package api

// This file defines the one pagination contract every collection endpoint in
// this API uses, so a client that can page one of them can page all of them.
//
// Shape choice, and why it is what it is. A survey of the Go APIs this
// project already benchmarks its layout against (docs/CONVENTIONS.md) found
// no single dominant list envelope — Kubernetes uses
// {apiVersion, kind, metadata.continue, items}, Gitea {ok, data}, Nomad a
// bare JSON array with pagination in headers, Prometheus {status, data}.
// What they agree on is narrower and more useful: a *named* collection field
// and page metadata beside it, never a bare array (which can never grow a
// metadata field without breaking every client). Miniflux — the closest
// analog to this project, a small self-hosted Go service with a public API —
// uses exactly {total, entries}, which is what this API had already arrived
// at independently for /exercises.
//
// So: keep the semantic collection key ("exercises", "revisions", "terms"),
// and standardize everything around it. Offset pagination rather than
// Kubernetes-style cursors because every collection here is small, ordered by
// a stable key, and served from a CDN — a cursor's advantage (stability under
// concurrent insertion) costs opaque tokens that break the plain-URL caching
// this API's whole performance story rests on.
type Pagination struct {
	Total   int `json:"total" doc:"Total items across all pages, ignoring pagination"`
	Page    int `json:"page" doc:"1-based page number echoed back from the request"`
	PerPage int `json:"per_page" doc:"Items per page echoed back from the request"`
}

// PageParams is the request half of the same contract, embedded in every
// list endpoint's input struct. huma promotes anonymously-embedded fields
// when parsing query parameters and when generating the OpenAPI schema, the
// same mechanism conditional.Params already relies on, so embedding this
// documents the parameters once rather than per endpoint.
type PageParams struct {
	// Bounded above as well as below. `minimum:"1"` alone left Page accepting
	// any int64 huma could parse, and Offset multiplies it — `?page=` near
	// math.MaxInt64 wrapped to a *negative* product, which Postgres rejects
	// with "OFFSET must not be negative" and mapStoreError turns into a 500.
	// A public unauthenticated API where one query parameter reliably 500s
	// four endpoints is a denial-of-service primitive, not a validation gap,
	// so the ceiling is enforced twice: here, where huma answers a real
	// request with a 422 naming the parameter, and again in Normalize for
	// direct handler calls that bypass tag validation entirely.
	Page int `query:"page" minimum:"1" maximum:"100000" default:"1" doc:"1-based page number; maximum 100000"`
	// Capped at 100 on every endpoint, not just the expensive ones: this is a
	// public unauthenticated API, and a uniform ceiling is one number for a
	// consumer to remember rather than a per-endpoint lookup.
	PerPage int `query:"per_page" minimum:"1" maximum:"100" default:"50" doc:"Items per page; maximum 100"`
}

// defaultPerPage matches PageParams.PerPage's `default` tag. Both exist
// because huma applies the tag when a parameter is absent from the query
// string, but a handler called directly — which every test in this package
// does, per docs/CONVENTIONS.md section 3 — receives whatever zero value the
// caller left in the struct. Normalize is what makes those two paths agree.
const defaultPerPage = 50

// maxPerPage matches PageParams.PerPage's `maximum` tag, and is enforced
// again here for the same reason: huma rejects an over-cap value from a real
// request with a 422, but a direct handler call bypasses that validation
// entirely and would otherwise issue an unbounded query.
const maxPerPage = 100

// maxPage matches PageParams.Page's `maximum` tag. Its job is arithmetic
// rather than product policy: maxPage*maxPerPage is 10,000,000, so
// Offset's multiplication cannot overflow on any platform where int is at
// least 32 bits, which is what actually makes its "never negative" promise
// true. Nothing real pages this deep — 100,000 pages of the smallest useful
// size is already far past the largest collection this API serves — so the
// ceiling costs no legitimate caller anything.
const maxPage = 100_000

// Normalize returns the page and per-page values to actually query with,
// clamping anything out of range rather than erroring. Requests that reach a
// handler through huma have already been validated against the struct tags,
// so this only ever fires for direct calls; clamping keeps those callers on
// the same code path as real requests instead of introducing an error case
// that production can never hit.
func (p PageParams) Normalize() (page, perPage int) {
	page = p.Page
	switch {
	case page < 1:
		page = 1
	case page > maxPage:
		page = maxPage
	}
	perPage = p.PerPage
	switch {
	case perPage < 1:
		perPage = defaultPerPage
	case perPage > maxPerPage:
		perPage = maxPerPage
	}
	return page, perPage
}

// Offset is the SQL OFFSET for this page. Normalize bounds both factors —
// page to [1, maxPage] and perPage to [1, maxPerPage] — before they are
// multiplied, so the product is always in [0, maxPage*maxPerPage] and can
// neither overflow nor come out negative. Clamping the low end alone was not
// enough: an unbounded page wrapped int64 and produced a negative OFFSET.
func (p PageParams) Offset() int {
	page, perPage := p.Normalize()
	return (page - 1) * perPage
}

// paginationFor builds the response-side metadata from the request-side
// params plus a total, so the echoed page/per_page always match the values
// actually queried with rather than the raw request values.
func paginationFor(p PageParams, total int) Pagination {
	page, perPage := p.Normalize()
	return Pagination{Total: total, Page: page, PerPage: perPage}
}
