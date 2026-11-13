package seed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Cawlumm/open-exercise-db/internal/media"
	"github.com/Cawlumm/open-exercise-db/internal/model"
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// DefaultSourceURL is free-exercise-db's generated combined dataset —
// docs/DESIGN.md's Day-1 bootstrap source.
const DefaultSourceURL = "https://raw.githubusercontent.com/yuhonas/free-exercise-db/main/dist/exercises.json"

// DefaultImageBaseURL is where each item's relative image path (e.g.
// "3_4_Sit-Up/0.jpg") resolves against, to reach the raw file on GitHub.
const DefaultImageBaseURL = "https://raw.githubusercontent.com/yuhonas/free-exercise-db/main/exercises/"

// licenseUnlicense and attributionSourceURL are the fixed provenance values
// docs/DESIGN.md's Bootstrap data section specifies for every imported row:
// license='Unlicense', source_url='https://github.com/yuhonas/free-exercise-db'.
const (
	licenseUnlicense     = "Unlicense"
	attributionSourceURL = "https://github.com/yuhonas/free-exercise-db"
)

// Taxonomy facet names, matching the CHECK constraint on
// taxonomy_terms.facet / exercise_revisions' facet columns
// (docs/database/migrations/0001_initial_schema.sql).
const (
	facetMuscleGroup = "muscle_group"
	facetEquipment   = "equipment"
	facetCategory    = "category"
	facetMechanic    = "mechanic"
	facetForce       = "force"
	facetLevel       = "level"
)

// maxImageBytes caps how much of an image response body is read, so one
// unexpectedly huge or slow-to-terminate response can't exhaust memory
// during an otherwise-routine 800-item import.
const maxImageBytes = 20 << 20 // 20MiB

// fedExercise mirrors one element of free-exercise-db's exercises.json.
// Field names match the upstream JSON exactly; see
// https://github.com/yuhonas/free-exercise-db for the source shape. force,
// mechanic, and equipment are nullable upstream (verified directly against
// the live dataset — e.g. 77 of 873 items have equipment: null); level is
// always present.
type fedExercise struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Force            *string  `json:"force"`
	Level            string   `json:"level"`
	Mechanic         *string  `json:"mechanic"`
	Equipment        *string  `json:"equipment"`
	PrimaryMuscles   []string `json:"primaryMuscles"`
	SecondaryMuscles []string `json:"secondaryMuscles"`
	Instructions     []string `json:"instructions"`
	Category         string   `json:"category"`
	Images           []string `json:"images"`
}

// Options configures Import.
type Options struct {
	// SourceURL is the exercises.json dataset to fetch. Defaults to
	// DefaultSourceURL; tests point this at an httptest server instead of
	// the real upstream (docs/PHASE1_BUILD_PLAN.md WO-4: "do not hit the
	// real free-exercise-db URL in tests").
	SourceURL string
	// ImageBaseURL is what each item's relative image path is resolved
	// against. Defaults to DefaultImageBaseURL.
	ImageBaseURL string
	// HTTPClient is used for both the dataset fetch and every image
	// download. Defaults to a client with a bounded per-request timeout.
	HTTPClient *http.Client
	// Limit caps how many items (after fetch, before processing) are
	// imported. 0 means no limit.
	Limit int
	// DryRun reports what would happen without writing anything — no
	// exercise/revision/taxonomy rows, no image downloads, no media Store
	// writes.
	DryRun bool
	// MediaStore is where re-hosted images are written. Required unless
	// DryRun is true.
	MediaStore media.Store
	// Logger receives one line per skipped (non-fatal) failure, e.g. an
	// image URL that 404s. Defaults to log.Default().
	Logger *log.Logger
}

