package api

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/conditional"

	"github.com/Cawlumm/open-exercise-db/internal/media"
	"github.com/Cawlumm/open-exercise-db/internal/model"
	"github.com/Cawlumm/open-exercise-db/internal/ratelimit"
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// imageCacheControl is far longer than cacheControl (api.go): a stored
// image's bytes never change once uploaded. A re-import that matches an
// already-uploaded image reuses its existing exercise_images row instead of
// writing a new one (store.MediaStore.GetImageByStorageKey's doc comment),
// and Phase 1 has no edit path for image bytes at all — so unlike exercise
// data, there is no "a recent edit might have gone stale" risk to hedge
// against with a short max-age. `immutable` tells a conforming client it
// never needs to revalidate this URL again once cached.
const imageCacheControl = "public, max-age=31536000, immutable"

// GetImageInput identifies the image by id. See ListExercisesInput's doc
// comment (exercises.go) for why conditional.Params is embedded rather than
// wired by hand.
type GetImageInput struct {
	conditional.Params
	ID string `path:"id" format:"uuid" doc:"Image UUID — see contract.Image.ID / Exercise.Images[].id"`
}

// GetImageOutput is a raw-bytes response, not a JSON body — see
// registerImageRoutes' Responses declaration and this file's package-level
// comment for how huma is asked to handle that.
type GetImageOutput struct {
	// ContentType overrides huma's normal content-negotiation-driven
	// Content-Type — huma.go treats a string field tagged `header:"Content-Type"`
	// as authoritative when a Body field is present, which is what makes
	// serving the *real* stored content type (never guessed from a file
	// extension, per this project's own JPEG-only-today-but-data-driven
	// design — see internal/media/encode.go) possible here.
	ContentType  string `header:"Content-Type"`
	CacheControl string `header:"Cache-Control"`
	ETag         string `header:"ETag"`
	// Body as a bare []byte is huma's documented, supported shape for a
	// binary response (https://huma.rocks/how-to/image-response/, mirrored
	// at github.com/danielgtaylor/huma/v2's huma_test.go "response-image"
	// case): huma.go's response-writing path special-cases a []byte Body
	// field and writes it straight to ctx.BodyWriter(), bypassing the
	// JSON/content-negotiation machinery entirely (the same special case
	// api.go's etagValue doc comment already discusses and rejects for
	// *typed* JSON responses, because it'd document them as opaque base64 in
	// the spec — that objection doesn't apply here, since this response
	// really is opaque bytes, not JSON pretending not to be).
	//
	// What this does mean: the response is fully buffered in memory before
	// being written, not streamed — huma has no first-class io.Reader-body
	// output (the only other escape hatch is a `func(Context)` field, which
	// would forfeit the header/status/ETag conveniences used above for a
	// bigger rewrite this defect doesn't warrant). Acceptable here: every
	// image in this dataset is re-encoded and capped at ~800px on the long
	// edge at import time (internal/media/encode.go's maxDimension), so
	// bytes per image top out in the tens-to-low-hundreds of KB, not a size
	// where buffering matters.
	Body []byte
}

