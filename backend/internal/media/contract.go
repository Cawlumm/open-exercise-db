package media

import (
	"strings"

	"github.com/Cawlumm/open-exercise-db/internal/contract"
	"github.com/Cawlumm/open-exercise-db/internal/model"
)

// ImageURL builds the client-fetchable URL for a re-hosted image, per
// docs/DESIGN.md's Open-Food-Facts-aligned design (see "Alignment with Open
// Food Facts" there): OFF serves product images from a separate host
// (images.openfoodfacts.org) so image traffic never touches the API
// process. baseURL is internal/config.Config.ImageBaseURL (env
// IMAGE_BASE_URL) — when set (production, once an R2/CDN host exists), the
// URL points directly at object storage using storageKey, the same key
// Store.Put/Get use; when unset (local dev, self-host with no CDN
// configured yet), it falls back to this API's own GET /api/v1/images/{id}
// route (internal/api/images.go), which serves the identical bytes through
// Store.Get. Either way the shape of contract.Exercise.Images doesn't
// change — only which host the URL points at.
//
// Not to be confused with internal/seed.Options.ImageBaseURL, an unrelated
// setting that resolves free-exercise-db's *upstream* relative image paths
// during import — the two happen to share a name in different packages but
// serve opposite ends of the pipeline (resolving a source URL to fetch from,
// vs. resolving a destination URL to serve from).
func ImageURL(baseURL, id, storageKey string) string {
	if baseURL == "" {
		return "/api/v1/images/" + id
	}
	return strings.TrimRight(baseURL, "/") + "/" + storageKey
}

// BuildImages converts store-layer image rows into the public
// contract.Image shape GET /exercises, GET /exercises/{id}, and GET
// /exercises/export all render via contract.Exercise.Images. One shared
// implementation for both internal/api (resolveExercise) and internal/export
// (assembleExercise) — unlike internal/contract.Translation's per-field
// resolution (which the two packages deliberately re-derive in parallel
// because they start from different store shapes), URL construction is a
// single policy decision that must not drift between the two callers, so it
// isn't duplicated the same way.
//
// Order is preserved from imgs (which callers fetch via
// store.MediaStore.ListImagesByIDs, itself ordered to match a revision's
// image_ids[] array) — this is the only place that ordering is used, so it's
// worth calling out rather than assuming.
func BuildImages(imgs []model.Image, baseURL string) []contract.Image {
	if len(imgs) == 0 {
		return nil
	}
	out := make([]contract.Image, 0, len(imgs))
	for _, img := range imgs {
		out = append(out, contract.Image{
			ID:              img.ID,
			URL:             ImageURL(baseURL, img.ID, img.StorageKey),
			Width:           img.Width,
			Height:          img.Height,
			ContentType:     img.ContentType,
			License:         img.License,
			AttributionText: img.AttributionText,
		})
	}
	return out
}