func (o Options) withDefaults() Options {
	if o.SourceURL == "" {
		o.SourceURL = DefaultSourceURL
	}
	if o.ImageBaseURL == "" {
		o.ImageBaseURL = DefaultImageBaseURL
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if o.Logger == nil {
		o.Logger = log.Default()
	}
	return o
}

// Result totals what Import did, for the CLI's summary report.
type Result struct {
	Fetched   int // items read from the dataset (after --limit)
	Created   int // brand new exercises
	Updated   int // existing exercises whose content changed, so a new revision was written
	Unchanged int // existing exercises whose content was identical — the idempotency case

	ImagesUploaded int // newly downloaded, re-encoded, and stored
	ImagesReused   int // already re-hosted from a previous run, row and object both present
	ImagesRepaired int // row survived but the object was gone — re-downloaded under the same id
	ImagesFailed   int // download/decode/store failures — logged and skipped, not fatal

	TaxonomyTermsSeen int
}

// Import fetches free-exercise-db's dataset and upserts every item as an
// approved exercise + revision, per docs/PHASE1_BUILD_PLAN.md WO-4. It never
// aborts the whole run over a single item's image failing — see
// processImages — but a dataset fetch failure or a database error is fatal,
// since those indicate the run as a whole can't produce trustworthy results.
func Import(ctx context.Context, st *store.Store, opts Options) (Result, error) {
	opts = opts.withDefaults()

	items, err := fetchDataset(ctx, opts.HTTPClient, opts.SourceURL)
	if err != nil {
		return Result{}, fmt.Errorf("fetch dataset: %w", err)
	}
	if opts.Limit > 0 && opts.Limit < len(items) {
		items = items[:opts.Limit]
	}

	result := Result{Fetched: len(items)}

	taxResult, err := PopulateTaxonomy(ctx, st, items, opts.DryRun)
	if err != nil {
		return result, fmt.Errorf("populate taxonomy: %w", err)
	}
	result.TaxonomyTermsSeen = taxResult.TermsSeen

	for _, item := range items {
		if len(item.PrimaryMuscles) == 0 {
			opts.Logger.Printf("import: skipping %s (%s): no primary muscle group", item.ID, item.Name)
			continue
		}

		var imageIDs []string
		if !opts.DryRun {
			var stats imageStats
			imageIDs, stats = processImages(ctx, st, opts.MediaStore, opts.HTTPClient, opts.ImageBaseURL, item.ID, item.Images, opts.Logger)
			result.ImagesUploaded += stats.uploaded
			result.ImagesReused += stats.reused
			result.ImagesRepaired += stats.repaired
			result.ImagesFailed += stats.failed
		}

		revision := buildRevisionInput(item, imageIDs)

		// The slug an importer would assign only matters for a brand new
		// exercise — reusing GetBySourceID's answer here (rather than
		// letting UpsertImport figure out new-vs-existing itself) is what
		// keeps uniqueExerciseSlug from ever running its dedup loop against
		// an exercise's own already-assigned slug.
		var slug string
		if !opts.DryRun {
			existing, err := st.Exercises.GetBySourceID(ctx, item.ID)
			switch {
			case err == nil:
				slug = existing.Slug
			case errors.Is(err, pgx.ErrNoRows):
				slug, err = uniqueExerciseSlug(ctx, st.Exercises, slugify(item.Name))
				if err != nil {
					return result, fmt.Errorf("generate slug for %s: %w", item.ID, err)
				}
			default:
				return result, fmt.Errorf("look up exercise for source %s: %w", item.ID, err)
			}
		}

		imported, err := st.Exercises.UpsertImport(ctx, store.UpsertImportInput{
			SourceID: item.ID,
			Slug:     slug,
			Revision: revision,
			DryRun:   opts.DryRun,
		})
		if err != nil {
			return result, fmt.Errorf("import %s (%s): %w", item.ID, item.Name, err)
		}

		switch {
		case imported.Created:
			result.Created++
		case imported.RevisionAdded:
			result.Updated++
		default:
			result.Unchanged++
		}
	}

	return result, nil
}

// fetchDataset downloads and decodes sourceURL's JSON array.
func fetchDataset(ctx context.Context, client *http.Client, sourceURL string) ([]fedExercise, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", sourceURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("request %s: unexpected status %s", sourceURL, resp.Status)
	}

	var items []fedExercise
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return nil, fmt.Errorf("decode %s: %w", sourceURL, err)
	}
	return items, nil
}

