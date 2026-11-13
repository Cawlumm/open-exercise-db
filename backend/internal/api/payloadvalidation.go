package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"

	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// Bounds on the free-text a submission may carry. Generous enough for any real
// exercise, tight enough that the moderation queue cannot be filled with prose.
const (
	maxNameLen         = 120
	maxInstructionLen  = 2000
	maxInstructions    = 40
	maxAliases         = 20
	maxSecondaryMuscle = 10

	// Provenance text is published verbatim on the exercise and in the
	// export, so it is bounded like the rest of the prose. 2048 is the
	// conventional practical ceiling for a URL.
	maxSourceURLLen     = 2048
	maxAttributionLen   = 500
	maxChangeSummaryLen = 500
)

// exercisePayload is the shape a create or edit proposes.
//
// Pointers on every optional field so "absent" and "explicitly empty" stay
// distinguishable: an edit omitting `instructions` proposes nothing about
// them, while one sending `[]` proposes deleting them. Collapsing those would
// make every partial edit look like a request to blank the fields it did not
// mention.
type exercisePayload struct {
	Name             *string   `json:"name"`
	MuscleGroup      *string   `json:"muscle_group"`
	Category         *string   `json:"category"`
	Equipment        *string   `json:"equipment"`
	Mechanic         *string   `json:"mechanic"`
	Force            *string   `json:"force"`
	Level            *string   `json:"level"`
	Instructions     *[]string `json:"instructions"`
	Aliases          *[]string `json:"aliases"`
	SecondaryMuscles *[]string `json:"secondary_muscles"`
	License          *string   `json:"license"`
	AttributionText  *string   `json:"attribution_text"`
	SourceURL        *string   `json:"source_url"`
	ChangeSummary    *string   `json:"change_summary"`
}

// validatePayloadFields checks a submission's content against the field rules
// and the live taxonomy.
//
// Runs at submit time rather than at approval, deliberately. A moderator is
// the last line, not the first: catching "muscle_group: banana" here means it
// never occupies a slot in the queue, never costs anyone a decision, and the
// app that sent it learns immediately — while catching it at review means a
// human spent attention on something a query could have refused.
//
// The taxonomy columns are plain TEXT with no foreign key (migration 0001,
// which chose that so adding a vocabulary term stays a row insert rather than
// a migration). That decision moved referential integrity into the app layer;
// this is the app layer holding up its end. Without it an approved
// `muscle_group: "banana"` publishes an exercise that appears in no filter and
// in no /meta listing — not broken, just permanently invisible, which is worse
// because nothing reports it.
func validatePayloadFields(ctx context.Context, s *store.Store, subType string, raw json.RawMessage) error {
	var p exercisePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		// Shape errors are the caller's, and the message names the field
		// rather than echoing a decoder error nobody can act on.
		return huma.Error422UnprocessableEntity("payload fields are not the expected types: " + fieldFromJSONError(err))
	}

	var problems []string

	// A translation is a name and instructions in another language. It still
	// gets the same length, blank and count rules as English content, but it
	// does not need the taxonomy fields a create needs and is never checked
	// against the English vocabulary.
	if subType == "translation" && (p.Name == nil || strings.TrimSpace(*p.Name) == "") {
		problems = append(problems, "name is required for a translation")
	}

	// A create must carry the fields an exercise cannot exist without. An
	// edit need not: it proposes a subset, and demanding the whole record
	// would make correcting one typo mean resubmitting everything.
	if subType == "create" {
		for name, v := range map[string]*string{
			"name": p.Name, "muscle_group": p.MuscleGroup,
			"category": p.Category, "equipment": p.Equipment,
		} {
			if v == nil || strings.TrimSpace(*v) == "" {
				problems = append(problems, name+" is required for a new exercise")
			}
		}
		if p.Instructions == nil || len(*p.Instructions) == 0 {
			problems = append(problems, "instructions are required for a new exercise")
		}
	}

	if p.Name != nil {
		if n := strings.TrimSpace(*p.Name); n == "" {
			problems = append(problems, "name must not be blank")
		} else if len(n) > maxNameLen {
			problems = append(problems, fmt.Sprintf("name is %d characters; the limit is %d", len(n), maxNameLen))
		}
	}

	problems = append(problems, checkStrings("instructions", p.Instructions, maxInstructions, maxInstructionLen)...)
	problems = append(problems, checkStrings("aliases", p.Aliases, maxAliases, maxNameLen)...)
	problems = append(problems, checkStrings("secondary_muscles", p.SecondaryMuscles, maxSecondaryMuscle, maxNameLen)...)

	problems = append(problems, checkProvenance(p)...)

	// Taxonomy last: it costs a query, and there is no point spending one on a
	// payload already known to be malformed. Translations carry no taxonomy.
	if len(problems) == 0 && subType != "translation" {
		taxProblems, err := checkTaxonomy(ctx, s, p)
		if err != nil {
			return err
		}
		problems = append(problems, taxProblems...)
	}

	if len(problems) == 0 {
		return nil
	}
	// Every problem at once, sorted. Returning only the first means a caller
	// fixing three mistakes discovers them one round trip at a time.
	sort.Strings(problems)
	return huma.Error422UnprocessableEntity(strings.Join(problems, "; "))
}

