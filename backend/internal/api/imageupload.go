package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/Cawlumm/open-exercise-db/internal/media"
	"github.com/Cawlumm/open-exercise-db/internal/model"
	"github.com/Cawlumm/open-exercise-db/internal/ratelimit"
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// maxUploadBytes bounds one uploaded image before any of it is decoded.
//
// Enforced against the declared body size and again while reading, because a
// Content-Length is a claim, not a fact. 10MB is generous for a photograph
// destined to be re-encoded down to 800px, and small enough that a hostile
// upload cannot exhaust a 512MiB instance.
const maxUploadBytes = 10 << 20

// uploadedImageTTLNote is the operational gap this endpoint knowingly leaves.
//
// An upload that is never referenced by a submission stays pending forever:
// nothing reaps it. R2's free tier is 10GB against 111MB in use, so this is a
// slow leak rather than an urgent one, but it is unbounded and there is no
// sweep. Recorded here rather than in a tracker nobody reads.
const uploadedImageTTLNote = "orphaned uploads are not yet reaped"

// acceptedLicenses are the licences an uploaded image may declare.
//
// The dataset ships under CC0 (see DATA_LICENSE). An image contributed under
// anything more restrictive makes that claim false for the whole corpus, and
// the person harmed is the downstream consumer who believed it. So the licence
// is required at upload, checked against a list, and stored — rather than
// collected as free text nobody validates.
//
// Unlicense and CC0 are both public-domain dedications; the bootstrap import
// carries Unlicense, so accepting it keeps contributed images on the same
// footing as the seeded ones.
var acceptedLicenses = map[string]bool{
	"CC0-1.0":   true,
	"Unlicense": true,
}

// UploadImageInput is a multipart upload.
//
// Multipart rather than base64 in JSON: a 10MB photograph becomes ~13MB of
// base64 that must be fully buffered and decoded before anything can be
// rejected, which converts a size cap into a memory cost.
type UploadImageInput struct {
	RawBody huma.MultipartFormFiles[struct {
		// No contentType allowlist. A part's declared MIME type is a claim by
		// the sender, so filtering on it would imply a guarantee it cannot
		// give — while rejecting genuine uploads from any client that does not
		// set a per-part header, which is most of them: Go's own
		// multipart.CreateFormFile defaults to application/octet-stream.
		// media.Encode decoding the bytes is the actual check, and it is the
		// one that cannot be lied to.
		File    huma.FormFile `form:"file" required:"true"`
		License string        `form:"license" required:"true"`
		// Explicitly optional: huma treats a form field as required unless
		// told otherwise, and an uploader photographing their own equipment
		// has nobody to credit. Forcing a value would just teach people to
		// type a placeholder.
		Attribution string `form:"attribution" required:"false"`
	}]
}

type UploadImageOutput struct {
	Status int
	Body   struct {
		ImageUploadID string `json:"image_upload_id" doc:"Reference this in a submission's image_upload_ids"`
		Width         int    `json:"width" doc:"After re-encoding, not as uploaded"`
		Height        int    `json:"height"`
		ContentType   string `json:"content_type"`
		Bytes         int    `json:"bytes" doc:"Size after re-encoding"`
		Status        string `json:"status" doc:"Always pending: an upload is not public until a moderator approves it"`
	}
}