// registerImageRoutes wires GET /api/v1/images/{id}. TierRead, not a new
// tier: docs/API_POLICY.md's Read tier is already framed as "cheap, cached,
// single-row reads," which an id-keyed image fetch is exactly one more
// instance of.
func registerImageRoutes(hapi huma.API, s *store.Store, mediaStore media.Store) {
	huma.Register(hapi, huma.Operation{
		OperationID: "get-image",
		Method:      http.MethodGet,
		Path:        "/api/v1/images/{id}",
		Summary:     "Fetch a re-hosted exercise image's bytes",
		Tags:        []string{"images"},
		Metadata:    map[string]any{rateLimitTierKey: ratelimit.TierRead},
		Errors:      []int{http.StatusNotFound, http.StatusTooManyRequests},
		// Declared explicitly (rather than left to huma's default []byte
		// schema, which documents as an opaque base64 string per api.go's
		// etagValue comment) so /docs and generated clients see this as the
		// binary image response it actually is. image/jpeg because every
		// image in this dataset is re-encoded to JPEG today
		// (internal/media/encode.go) — revisit if that ever changes.
		Responses: map[string]*huma.Response{
			"200": {
				Description: "Image bytes",
				Content: map[string]*huma.MediaType{
					"image/jpeg": {Schema: &huma.Schema{Type: "string", Format: "binary"}},
				},
			},
		},
	}, func(ctx context.Context, input *GetImageInput) (*GetImageOutput, error) {
		return GetImage(ctx, s, mediaStore, input)
	})

	// Registered after the plain-id route, and reachable because the two
	// patterns differ in segment count rather than overlapping — huma routes
	// through the same ServeMux whose wildcard rules api.go's index routes
	// already had to be checked against.
	huma.Register(hapi, huma.Operation{
		OperationID: "get-image-variant",
		Method:      http.MethodGet,
		Path:        "/api/v1/images/{id}/{variant}",
		Summary:     "Fetch one rendition of a re-hosted exercise image",
		Description: "Renditions are listed per image as Exercise.Images[].variants. " +
			"Today every image has exactly one, full, whose URL is the same as " +
			"Image.url — smaller sizes will appear here without url changing.",
		Tags:     []string{"images"},
		Metadata: map[string]any{rateLimitTierKey: ratelimit.TierRead},
		Errors:   []int{http.StatusNotFound, http.StatusTooManyRequests},
		Responses: map[string]*huma.Response{
			"200": {
				Description: "Image bytes",
				Content: map[string]*huma.MediaType{
					"image/jpeg": {Schema: &huma.Schema{Type: "string", Format: "binary"}},
				},
			},
		},
	}, func(ctx context.Context, input *GetImageVariantInput) (*GetImageOutput, error) {
		return GetImageVariant(ctx, s, mediaStore, input)
	})
}

// GetImage looks up id's exercise_images row for its storage_key/
// content_type, then reads the actual bytes from mediaStore — the same
// media.Store implementation (filesystem in Phase 1, R2 in Phase 2)
// cmd/import-freeexercisedb wrote them through. Exported, like every other
// handler in this package, so tests can call it directly with an input
// struct (docs/CONVENTIONS.md section 3/6).
// requireVisibleImage refuses to serve an image that has not been approved.
//
// This is the difference between hosting a moderated dataset and hosting
// whatever anyone uploads. Once POST /images exists, an unapproved row is
// arbitrary bytes a stranger sent: without this gate they are fetchable from
// this domain the instant they land, and the first anyone knows is when
// somebody else finds them. The moderation queue cannot be the control if the
// content is already public while it sits in it.
//
// A moderator sees pending images, because judging one requires looking at it.
// Same endpoint rather than a separate admin route: two routes reading the
// same bytes is two places to get the check wrong, and the second one is
// always the one that gets forgotten.
//
// 404 rather than 403 for everyone else. A 403 would confirm the id exists,
// turning this endpoint into an oracle for enumerating unapproved uploads —
// which is exactly the set of content that must not be discoverable.
func requireVisibleImage(ctx context.Context, img model.Image) error {
	if img.Status == model.ImageStatusApproved {
		return nil
	}
	if user, ok := UserFromContext(ctx); ok && user.CanModerate() {
		return nil
	}
	return huma.Error404NotFound("image not found")
}