// buildRevisionInput maps one upstream item plus its already-resolved
// imageIDs onto the store's import content shape. All taxonomy-facet
// fields are slugified with the exact same function PopulateTaxonomy uses
// to seed taxonomy_terms, so a revision's muscle_group/equipment/etc. is
// always a slug some taxonomy_terms row actually has.
func buildRevisionInput(item fedExercise, imageIDs []string) store.ImportRevisionInput {
	secondary := make([]string, 0, len(item.PrimaryMuscles)-1+len(item.SecondaryMuscles))
	for _, m := range item.PrimaryMuscles[1:] {
		secondary = append(secondary, slugify(m))
	}
	for _, m := range item.SecondaryMuscles {
		secondary = append(secondary, slugify(m))
	}

	equipment := noneEquipmentSlug
	if item.Equipment != nil && *item.Equipment != "" {
		equipment = slugify(*item.Equipment)
	}

	instructions := item.Instructions
	if instructions == nil {
		instructions = []string{}
	}

	if imageIDs == nil {
		imageIDs = []string{}
	}

	var mechanic, force, level *string
	if item.Mechanic != nil && *item.Mechanic != "" {
		v := slugify(*item.Mechanic)
		mechanic = &v
	}
	if item.Force != nil && *item.Force != "" {
		v := slugify(*item.Force)
		force = &v
	}
	if item.Level != "" {
		v := slugify(item.Level)
		level = &v
	}

	license := licenseUnlicense
	sourceURL := attributionSourceURL

	return store.ImportRevisionInput{
		Name:             item.Name,
		Aliases:          []string{},
		MuscleGroup:      slugify(item.PrimaryMuscles[0]),
		SecondaryMuscles: secondary,
		Category:         slugify(item.Category),
		Equipment:        equipment,
		Mechanic:         mechanic,
		Force:            force,
		Level:            level,
		Instructions:     instructions,
		ImageIDs:         imageIDs,
		License:          &license,
		AttributionText:  nil,
		SourceURL:        &sourceURL,
		ChangeSummary:    nil,
	}
}

// imageStats totals processImages' outcomes for one exercise's image list.
type imageStats struct {
	uploaded, reused, repaired, failed int
}

