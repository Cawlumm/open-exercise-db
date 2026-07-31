package contract

import "time"

// Exercise is the published representation of an exercise: its stable
// identity and taxonomy fields (never translated) alongside a Translation
// block carrying whatever locale was actually resolved for its
// human-readable content. Rendered by GET /exercises, GET /exercises/{id},
// and GET /exercises/export — see doc.go for why this type lives here
// rather than in internal/api or internal/export.
//
// Deliberately not model.Exercise/model.Revision reused directly —
// docs/CONVENTIONS.md section 2: the published shape is a compatibility
// contract and must not drift just because a store field is renamed.
type Exercise struct {
	ID       string  `json:"id" doc:"Exercise UUID — what mirroring consumers should key on, not slug or name"`
	Slug     string  `json:"slug"`
	SourceID *string `json:"source_id,omitempty" doc:"Identifier from the original bootstrap source (e.g. free-exercise-db), if imported"`
	Status   string  `json:"status" doc:"published or archived"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	RevisionID       string   `json:"revision_id" doc:"ID of the revision currently live for this exercise"`
	MuscleGroup      string   `json:"muscle_group" doc:"Taxonomy slug; see GET /meta/muscle-groups"`
	SecondaryMuscles []string `json:"secondary_muscles"`
	Category         string   `json:"category" doc:"Taxonomy slug; see GET /meta/categories"`
	Equipment        string   `json:"equipment" doc:"Taxonomy slug; see GET /meta/equipment"`
	Mechanic         *string  `json:"mechanic,omitempty" doc:"compound or isolation"`
	Force            *string  `json:"force,omitempty" doc:"push, pull, or static"`
	Level            *string  `json:"level,omitempty" doc:"beginner, intermediate, or advanced"`
	Images           []Image  `json:"images" doc:"Re-hosted images for this exercise, each carrying enough to fetch and render it — see the Image schema"`
	VideoIDs         []string `json:"video_ids"`
	License          *string  `json:"license,omitempty" doc:"Per-revision license, e.g. Unlicense or CC-BY-SA-4.0 — provenance is honest per-exercise rather than assuming one license for the whole export"`
	AttributionText  *string  `json:"attribution_text,omitempty"`
	SourceURL        *string  `json:"source_url,omitempty"`

	Translation Translation `json:"translation" doc:"Locale-resolved name/aliases/instructions — see the Translation schema for how each field's served locale and machine-origin flag are reported"`
}

// Image is one re-hosted exercise image, resolved to a URL a client can
// fetch directly — the fix for the defect this type was introduced to close:
// GET /exercises previously returned only opaque image_ids UUIDs with no
// resolution path, so a consumer could never actually display an image.
// Modeled on Open Food Facts' own product image metadata (an id/URL plus
// size and provenance, letting the client construct or follow the URL
// itself) rather than inlining bytes — bytes never belong in a JSON API
// response any more than they belong in Postgres (internal/media's package
// doc has the storage-boundary rationale).
//
// URL is resolved by internal/media.BuildImages at read time from
// internal/config.Config.ImageBaseURL: pointed at an external CDN/object-store
// host when that's configured (production, once Phase 2's R2 migration
// lands), or at this API's own GET /api/v1/images/{id} route
// (internal/api/images.go) when it isn't (local dev, self-host) — see that
// config field's doc comment for the full rationale. Either way the client
// never has to know which mode is active; it just follows the URL.
type Image struct {
	ID              string  `json:"id"`
	URL             string  `json:"url" doc:"Directly fetchable — either an external CDN/object-store URL or this API's own GET /api/v1/images/{id}, depending on server configuration"`
	Width           *int    `json:"width,omitempty"`
	Height          *int    `json:"height,omitempty"`
	ContentType     string  `json:"content_type" doc:"The real, stored content type (e.g. image/jpeg) — never guessed from a file extension"`
	License         *string `json:"license,omitempty"`
	AttributionText *string `json:"attribution_text,omitempty"`
}

// Translation renders model.ResolvedTranslation (docs/PHASE1_BUILD_PLAN.md's
// WO-2 status note: this, not model.Translation, is what gets rendered).
// It's nested under Exercise.Translation rather than flattened, so the
// always-present taxonomy/metadata fields on Exercise stay visually and
// schematically separate from the fields that can legitimately come from a
// different locale than the one requested. Every translatable field carries
// its own served locale and machine-origin flag — mirroring
// ResolvedTranslation's own per-field design — because a single response can
// honestly mix e.g. an "es" name with "en" instructions when only the name
// has been translated.
//
// GET /exercises/export always resolves with an empty requested locale
// (there is no ?locale= parameter on that endpoint), which
// store.TranslationStore.GetForRevision treats identically to an explicit
// "en" — the same English-default behavior GET /exercises and
// GET /exercises/{id} have when no ?locale= is given.
type Translation struct {
	RequestedLocale string `json:"requested_locale" doc:"Locale that was requested; en if none was given"`

	Name              string `json:"name"`
	NameLocale        string `json:"name_locale" doc:"Locale this field was actually served in — may fall back to en if requested_locale has no translation for it"`
	NameMachineOrigin bool   `json:"name_machine_origin" doc:"True if the served value is an unreviewed machine translation"`

	Aliases              []string `json:"aliases"`
	AliasesLocale        string   `json:"aliases_locale"`
	AliasesMachineOrigin bool     `json:"aliases_machine_origin"`

	Instructions              []string `json:"instructions" doc:"JSON array of strings — matches the database column; never flattened into a single numbered description"`
	InstructionsLocale        string   `json:"instructions_locale"`
	InstructionsMachineOrigin bool     `json:"instructions_machine_origin"`
}
