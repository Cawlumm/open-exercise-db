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
	Page int `query:"page" minimum:"1" default:"1" doc:"1-based page number"`
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

// Normalize returns the page and per-page values to actually query with,
// clamping anything out of range rather than erroring. Requests that reach a
// handler through huma have already been validated against the struct tags,
// so this only ever fires for direct calls; clamping keeps those callers on
// the same code path as real requests instead of introducing an error case
// that production can never hit.
func (p PageParams) Normalize() (page, perPage int) {
	page = p.Page
	if page < 1 {
		page = 1
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

// Offset is the SQL OFFSET for this page. Paired with Normalize's clamping so
// it can never return a negative offset.
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