// checkProvenance validates the fields that are published as-is alongside the
// content: where it came from and under what terms.
//
// The licence matters most. The catalog is republished under CC0 (DATA_LICENSE),
// so a revision declaring CC-BY-SA or "all rights reserved" would make that
// grant false for the exercise carrying it. It is checked against the same
// list image uploads use, and blank means "not stated".
func checkProvenance(p exercisePayload) []string {
	var problems []string

	if p.License != nil && strings.TrimSpace(*p.License) != "" && !acceptedLicenses[*p.License] {
		problems = append(problems, fmt.Sprintf(
			"license %q is not accepted; the catalog is CC0, so only CC0-1.0 or Unlicense can be republished", *p.License))
	}

	if p.SourceURL != nil && strings.TrimSpace(*p.SourceURL) != "" {
		if msg := checkSourceURL(*p.SourceURL); msg != "" {
			problems = append(problems, msg)
		}
	}

	if p.AttributionText != nil && utf8.RuneCountInString(*p.AttributionText) > maxAttributionLen {
		problems = append(problems, fmt.Sprintf("attribution_text is %d characters; the limit is %d",
			utf8.RuneCountInString(*p.AttributionText), maxAttributionLen))
	}
	if p.ChangeSummary != nil && utf8.RuneCountInString(*p.ChangeSummary) > maxChangeSummaryLen {
		problems = append(problems, fmt.Sprintf("change_summary is %d characters; the limit is %d",
			utf8.RuneCountInString(*p.ChangeSummary), maxChangeSummaryLen))
	}
	return problems
}

// checkSourceURL returns a problem description for a source_url that is not an
// absolute https URL, or "".
//
// https only: the value is rendered as a link in clients, so javascript:,
// data: and file: must not get through, and plain http is refused so a
// provenance link cannot be silently swapped in transit.
func checkSourceURL(raw string) string {
	if len(raw) > maxSourceURLLen {
		return fmt.Sprintf("source_url is %d characters; the limit is %d", len(raw), maxSourceURLLen)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return "source_url must be an absolute https URL"
	}
	if u.User != nil {
		return "source_url must not contain credentials"
	}
	return ""
}

// checkStrings validates a string array field.
func checkStrings(field string, v *[]string, maxItems, maxLen int) []string {
	if v == nil {
		return nil
	}
	var problems []string
	if len(*v) > maxItems {
		problems = append(problems, fmt.Sprintf("%s has %d entries; the limit is %d", field, len(*v), maxItems))
	}
	for i, s := range *v {
		if strings.TrimSpace(s) == "" {
			problems = append(problems, fmt.Sprintf("%s[%d] is blank", field, i))
			continue
		}
		if len(s) > maxLen {
			problems = append(problems, fmt.Sprintf("%s[%d] is %d characters; the limit is %d", field, i, len(s), maxLen))
		}
	}
	return problems
}

// checkTaxonomy rejects values naming no vocabulary term.
func checkTaxonomy(ctx context.Context, s *store.Store, p exercisePayload) ([]string, error) {
	// Reuses the same lookup GET /exercises validates its filters with, so a
	// value that would be rejected as a filter cannot be accepted as content —
	// a submission proposing a muscle group nobody can then filter by is not
	// worth queueing.
	var pairs []store.FacetSlug
	add := func(facet string, v *string) {
		if v != nil && strings.TrimSpace(*v) != "" {
			pairs = append(pairs, store.FacetSlug{Facet: facet, Slug: strings.TrimSpace(*v)})
		}
	}
	add("muscle_group", p.MuscleGroup)
	add("category", p.Category)
	add("equipment", p.Equipment)
	add("mechanic", p.Mechanic)
	add("force", p.Force)
	add("level", p.Level)
	if p.SecondaryMuscles != nil {
		for _, m := range *p.SecondaryMuscles {
			if strings.TrimSpace(m) != "" {
				pairs = append(pairs, store.FacetSlug{Facet: "muscle_group", Slug: strings.TrimSpace(m)})
			}
		}
	}
	if len(pairs) == 0 {
		return nil, nil
	}

	unknown, err := s.Taxonomy.UnknownSlugs(ctx, pairs)
	if err != nil {
		return nil, mapStoreError(err, "")
	}

	problems := make([]string, 0, len(unknown))
	for _, u := range unknown {
		problems = append(problems, fmt.Sprintf(
			"%s %q is not a known value; see /api/v1/meta/%s", u.Facet, u.Slug, metaPathFor(u.Facet)))
	}
	return problems, nil
}

// metaPathFor maps a facet onto the endpoint listing its vocabulary, so the
// error tells a caller where to look rather than only that they were wrong.
func metaPathFor(facet string) string {
	switch facet {
	case "muscle_group":
		return "muscle-groups"
	case "category":
		return "categories"
	default:
		// equipment, mechanic, force, level. Only the first three have their
		// own endpoint today; the others are still listed under the exercise
		// schema, and naming the closest useful place beats naming none.
		return facet
	}
}

// fieldFromJSONError extracts the offending field from a decode error.
func fieldFromJSONError(err error) string {
	// errors.As rather than a type assertion: encoding/json wraps its errors
	// in some paths, and a bare assertion would silently fall through to the
	// generic message exactly when the specific one is most useful.
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) && typeErr.Field != "" {
		return fmt.Sprintf("%s should be %s", typeErr.Field, typeErr.Type)
	}
	return "check the field types against the schema"
}