// processImages re-hosts each of paths (relative to imageBaseURL) as an
// exercise_images row, returning the resulting ids in the same order.
//
// Idempotency: the storage key is deterministic
// ("exercises/<source-id>/<index>.jpg"), so a re-run recognizes an
// already-uploaded image via GetImageByStorageKey and reuses its id
// instead of re-downloading/re-encoding/re-inserting — which is what keeps
// image_ids stable across runs (store.revisionUnchanged compares it like
// any other field).
//
// Repair: recognizing the row is not the same as confirming the object, and
// the two have independent lifetimes — Postgres persists on its own volume
// while the bytes sit in a container layer or an R2 bucket that can be
// recreated under it. This originally treated the row as proof of the
// object and skipped straight to reuse, so a deployment that lost its media
// directory kept 1746 rows advertising images that every request 404'd, and
// re-running the importer — the documented repair — reported them all
// "reused" and downloaded nothing. The gap was unfixable through any
// supported path. Checking mediaStore.Exists costs one HEAD (or one stat)
// per image and makes the import genuinely idempotent about bytes rather
// than only about rows.
//
// A repaired image keeps its existing id, so revisions referencing it are
// untouched; only the object and the row's encoding facts are rewritten.
//
// Fault tolerance: a download, decode, or store failure for one image is
// logged and that image is skipped — never fatal to the exercise (its
// other images still get processed) or to the run (docs/PHASE1_BUILD_PLAN.md
// WO-4: "one bad asset must not abort an 800-item import"). An Exists that
// errors is deliberately treated as present: the failure means the store
// could not answer, and re-uploading every object because a token briefly
// lost its read scope would be a far more expensive mistake than skipping a
// repair this run and catching it on the next.
func processImages(ctx context.Context, st *store.Store, mediaStore media.Store, client *http.Client, imageBaseURL, sourceID string, paths []string, logger *log.Logger) ([]string, imageStats) {
	ids := make([]string, 0, len(paths))
	var stats imageStats

	for i, p := range paths {
		key := fmt.Sprintf("exercises/%s/%d.jpg", sourceID, i)
		imageURL := imageBaseURL + p

		existing, err := st.Media.GetImageByStorageKey(ctx, key)
		switch {
		case err == nil:
			// Row found. Whether that is enough depends on the object.
			present, existsErr := mediaStore.Exists(ctx, key)
			if existsErr != nil {
				logger.Printf("import %s: check stored image %s: %v (assuming present, will retry next run)", sourceID, key, existsErr)
				ids = append(ids, existing.ID)
				stats.reused++
				continue
			}
			if present {
				ids = append(ids, existing.ID)
				stats.reused++
				continue
			}

			encoded, err := fetchEncoded(ctx, client, imageURL)
			if err != nil {
				logger.Printf("import %s: repair %s: %v (skipping)", sourceID, key, err)
				stats.failed++
				continue
			}
			if err := mediaStore.Put(ctx, key, bytes.NewReader(encoded.Data), encoded.ContentType); err != nil {
				logger.Printf("import %s: repair store %s: %v (skipping)", sourceID, key, err)
				stats.failed++
				continue
			}
			// Row metadata is updated after the object lands, never before:
			// the reverse order would claim an encoding for an object that
			// was never written.
			//
			// A failure here does NOT self-heal. The object now exists, so
			// every later run takes the reuse path above and never revisits
			// this row — the stale width/height/content_type persist until
			// someone corrects them or deletes the object. Deliberately not
			// counted as a failure: the bytes are restored and the image
			// serves, which is the outcome that mattered. It is logged at a
			// volume proportionate to being permanent rather than passed
			// over, because an unbounded "will fix itself" is exactly the
			// assumption that produced the missing objects in the first
			// place.
			// The variant row is re-pointed too: a repair rewrote these bytes,
			// so the rendition's recorded encoding has to describe what is
			// actually stored, not what was there before it went missing.
			recordFullVariant(ctx, st, existing.ID, key, encoded, logger, sourceID)
			recordThumbVariant(ctx, st, mediaStore, existing.ID, key, encoded.Data, logger, sourceID)

			width, height := encoded.Width, encoded.Height
			if err := st.Media.UpdateImageEncoding(ctx, existing.ID, encoded.ContentType, &width, &height); err != nil {
				logger.Printf("import %s: WARNING: repaired object %s but could not update its row: %v — "+
					"the image serves correctly while its recorded width/height/content_type stay stale, "+
					"and no later run will retry this. Fix the row by hand, or delete the object to force "+
					"another repair.", sourceID, key, err)
			}
			logger.Printf("import %s: repaired missing object %s (row existed, bytes did not)", sourceID, key)
			ids = append(ids, existing.ID)
			stats.repaired++
			continue

		case !errors.Is(err, pgx.ErrNoRows):
			logger.Printf("import %s: look up existing image %s: %v (skipping)", sourceID, key, err)
			stats.failed++
			continue
		}

		encoded, err := fetchEncoded(ctx, client, imageURL)
		if err != nil {
			logger.Printf("import %s: %v (skipping)", sourceID, err)
			stats.failed++
			continue
		}

		if err := mediaStore.Put(ctx, key, bytes.NewReader(encoded.Data), encoded.ContentType); err != nil {
			logger.Printf("import %s: store %s: %v (skipping)", sourceID, key, err)
			stats.failed++
			continue
		}

		license := licenseUnlicense
		width, height := encoded.Width, encoded.Height
		id, err := st.Media.InsertImage(ctx, store.InsertImageInput{
			StorageKey:  key,
			ContentType: encoded.ContentType,
			Width:       &width,
			Height:      &height,
			License:     &license,
			// A trusted first-party import bypasses the moderation queue
			// the same way exercises/revisions do (docs/DESIGN.md's
			// Bootstrap data section), so these are approved on write, not
			// left 'pending' for a review that will never happen for them.
			Status: "approved",
		})
		if err != nil {
			logger.Printf("import %s: insert image row for %s: %v (skipping)", sourceID, key, err)
			stats.failed++
			continue
		}

		recordFullVariant(ctx, st, id, key, encoded, logger, sourceID)
		recordThumbVariant(ctx, st, mediaStore, id, key, encoded.Data, logger, sourceID)

		ids = append(ids, id)
		stats.uploaded++
	}

	return ids, stats
}

