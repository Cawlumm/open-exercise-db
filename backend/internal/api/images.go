package api

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/conditional"
	"github.com/jackc/pgx/v5"

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
		Metadata:    map[string]any{rateLimitTierKey: ratelimit.TierImage},
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
		Description: "Renditions are listed per image as Exercise.Images[].variants: " +
			"full (whose URL is the same as Image.url) and thumb (longest edge at most 128px). " +
			"Asking for thumb or full on an image that has no such rendition yet serves the " +
			"full image rather than a 404; any other unknown name is a 404.",
		Tags:     []string{"images"},
		Metadata: map[string]any{rateLimitTierKey: ratelimit.TierImage},
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
	return serveImageObject(ctx, mediaStore, &input.Params, imageObject{
		key:         img.StorageKey,
		contentType: img.ContentType,
		storedHash:  img.ContentSHA256,
		label:       "image " + img.ID,
	}, imageCacheControl, "image not found")
}

// imageObject is what serveImageObject needs to know about one stored
// rendition, whichever route resolved it.
type imageObject struct {
	key         string
	contentType string
	storedHash  *string // exercise_image_variants.content_sha256; nil = unknown
	label       string  // for logs
}

// serveImageObject answers a conditional GET for one stored object, reading
// the media store only when the answer is not a 304.
//
// The ETag is the first 32 hex characters of the content's sha256 (the
// value etagValueBytes has always produced), taken from the recorded hash
// when there is one. That is what lets a revalidation skip the storage read
// entirely: before migration 0005 every 304 downloaded and hashed the whole
// object first, costing a 200's storage read for a response with no body.
//
// Why a recorded hash is safe to trust: images are addressed by id and their
// bytes are not edited in place. The one path that does rewrite an object
// under an existing key, internal/seed's repair of a missing object, goes
// through store.UpsertImageVariant, which overwrites content_sha256 with the
// new bytes' hash or clears it to NULL, and NULL falls back to hashing the
// bytes. If bytes were ever replaced out of band without touching the row,
// the 200 path below notices: it re-hashes what it read, logs the mismatch
// and sends the true ETag, so a client that downloads the new bytes is never
// handed a tag describing the old ones. A client still holding the old tag
// could be told 304 until the row is corrected, which is no weaker than the
// promise `immutable` in imageCacheControl already makes: bytes under a
// live id are not to be replaced without updating the row.
func serveImageObject(ctx context.Context, mediaStore media.Store, params *conditional.Params, obj imageObject, cc, notFoundMsg string) (*GetImageOutput, error) {
	if obj.storedHash != nil {
		if notModified := checkNotModified(params, etagFromSHA256(*obj.storedHash), cc); notModified != nil {
			return nil, notModified
		}
	}

	rc, err := mediaStore.Get(ctx, obj.key)
	if err != nil {
		// A row whose object is absent is a 404, not a 500. Both backends
		// report it as media.ErrObjectNotFound (FilesystemStore wraps
		// os.ErrNotExist, R2Store maps types.NoSuchKey) specifically so this
		// distinction could be made, and until this check existed nothing in
		// the API consumed the sentinel — every missing object answered 500.
		//
		// It matters most in the cases that actually produce missing
		// objects: an incomplete cmd/migrate-media backfill, or a
		// partially-completed re-encode, leaves rows pointing at keys the
		// bucket never received. As a 5xx those are uncacheable at the CDN,
		// indistinguishable from the API being broken, and they bury a real
		// outage under thousands of routine errors.
		if errors.Is(err, media.ErrObjectNotFound) {
			log.Printf("%s: object %s missing from media store", obj.label, obj.key)
			return nil, huma.Error404NotFound(notFoundMsg)
		}
		log.Printf("media store get %s (%s): %v", obj.key, obj.label, err)
		return nil, huma.Error500InternalServerError("internal error")
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		log.Printf("read %s (%s): %v", obj.label, obj.key, err)
		return nil, huma.Error500InternalServerError("internal error")
	}

	tag := etagValueBytes(data)
	if obj.storedHash != nil && etagFromSHA256(*obj.storedHash) != tag {
		log.Printf("%s: recorded content_sha256 does not match object %s; serving the object's own ETag "+
			"(clear or correct the row's content_sha256)", obj.label, obj.key)
	}
	if notModified := checkNotModified(params, tag, cc); notModified != nil {
		return nil, notModified
	}

	return &GetImageOutput{
		ContentType:  obj.contentType,
		CacheControl: cc,
		ETag:         etagHeader(tag),
		Body:         data,
	}, nil
}

