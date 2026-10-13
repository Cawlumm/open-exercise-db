package api

import (
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"
)

// Postgres text values cannot contain U+0000 — the wire protocol has no way to
// represent one, so pgx rejects the query rather than the database rejecting the
// value. That surfaced as a 500: `?q=a%00b` reached the store, failed at the
// driver, and mapStoreError reported it as "internal error". It is nothing of
// the sort. The request is malformed, the server is fine, and a 500 both
// misleads the caller and pollutes the error-rate signal that is supposed to
// mean "this service is broken".
//
// Checked as middleware over the raw query string rather than per-parameter in
// each handler, for the same reason huma's own unknown-parameter rejection is
// central: a per-field check covers the fields someone remembered, and the next
// string parameter added would quietly reintroduce the 500. Anything that
// reaches a handler from here on is free of NULs regardless of which parameter
// carried it.
//
// Deliberately rejecting rather than stripping. A caller who sent a NUL did not
// mean to, and silently searching for something adjacent to what they asked for
// is the same class of confidently-wrong answer as the unescaped LIKE wildcards
// this was found alongside.

// The same holds for bytes that are not UTF-8: Postgres rejects them as an
// "invalid byte sequence" at execution, so `?q=extensi%F3n` (a Latin-1 ó, which
// a client with the wrong encoding sends) reached the database and came back as
// the same misleading 500.

// rejectBadTextMiddleware answers 422 for any request whose query string
// contains a NUL byte or a value that is not valid UTF-8.
func rejectBadTextMiddleware(hapi huma.API) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		u := ctx.URL()
		// RawQuery, not the parsed values: a NUL can arrive percent-encoded
		// (%00) or raw, and checking the decoded values would mean parsing the
		// query twice and missing whichever form huma's own parse rejected
		// first.
		if strings.ContainsRune(u.RawQuery, 0) || strings.Contains(u.RawQuery, "%00") {
			huma.WriteErr(hapi, ctx, http.StatusUnprocessableEntity,
				"query string contains a NUL byte (U+0000), which cannot appear in a text value")
			return
		}
		if !validQueryText(u.RawQuery) {
			huma.WriteErr(hapi, ctx, http.StatusUnprocessableEntity,
				"query string is not valid UTF-8; percent-encode text as UTF-8")
			return
		}
		next(ctx)
	}
}

// validQueryText reports whether every decoded key and value is valid UTF-8.
// A query that does not parse is left for huma to reject.
func validQueryText(rawQuery string) bool {
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return true
	}
	for k, vs := range values {
		if !utf8.ValidString(k) {
			return false
		}
		for _, v := range vs {
			if !utf8.ValidString(v) {
				return false
			}
		}
	}
	return true
}
