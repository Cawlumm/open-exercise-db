package api

import "strings"

// BCP-47 tags are case-insensitive (RFC 5646 §2.1.1: "the tags and their
// subtags... are not case sensitive"), but every locale lookup in this API is a
// literal string comparison against a database column, so case was load-bearing
// where the standard says it must not be. `?locale=EN` and `?locale=es-mx` are
// both perfectly valid tags that matched nothing and fell silently back to
// English — and silently is the problem: the response looked exactly like a
// successful request for a locale with no translations yet.
//
// Normalizing here, at the edge, rather than in the store: the canonical form
// is a property of the request, and doing it once on the way in means every
// downstream consumer — the translation lookup, the ETag, the requested_locale
// echoed back to the client — all agree on one spelling. Normalizing per query
// would leave the echoed value and the queried value free to drift.

// normalizeLocale returns the canonical form of a BCP-47 tag: language subtag
// lowercase, region subtag uppercase ("EN" -> "en", "es-mx" -> "es-MX"),
// surrounding whitespace removed.
//
// Deliberately shallow. Full BCP-47 canonicalization (script subtags,
// extensions, grandfathered tags, preferred-value substitution) is a
// golang.org/x/text dependency for cases this dataset has none of — every
// locale in it is a two-letter language with an optional region. What is
// handled here is what the query string actually receives: the wrong case, and
// stray whitespace from a hand-built URL.
func normalizeLocale(locale string) string {
	locale = strings.TrimSpace(locale)
	if locale == "" {
		return ""
	}

	lang, region, hasRegion := strings.Cut(locale, "-")
	if !hasRegion || lang == "" {
		// No region, or a leading "-" that means this is not a well-formed tag
		// at all. Lowercase it and hand it on rather than inventing structure:
		// the job here is canonicalizing case, not validating, and uppercasing
		// the tail of "-nonsense" would produce a value the caller never sent.
		return strings.ToLower(locale)
	}
	return strings.ToLower(lang) + "-" + strings.ToUpper(region)
}
