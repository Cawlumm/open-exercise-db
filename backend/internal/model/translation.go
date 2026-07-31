package model

import "time"

// Translation is one raw exercise_translations row: a single locale's
// rendering of a Revision's translatable fields (name, aliases,
// instructions). Rows accumulate as superseded history in the same
// append-only style as Revision; only the row with status 'approved' for a
// given (revision_id, locale) is live.
type Translation struct {
	ID           string    `json:"id"`
	RevisionID   string    `json:"revision_id"`
	Locale       string    `json:"locale"`
	Name         string    `json:"name"`
	Aliases      []string  `json:"aliases"`
	Instructions []string  `json:"instructions"`
	TranslatedBy *string   `json:"translated_by,omitempty"`
	Origin       string    `json:"origin"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
}

// ResolvedTranslation is a Revision's translatable content resolved for a
// requested locale, per docs/PHASE1_BUILD_PLAN.md's WO-2 "Locale fallback
// rule": fallback to English (the language the revision's own columns are
// always written in) happens per field, not for the whole record, so a
// Translation row missing e.g. instructions doesn't blank the whole
// response. Each field therefore carries its own served-locale and
// machine-origin flag rather than one record-level pair of either, since a
// single response can legitimately mix "es" and "en" fields.
type ResolvedTranslation struct {
	RequestedLocale string `json:"requested_locale"`

	Name              string `json:"name"`
	NameLocale        string `json:"name_locale"`
	NameMachineOrigin bool   `json:"name_machine_origin"`

	Aliases              []string `json:"aliases"`
	AliasesLocale        string   `json:"aliases_locale"`
	AliasesMachineOrigin bool     `json:"aliases_machine_origin"`

	Instructions              []string `json:"instructions"`
	InstructionsLocale        string   `json:"instructions_locale"`
	InstructionsMachineOrigin bool     `json:"instructions_machine_origin"`
}

// TranslationSummary describes one locale's completeness for an exercise,
// for GET /exercises/{id}/translations — a listing view, not full content.
type TranslationSummary struct {
	Locale     string `json:"locale"`
	Origin     string `json:"origin"`
	Complete   bool   `json:"complete"`
	RevisionID string `json:"revision_id"`
}