// etagFromSHA256 turns a recorded content_sha256 into the bare ETag
// etagValueBytes computes from the same bytes: the first 16 bytes, in hex.
// The migration's CHECK guarantees 64 characters.
func etagFromSHA256(hexSum string) string {
	return hexSum[:32]
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
// asking for a rendition this server does not make and silently receiving a
// full-resolution image sees a successful request while downloading many
// times the bytes it budgeted for, with nothing in the response to reveal
// the substitution.
//
// The renditions the server does make are different. Every new image gets a
// thumb, but existing ones only get theirs when cmd/backfill-thumbs runs
// after deploy, and an image whose thumb failed to encode has none. A client
// that builds /{id}/thumb itself, rather than reading it from
// Exercise.Images[].variants, must not get a broken image for that, so a
// missing thumb (or a missing full row, which says nothing about the bytes)
// is served from the image itself. See serveMissingRendition.
func GetImageVariant(ctx context.Context, s *store.Store, mediaStore media.Store, input *GetImageVariantInput) (*GetImageOutput, error) {
	variant, imageStatus, err := s.Media.GetImageVariant(ctx, input.ID, input.Variant)
	if errors.Is(err, pgx.ErrNoRows) && (input.Variant == model.VariantThumb || input.Variant == model.VariantFull) {
		return serveMissingRendition(ctx, s, mediaStore, input)
	}
	if err != nil {
		return nil, mapStoreError(err, "image variant not found")
	}
	// A variant is exactly as public as the image it belongs to.
	if err := requireVisibleImage(ctx, model.Image{ID: input.ID, Status: imageStatus}); err != nil {
		return nil, err
	}

	return serveImageObject(ctx, mediaStore, &input.Params, imageObject{
		key:         variant.StorageKey,
		contentType: variant.ContentType,
		storedHash:  variant.ContentSHA256,
		label:       "image " + input.ID + " variant " + input.Variant,
	}, imageCacheControl, "image variant not found")
}

// serveMissingRendition answers /{id}/full or /{id}/thumb for an image that
// has no variant row by that name, with the image's own (full) bytes.
//
// For full that is simply the same object, so it keeps the immutable cache
// policy. For thumb it is a stand-in, and caching it as immutable would pin
// the full-size bytes to the thumb URL in every browser and CDN for a year,
// long after the backfill has made the real thumb. So it goes out with the
// API's ordinary short policy (cacheControl, api.go) instead, and
// revalidation picks the thumb up once it exists: its ETag differs from the
// full image's, so the client's If-None-Match stops matching and it gets a
// 200 with the small bytes.
func serveMissingRendition(ctx context.Context, s *store.Store, mediaStore media.Store, input *GetImageVariantInput) (*GetImageOutput, error) {
	img, err := s.Media.GetImageByID(ctx, input.ID)
	if err != nil {
		return nil, mapStoreError(err, "image variant not found")
	}
	if err := requireVisibleImage(ctx, img); err != nil {
		return nil, err
	}
	cc := imageCacheControl
	if input.Variant != model.VariantFull {
		cc = cacheControl
	}
	return serveImageObject(ctx, mediaStore, &input.Params, imageObject{
		key:         img.StorageKey,
		contentType: img.ContentType,
		storedHash:  img.ContentSHA256,
		label:       "image " + img.ID + " (standing in for missing " + input.Variant + ")",
	}, cc, "image variant not found")
}