func GetImage(ctx context.Context, s *store.Store, mediaStore media.Store, input *GetImageInput) (*GetImageOutput, error) {
	img, err := s.Media.GetImageByID(ctx, input.ID)
	if err != nil {
		return nil, mapStoreError(err, "image not found")
	}
	if err := requireVisibleImage(ctx, img); err != nil {
		return nil, err
	}

	rc, err := mediaStore.Get(ctx, img.StorageKey)
	if err != nil {
		// A row whose object is absent is a 404, not a 500. Both backends
		// report it as media.ErrObjectNotFound (FilesystemStore wraps
		// os.ErrNotExist, R2Store maps types.NoSuchKey) specifically so this
		// distinction could be made, and until this check existed nothing in
		// the API consumed the sentinel — every missing object answered 500.
		//
		// It matters most in the case that actually produces missing objects:
		// an incomplete cmd/migrate-media backfill leaves rows pointing at keys
		// the destination bucket never received. As a 5xx those are uncacheable
		// at the CDN, indistinguishable from the API being broken, and they
		// bury a real outage under thousands of routine errors.
		if errors.Is(err, media.ErrObjectNotFound) {
			log.Printf("image %s: object %s missing from media store", img.ID, img.StorageKey)
			return nil, huma.Error404NotFound("image not found")
		}
		log.Printf("media store get %s (image %s): %v", img.StorageKey, img.ID, err)
		return nil, huma.Error500InternalServerError("internal error")
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		log.Printf("read image %s (%s): %v", img.ID, img.StorageKey, err)
		return nil, huma.Error500InternalServerError("internal error")
	}

	tag := etagValueBytes(data)
	if notModified := checkNotModified(&input.Params, tag, imageCacheControl); notModified != nil {
		return nil, notModified
	}

	return &GetImageOutput{
		ContentType:  img.ContentType,
		CacheControl: imageCacheControl,
		ETag:         etagHeader(tag),
		Body:         data,
	}, nil
}

// GetImageVariantInput identifies one rendition by image id and variant
// name.
//
// Variant is a path segment rather than a ?variant= query parameter so each
// rendition is a distinct cacheable URL. A query string would work, but CDNs
// vary in whether they key on it, and getting a full-size image from a
// thumbnail URL because an edge cache ignored the query is the failure this
// avoids by construction.
//
// Pattern-constrained rather than validated in the handler: an unbounded
// name reaches the store as a query parameter and, worse, would be
// reflected into a 404 message. The character class matches what a
// rendition name can legitimately be.
type GetImageVariantInput struct {
	conditional.Params
	ID      string `path:"id" format:"uuid" doc:"Image UUID — see contract.Image.ID / Exercise.Images[].id"`
	Variant string `path:"variant" pattern:"^[a-z0-9][a-z0-9_-]{0,31}$" doc:"Rendition name from Exercise.Images[].variants[].name, e.g. full"`
}

// GetImageVariant serves one named rendition's bytes.
//
// Unknown names are 404, never a fallback to the full image. A client
// asking for a thumbnail and silently receiving a full-resolution image
// sees a successful request while downloading many times the bytes it
// budgeted for — on exactly the mobile connections thumbnails exist to
// protect, and with nothing in the response to reveal the substitution.
func GetImageVariant(ctx context.Context, s *store.Store, mediaStore media.Store, input *GetImageVariantInput) (*GetImageOutput, error) {
	variant, imageStatus, err := s.Media.GetImageVariant(ctx, input.ID, input.Variant)
	if err != nil {
		return nil, mapStoreError(err, "image variant not found")
	}
	// A variant is exactly as public as the image it belongs to.
	if err := requireVisibleImage(ctx, model.Image{ID: input.ID, Status: imageStatus}); err != nil {
		return nil, err
	}

	rc, err := mediaStore.Get(ctx, variant.StorageKey)
	if err != nil {
		// Same reasoning as GetImage: a row whose object is absent is a 404,
		// not a 500. Variants make this more likely rather than less — a
		// partially-completed re-encode leaves rows for renditions whose
		// bytes were never written.
		if errors.Is(err, media.ErrObjectNotFound) {
			log.Printf("image %s variant %s: object %s missing from media store", input.ID, input.Variant, variant.StorageKey)
			return nil, huma.Error404NotFound("image variant not found")
		}
		log.Printf("media store get %s (image %s variant %s): %v", variant.StorageKey, input.ID, input.Variant, err)
		return nil, huma.Error500InternalServerError("internal error")
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		log.Printf("read image %s variant %s (%s): %v", input.ID, input.Variant, variant.StorageKey, err)
		return nil, huma.Error500InternalServerError("internal error")
	}

	tag := etagValueBytes(data)
	if notModified := checkNotModified(&input.Params, tag, imageCacheControl); notModified != nil {
		return nil, notModified
	}

	return &GetImageOutput{
		ContentType:  variant.ContentType,
		CacheControl: imageCacheControl,
		ETag:         etagHeader(tag),
		Body:         data,
	}, nil
}