// recordFullVariant writes the "full" rendition row for an image whose
// bytes were just stored, so a newly imported image is described the same
// way migration 0002 describes every pre-existing one.
//
// Failure is logged and not counted against the image. The image itself is
// complete — row written, bytes stored, and contract.Image's url/width/
// height all read from exercise_images rather than from here — so a missing
// variant row costs the response nothing today: media.BuildImages
// synthesizes the full entry when none exists. Treating it as a failure
// would drop a perfectly good image out of the revision over metadata that
// is currently redundant.
func recordFullVariant(ctx context.Context, st *store.Store, imageID, key string, encoded media.EncodeResult, logger *log.Logger, sourceID string) {
	width, height := encoded.Width, encoded.Height
	hash := media.ContentSHA256(encoded.Data)
	if _, err := st.Media.UpsertImageVariant(ctx, store.UpsertImageVariantInput{
		ImageID:       imageID,
		Name:          model.VariantFull,
		StorageKey:    key,
		ContentType:   encoded.ContentType,
		Width:         &width,
		Height:        &height,
		ContentSHA256: &hash,
	}); err != nil {
		logger.Printf("import %s: record full variant for %s: %v (image is served from exercise_images; re-run to add it)", sourceID, key, err)
	}
}

// recordThumbVariant writes the thumb rendition for an image whose full
// bytes were just stored. Failure is logged and not counted against the
// image, for the same reason as recordFullVariant: the image is complete
// without it, the thumb route serves the full image in its place, and
// cmd/backfill-thumbs picks up any image still missing one.
func recordThumbVariant(ctx context.Context, st *store.Store, mediaStore media.Store, imageID, fullKey string, full []byte, logger *log.Logger, sourceID string) {
	thumb, err := media.StoreThumb(ctx, mediaStore, fullKey, full)
	if err != nil {
		logger.Printf("import %s: thumb for %s: %v (full image unaffected; cmd/backfill-thumbs will retry)", sourceID, fullKey, err)
		return
	}
	width, height := thumb.Width, thumb.Height
	if _, err := st.Media.UpsertImageVariant(ctx, store.UpsertImageVariantInput{
		ImageID:       imageID,
		Name:          model.VariantThumb,
		StorageKey:    thumb.StorageKey,
		ContentType:   thumb.ContentType,
		Width:         &width,
		Height:        &height,
		ContentSHA256: &thumb.ContentSHA256,
	}); err != nil {
		logger.Printf("import %s: record thumb variant for %s: %v (cmd/backfill-thumbs will retry)", sourceID, fullKey, err)
	}
}

// fetchEncoded downloads url and runs it through media.Encode, the pair of
// steps that turns a source URL into bytes ready for a Store.
//
// Extracted so the first-upload and repair paths in processImages cannot
// drift: a repair that re-downloaded without re-encoding would write raw
// source bytes — EXIF intact, dimensions uncapped — under a key whose row
// claims the encoder's normalized output. Errors are wrapped with the URL
// so the caller can log one line without restating it.
func fetchEncoded(ctx context.Context, client *http.Client, url string) (media.EncodeResult, error) {
	raw, err := downloadImage(ctx, client, url)
	if err != nil {
		return media.EncodeResult{}, fmt.Errorf("download %s: %w", url, err)
	}
	encoded, err := media.Encode(bytes.NewReader(raw))
	if err != nil {
		return media.EncodeResult{}, fmt.Errorf("encode %s: %w", url, err)
	}
	return encoded, nil
}

// downloadImage fetches url and returns its body, capped at maxImageBytes.
func downloadImage(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return data, nil
}
