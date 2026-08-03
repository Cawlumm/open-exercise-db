package api

import (
	"encoding/json"
	"net/http"
)

// The three URLs a person tries first — `/`, `/api`, `/api/v1` — were all dead
// ends. They answered a correct problem+json 404 pointing at /docs, which is
// better than net/http's plain text, but it is still "no" to the only question
// a newcomer can ask before they know anything: what is here?
//
// Every discoverable API answers this. GitHub's root hands back a map of URL
// templates; Kubernetes serves /api and /apis as version discovery documents.
// The cost is one static handler and the benefit is that curl-ing the bare host
// — which is what someone does with a URL they were handed — teaches them the
// shape of the API instead of telling them they are lost.
//
// Served as JSON rather than redirecting to /docs: the caller reaching this is
// usually a program or a person with curl, and a machine-readable index is
// useful to both, where an HTML docs page is only useful to one. The docs URL
// is included so a human gets there in one more hop.

// indexDocument is the API's discovery response. A plain struct rather than a
// map so field order in the JSON is stable — a response that reshuffles between
// requests defeats the ETag on it and reads as noise in a diff.
type indexDocument struct {
	Name          string            `json:"name"`
	Description   string            `json:"description"`
	Version       string            `json:"version"`
	Documentation string            `json:"documentation"`
	OpenAPI       string            `json:"openapi"`
	License       map[string]string `json:"license"`
	Endpoints     []indexEndpoint   `json:"endpoints"`
	Notes         []string          `json:"notes"`
}

type indexEndpoint struct {
	Path        string `json:"path"`
	Description string `json:"description"`
}

// IndexHandler serves the discovery document.
//
// Mounted on the exact-match pattern "GET /{$}" rather than "GET /": the bare
// pattern is a prefix wildcard that matches every path, which would make Go's
// ServeMux stop returning 405 for wrong-verb requests to real routes — the
// regression internal/api/errors.go documents at length. `{$}` matches only
// "/", verified against this Go version, so 404 and 405 both still work.
func IndexHandler(apiVersion string) http.Handler {
	doc := indexDocument{
		Name:          "open-exercise-db",
		Description:   "A community-maintained open database of exercises. Public, read-only, no authentication required.",
		Version:       apiVersion,
		Documentation: "/docs",
		OpenAPI:       "/api/v1/openapi.json",
		License: map[string]string{
			"data": "CC0-1.0",
			"code": "MIT",
		},
		Endpoints: []indexEndpoint{
			{"/api/v1/exercises", "List and search exercises. Supports q, slug, muscle_group, equipment, category, mechanic, level, locale, page, per_page."},
			{"/api/v1/exercises/{id}", "One exercise by UUID."},
			{"/api/v1/exercises/{id}/revisions", "An exercise's append-only revision history."},
			{"/api/v1/exercises/{id}/translations", "Which locales an exercise is translated into."},
			{"/api/v1/exercises/export", "The complete dataset in one response, for mirroring."},
			{"/api/v1/images/{id}", "Image bytes. Every exercise response carries fetchable image URLs."},
			{"/api/v1/meta/muscle-groups", "Valid muscle_group filter values."},
			{"/api/v1/meta/equipment", "Valid equipment filter values."},
			{"/api/v1/meta/categories", "Valid category filter values."},
			{"/api/v1/meta/locales", "Locales with translated content."},
			{"/health", "Liveness check."},
		},
		Notes: []string{
			"Set a User-Agent of the form \"AppName/Version (contact)\" — unidentified clients share a much smaller rate-limit budget.",
			"Unknown query parameters and unknown filter values are rejected rather than ignored, so a typo fails loudly instead of returning a wrong result.",
			"Every collection returns {<name>: [...], total, page, per_page}. Errors are RFC 9457 problem+json; switch on the \"type\" URI, not the prose.",
			"Responses carry ETag and Cache-Control; send If-None-Match to revalidate cheaply.",
		},
	}

	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		// Encoding a static struct with no cycles or unsupported types cannot
		// fail; if it somehow does, that is a programming error worth surfacing
		// at startup rather than serving a broken index for the process's life.
		panic("api: encoding index document: " + err.Error())
	}
	body = append(body, '\n')

	tag := etagHeader(etagValueBytes(body))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The index changes only when the binary does, so it is safe to cache
		// hard — and it is the one response most likely to be fetched by
		// someone hammering the host while exploring.
		w.Header().Set("Cache-Control", cacheControl)
		w.Header().Set("ETag", tag)

		if match := r.Header.Get("If-None-Match"); match != "" && match == tag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write(body)
	})
}
