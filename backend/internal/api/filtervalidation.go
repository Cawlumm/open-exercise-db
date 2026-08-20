package api

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// A taxonomy filter naming a term that does not exist used to answer 200 with
// an empty list — indistinguishable from a filter that is spelled correctly and
// legitimately matches nothing. That ambiguity falls entirely on the caller:
// `?muscle_group=Chest` (right idea, wrong case), `?muscle_group=chests`
// (plural guess) and `?muscle_group=banana` (nonsense) all looked exactly like
// a real empty result, so a client had no way to tell "no data yet" from "your
// request is wrong" and would reasonably conclude the dataset was empty.
//
// Deliberately a 422 rather than a warning alongside an empty list. These are
// closed vocabularies published by GET /meta/* specifically so a client can
// discover the valid values, and a wrong one is the same class of mistake as
// `per_page=0` — a malformed request, not a request with no answer. Answering
// 200 would also make the failure invisible to exactly the clients that check
// only the status code.

// filterParam pairs the query parameter a caller typed with the taxonomy facet
// it filters on. The two names differ (`muscle_group` the parameter,
// `muscle_group` the facet, but `category` vs `category`, `level` vs `level`),
// so the error can quote the parameter the caller actually wrote while the
// lookup uses the facet the database stores.
type filterParam struct {
	param string
	facet string
	value string
	// metaPath is the endpoint listing this facet's valid values, named in the
	// error so the fix is one documented request away rather than a search
	// through /docs. Empty for facets with no /meta route of their own.
	metaPath string
}

// taxonomyFiltersOf collects the non-empty taxonomy filters on a list request.
func taxonomyFiltersOf(input *ListExercisesInput) []filterParam {
	// One entry per value, not per parameter: every value a caller supplied is
	// validated, so ?muscle_group=chest,banana names banana rather than passing
	// because its first value happened to be real.
	groups := []struct {
		param, facet, metaPath string
		values                 []string
	}{
		{"muscle_group", "muscle_group", "/api/v1/meta/muscle-groups", input.MuscleGroup},
		{"equipment", "equipment", "/api/v1/meta/equipment", input.Equipment},
		{"category", "category", "/api/v1/meta/categories", input.Category},
		// mechanic, force and level are taxonomy facets but have no /meta route
		// (docs/DESIGN.md lists only four). Their vocabularies are small and
		// fixed, so the error spells the valid values out instead.
		{"mechanic", "mechanic", "", input.Mechanic},
		{"level", "level", "", input.Level},
	}

	var set []filterParam
	for _, g := range groups {
		for _, v := range g.values {
			if v = strings.TrimSpace(v); v != "" {
				set = append(set, filterParam{param: g.param, facet: g.facet, value: v, metaPath: g.metaPath})
			}
		}
	}
	return set
}

// validateTaxonomyFilters rejects any filter naming a term that does not exist,
// with a message that names the parameter, the bad value, and where to find the
// valid ones.
func validateTaxonomyFilters(ctx context.Context, s *store.Store, input *ListExercisesInput) error {
	filters := taxonomyFiltersOf(input)
	if len(filters) == 0 {
		return nil
	}

	pairs := make([]store.FacetSlug, 0, len(filters))
	for _, f := range filters {
		pairs = append(pairs, store.FacetSlug{Facet: f.facet, Slug: f.value})
	}

	unknown, err := s.Taxonomy.UnknownSlugs(ctx, pairs)
	if err != nil {
		return mapStoreError(err, "")
	}
	if len(unknown) == 0 {
		return nil
	}

	// Index by facet so the message can be built in the caller's parameter
	// order rather than whatever order Postgres returned rows in — a stable
	// message is one a client can match on and a human can diff.
	// Keyed by facet AND slug: a facet may now carry several values, and keying
	// by facet alone would report only whichever bad one Postgres returned last.
	bad := make(map[store.FacetSlug]bool, len(unknown))
	for _, u := range unknown {
		bad[store.FacetSlug{Facet: u.Facet, Slug: u.Slug}] = true
	}

	var details []error
	for _, f := range filters {
		if !bad[store.FacetSlug{Facet: f.facet, Slug: f.value}] {
			continue
		}
		details = append(details, &huma.ErrorDetail{
			Message:  unknownFilterMessage(ctx, s, f),
			Location: "query." + f.param,
			Value:    f.value,
		})
	}

	return huma.Error422UnprocessableEntity("validation failed", details...)
}

// unknownFilterMessage explains one bad filter value.
//
// It fetches the facet's real vocabulary rather than hardcoding it: taxonomy is
// seeded data that grows once contributions open, and a hardcoded list would
// start lying the first time a term is added. Small enough to be free — every
// facet is well under a hundred rows, and this only runs on a request that is
// already being rejected.
func unknownFilterMessage(ctx context.Context, s *store.Store, f filterParam) string {
	msg := fmt.Sprintf("unknown %s %q", f.param, f.value)

	terms, err := s.Taxonomy.ListTerms(ctx, f.facet, "en", maxPerPage, 0)
	if err != nil || len(terms) == 0 {
		// The vocabulary lookup is a nicety; failing it must not turn a clean
		// 422 into a 500, so fall back to the pointer alone.
		if f.metaPath != "" {
			return msg + "; see " + f.metaPath + " for valid values"
		}
		return msg
	}

	slugs := make([]string, 0, len(terms))
	for _, t := range terms {
		slugs = append(slugs, t.Slug)
	}
	sort.Strings(slugs)

	// A suggestion first when one is obvious — case and whitespace are the two
	// mistakes this endpoint actually sees, and naming the exact fix is worth
	// more than a list the caller has to scan.
	if guess, ok := closestSlug(f.value, slugs); ok {
		msg += fmt.Sprintf("; did you mean %q?", guess)
	}

	const maxListed = 12
	if len(slugs) <= maxListed {
		return msg + " (valid: " + strings.Join(slugs, ", ") + ")"
	}
	if f.metaPath != "" {
		return fmt.Sprintf("%s (%d valid values, e.g. %s — see %s for the full list)",
			msg, len(slugs), strings.Join(slugs[:5], ", "), f.metaPath)
	}
	return fmt.Sprintf("%s (%d valid values, e.g. %s)", msg, len(slugs), strings.Join(slugs[:5], ", "))
}

// closestSlug returns a valid slug that differs from value only by case or
// surrounding whitespace.
//
// Deliberately not a general edit-distance match. The mistakes this endpoint
// actually receives are `Chest` for `chest` and `chest ` for `chest` — a
// caller who has the right word in the wrong shape. A fuzzy matcher would also
// confidently suggest `chest` for `chests` and `abductors` for `adductors`,
// and a wrong suggestion in an error message is worse than none: it sends the
// caller to a value that exists but is not what they meant.
func closestSlug(value string, slugs []string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	for _, s := range slugs {
		if s == normalized {
			return s, true
		}
	}
	return "", false
}