// registerImageUploadRoutes wires POST /api/v1/images.
func registerImageUploadRoutes(hapi huma.API, s *store.Store, mediaStore media.Store) {
	huma.Register(hapi, huma.Operation{
		OperationID: "upload-image",
		Method:      http.MethodPost,
		Path:        "/api/v1/images",
		Summary:     "Upload an image for use in a submission",
		Description: "Returns an id to reference from a submission's image_upload_ids. " +
			"The image is re-encoded on arrival and is not publicly served until a moderator approves the submission that uses it.",
		Tags: []string{"images"},
		Metadata: map[string]any{
			// The export tier, not search: this is the most expensive request
			// this API accepts — it buffers megabytes, decodes them, re-encodes
			// them, and writes to object storage. Budgeting it like a search
			// would let one client saturate an instance with work no cache can
			// absorb.
			rateLimitTierKey: ratelimit.TierExport,
			authRequiredKey:  true,
		},
		Errors: []int{
			http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden,
			http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity,
			http.StatusTooManyRequests,
		},
	}, func(ctx context.Context, in *UploadImageInput) (*UploadImageOutput, error) {
		app, ok := AppFromContext(ctx)
		if !ok {
			// A signed-in person may upload too, but attribution and quota
			// both key on the app today. Rejecting rather than silently
			// storing an unattributed image: an upload nobody is accountable
			// for is exactly what the quota exists to prevent.
			return nil, huma.Error403Forbidden("uploading currently requires an app credential")
		}

		form := in.RawBody.Data()

		license := strings.TrimSpace(form.License)
		if !acceptedLicenses[license] {
			return nil, huma.Error422UnprocessableEntity(fmt.Sprintf(
				"license must be one of CC0-1.0 or Unlicense (got %q) — this dataset is published under CC0 and cannot carry more restrictive images",
				license))
		}

		file := form.File
		if file.Size > maxUploadBytes {
			return nil, huma.Error413RequestEntityTooLarge(fmt.Sprintf(
				"image is %d bytes; the limit is %d", file.Size, maxUploadBytes))
		}

		if err := checkUploadQuota(ctx, s, app); err != nil {
			return nil, err
		}

		// Re-encode from the decoded pixels, and store only that. This is the
		// single most valuable line here: it strips EXIF, which on a phone
		// photo taken in a gym carries GPS coordinates the uploader did not
		// think they were publishing; and it defuses a polyglot, since a file
		// that is both a valid JPEG and a valid script stops being either once
		// it has been rebuilt from an image.Image. It also means a file that
		// merely claims to be an image fails here rather than being stored.
		encoded, err := media.Encode(newLimitedReader(file, maxUploadBytes))
		if err != nil {
			if errors.Is(err, errUploadTooLarge) {
				return nil, huma.Error413RequestEntityTooLarge(fmt.Sprintf(
					"image exceeds the %d byte limit", maxUploadBytes))
			}
			// Decode failure is the caller's problem, not a server fault: the
			// bytes were not a supported image whatever the part declared.
			return nil, huma.Error422UnprocessableEntity("file is not a readable JPEG, PNG or GIF")
		}

		key := uploadStorageKey(app.ID, time.Now())
		if err := mediaStore.Put(ctx, key, bytes.NewReader(encoded.Data), encoded.ContentType); err != nil {
			log.Printf("upload: put %s for app %s: %v", key, app.ID, err)
			return nil, huma.Error500InternalServerError("internal error")
		}

		var attribution *string
		if a := strings.TrimSpace(form.Attribution); a != "" {
			attribution = &a
		}
		width, height := encoded.Width, encoded.Height
		imageID, err := s.Media.InsertImage(ctx, store.InsertImageInput{
			StorageKey:      key,
			ContentType:     encoded.ContentType,
			Width:           &width,
			Height:          &height,
			License:         &license,
			AttributionText: attribution,
			// Pending, always. The row exists so a submission can reference
			// it; api.requireVisibleImage is what keeps the bytes unreachable
			// until somebody has looked at them.
			Status: model.ImageStatusPending,
		})
		if err != nil {
			// The object is now written and the row is not. Left alone that is
			// an orphan in storage rather than a broken reference, which is
			// the right way round: nothing points at it and the next cleanup
			// pass can find it by key prefix.
			log.Printf("upload: insert row for %s: %v (object written, now orphaned)", key, err)
			return nil, mapStoreError(err, "")
		}

		out := &UploadImageOutput{Status: http.StatusCreated}
		out.Body.ImageUploadID = imageID
		out.Body.Width = encoded.Width
		out.Body.Height = encoded.Height
		out.Body.ContentType = encoded.ContentType
		out.Body.Bytes = len(encoded.Data)
		out.Body.Status = model.ImageStatusPending
		return out, nil
	})
}

// maxPendingUploadsPerApp bounds how many unreferenced uploads an app may
// accumulate.
//
// Separate from the submission caps because images cost storage and bandwidth
// in a way a JSON payload does not: an app inside its submission quota can
// still upload thousands of photographs, and nothing reaps the ones it never
// references. Ten times the pending-submission cap leaves generous room for an
// app that uploads several images per exercise while still bounding the leak.
const pendingUploadMultiplier = 10

func checkUploadQuota(ctx context.Context, s *store.Store, app model.App) error {
	pending, err := s.Media.CountPendingUploadsByApp(ctx, app.ID)
	if err != nil {
		return mapStoreError(err, "")
	}
	limit := app.MaxPending * pendingUploadMultiplier
	if pending >= limit {
		return huma.Error429TooManyRequests(fmt.Sprintf(
			"you have %d images awaiting review and the limit is %d; %s",
			pending, limit, uploadedImageTTLNote))
	}
	return nil
}

// uploadStorageKey namespaces contributed images away from imported ones.
//
// "uploads/<app>/<timestamp>-<nanos>" rather than the importer's
// "exercises/<source-id>/<n>": those keys are derived from upstream identity
// and are stable across re-imports, which is exactly what a contributed image
// has no equivalent of. Keeping the two prefixes apart also means a cleanup
// sweep for orphaned uploads can be written as a prefix scan without any risk
// of touching the 1,746 imported objects.
func uploadStorageKey(appID string, now time.Time) string {
	return fmt.Sprintf("uploads/%s/%d-%d.jpg", appID, now.Unix(), now.Nanosecond())
}
