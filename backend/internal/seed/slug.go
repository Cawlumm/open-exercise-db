package seed

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// nonSlugChars matches any run of characters that isn't a lowercase letter
// or digit, so slugify can collapse it to a single hyphen. Existing hyphens
// collapse too, which is what keeps "e-z curl bar" (an upstream equipment
// value that already contains a hyphen) from becoming "e--z-curl-bar".
var nonSlugChars = regexp.MustCompile(`[^a-z0-9]+`)

// slugify is the one slug function shared across this package: it's used
// both for exercise slugs (from exercise name) and taxonomy term slugs
// (from a facet value like "middle back" or "olympic weightlifting").
// Sharing it matters because exercise_revisions' facet columns are plain
// TEXT slugs validated in the app layer against taxonomy_terms.slug
// (docs/DESIGN.md's tradeoff note) — the two must be generated identically
// or a revision's muscle_group would never match any taxonomy_terms row.
func slugify(s string) string {
	s = strings.ToLower(s)
	s = nonSlugChars.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// uniqueExerciseSlug returns base if it's not already taken by any
// exercise, or base with a numeric suffix ("-2", "-3", ...) appended until
// one is free. Only called for exercises being created for the first time
// (docs/PHASE1_BUILD_PLAN.md WO-4: "slug generated from name, deduped with
// a numeric suffix on collision") — an existing exercise being updated
// keeps its already-assigned slug instead, so this never runs a
// self-collision loop against a row's own slug.
func uniqueExerciseSlug(ctx context.Context, es *store.ExerciseStore, base string) (string, error) {
	candidate := base
	for n := 2; ; n++ {
		exists, err := es.SlugExists(ctx, candidate)
		if err != nil {
			return "", fmt.Errorf("check slug %q: %w", candidate, err)
		}
		if !exists {
			return candidate, nil
		}
		candidate = fmt.Sprintf("%s-%d", base, n)
	}
}
