package api

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"testing"
)

// realPNG builds an actual image, so the re-encode path runs for real rather
// than against bytes that merely claim to be a picture.
func realPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 120, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func TestUpload_RejectsAnonymous(t *testing.T) {
	_, _, _, api, _ := newImageGateAPI(t)

	body, ct := multipartImage(t, realPNG(t, 40, 30), "CC0-1.0", "")
	resp := api.Post("/api/v1/images", "User-Agent: "+testUA, "Content-Type: "+ct, bytes.NewReader(body))
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous upload = %d, want 401: %s", resp.Code, resp.Body.String())
	}
}

func TestUpload_StoresPendingAndReEncodes(t *testing.T) {
	s, _, _, api, modToken := newImageGateAPI(t)
	appToken := registerTestApp(t, s, "uploading-app")

	// Deliberately larger than the 800px cap so the resize is observable.
	body, ct := multipartImage(t, realPNG(t, 1200, 900), "CC0-1.0", "A Contributor")
	resp := api.Post("/api/v1/images", "User-Agent: "+testUA, "Authorization: Bearer "+appToken,
		"Content-Type: "+ct, bytes.NewReader(body))
	if resp.Code != http.StatusCreated {
		t.Fatalf("upload = %d, want 201: %s", resp.Code, resp.Body.String())
	}

	var out struct {
		ImageUploadID string `json:"image_upload_id"`
		Width         int    `json:"width"`
		Height        int    `json:"height"`
		ContentType   string `json:"content_type"`
		Status        string `json:"status"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, resp.Body.String())
	}

	// Re-encoded, not stored as sent: a PNG went in and a capped JPEG came
	// out. That transformation is what strips EXIF and defuses a polyglot.
	if out.ContentType != "image/jpeg" {
		t.Errorf("content_type = %q, want image/jpeg — the upload was not re-encoded", out.ContentType)
	}
	if out.Width != 800 {
		t.Errorf("width = %d, want 800 — a 1200px upload should have been capped", out.Width)
	}
	if out.Status != "pending" {
		t.Errorf("status = %q, want pending", out.Status)
	}

	// And it is not public yet — the gate and the upload agree.
	if r := api.Get("/api/v1/images/"+out.ImageUploadID, "User-Agent: "+testUA); r.Code != http.StatusNotFound {
		t.Errorf("freshly uploaded image is publicly served (%d) — it must not be", r.Code)
	}
	if r := api.Get("/api/v1/images/"+out.ImageUploadID, "User-Agent: "+testUA,
		"Authorization: Bearer "+modToken); r.Code != http.StatusOK {
		t.Errorf("moderator cannot see the pending upload (%d)", r.Code)
	}
}

func TestUpload_RejectsNonImages(t *testing.T) {
	s, _, _, api, _ := newImageGateAPI(t)
	appToken := registerTestApp(t, s, "junk-app")

	// A file claiming to be a JPEG that is not one. Nothing but decoding the
	// bytes can tell the difference, which is why the decode is the check.
	body, ct := multipartImage(t, []byte("MZ\x90\x00 this is an executable, not a picture"), "CC0-1.0", "")
	resp := api.Post("/api/v1/images", "User-Agent: "+testUA, "Authorization: Bearer "+appToken,
		"Content-Type: "+ct, bytes.NewReader(body))
	if resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("non-image upload = %d, want 422: %s", resp.Code, resp.Body.String())
	}
}

func TestUpload_RequiresAnAcceptableLicense(t *testing.T) {
	s, _, _, api, _ := newImageGateAPI(t)
	appToken := registerTestApp(t, s, "licensing-app")

	// The dataset is CC0. An image under anything more restrictive makes that
	// claim false for the whole corpus, and the person harmed is a downstream
	// consumer who believed it.
	for _, license := range []string{"", "CC-BY-4.0", "All rights reserved", "MIT"} {
		body, ct := multipartImage(t, realPNG(t, 40, 30), license, "")
		resp := api.Post("/api/v1/images", "User-Agent: "+testUA, "Authorization: Bearer "+appToken,
			"Content-Type: "+ct, bytes.NewReader(body))
		if resp.Code != http.StatusUnprocessableEntity && resp.Code != http.StatusBadRequest {
			t.Errorf("license %q accepted with %d, want rejection", license, resp.Code)
		}
	}

	// Unlicense is accepted, because the 1,746 imported images carry it and
	// contributed images should stand on the same footing.
	body, ct := multipartImage(t, realPNG(t, 40, 30), "Unlicense", "")
	if resp := api.Post("/api/v1/images", "User-Agent: "+testUA, "Authorization: Bearer "+appToken,
		"Content-Type: "+ct, bytes.NewReader(body)); resp.Code != http.StatusCreated {
		t.Errorf("Unlicense rejected with %d: %s", resp.Code, resp.Body.String())
	}
}

func TestUpload_AttachesToASubmission(t *testing.T) {
	s, pool, _, api, _ := newImageGateAPI(t)
	appToken := registerTestApp(t, s, "attaching-app")

	body, ct := multipartImage(t, realPNG(t, 200, 150), "CC0-1.0", "")
	resp := api.Post("/api/v1/images", "User-Agent: "+testUA, "Authorization: Bearer "+appToken,
		"Content-Type: "+ct, bytes.NewReader(body))
	if resp.Code != http.StatusCreated {
		t.Fatalf("upload = %d: %s", resp.Code, resp.Body.String())
	}
	var up struct {
		ImageUploadID string `json:"image_upload_id"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &up); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// The point of the whole feature: an upload id a submission can carry.
	// Until this worked, a contributor could not attach a photograph at all.
	code, subBody := postJSON(t, api, "/api/v1/submissions", appToken,
		`{"type":"create","payload":{"name":"Uploaded Exercise","muscle_group":"abdominals","category":"strength","equipment":"cable","instructions":["Do it."]},"image_upload_ids":["`+up.ImageUploadID+`"]}`)
	if code != http.StatusCreated {
		t.Fatalf("submission with an image = %d: %s", code, subBody)
	}

	var stored int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM submissions WHERE $1 = ANY(image_upload_ids)`, up.ImageUploadID).Scan(&stored); err != nil {
		t.Fatalf("count: %v", err)
	}
	if stored != 1 {
		t.Errorf("submission does not reference the uploaded image (%d rows)", stored)
	}
}

// The check checkImageOwnership exists for. An app that could reference an id
// it did not upload could attach a stranger's unreviewed photograph to its own
// submission — and on approval that image is published under the other app's
// storage key, with this app's submission as the only record of how it got
// there. Enumeration is the milder half: ids are uuids, but a distinguishable
// "no such image" would still confirm which ones are real.
func TestUpload_CannotAttachAnotherAppsImage(t *testing.T) {
	s, _, _, api, _ := newImageGateAPI(t)
	victim := registerTestApp(t, s, "victim-app")
	thief := registerTestApp(t, s, "thief-app")

	body, ct := multipartImage(t, realPNG(t, 60, 60), "CC0-1.0", "")
	resp := api.Post("/api/v1/images", "User-Agent: "+testUA, "Authorization: Bearer "+victim,
		"Content-Type: "+ct, bytes.NewReader(body))
	if resp.Code != http.StatusCreated {
		t.Fatalf("victim upload = %d: %s", resp.Code, resp.Body.String())
	}
	var up struct {
		ImageUploadID string `json:"image_upload_id"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &up); err != nil {
		t.Fatalf("decode: %v", err)
	}

	sub := func(id string) (int, string) {
		return postJSON(t, api, "/api/v1/submissions", thief,
			`{"type":"create","payload":{"name":"Borrowed `+id[:8]+`","muscle_group":"abdominals","category":"strength","equipment":"cable","instructions":["Do it."]},"image_upload_ids":["`+id+`"]}`)
	}

	if code, b := sub(up.ImageUploadID); code != http.StatusUnprocessableEntity {
		t.Errorf("another app's image accepted with %d, want 422: %s", code, b)
	}

	// An id that exists nowhere must fail identically — same status, same
	// message — or the difference between the two is the oracle.
	if code, b := sub("00000000-0000-4000-8000-000000000000"); code != http.StatusUnprocessableEntity {
		t.Errorf("nonexistent image id = %d, want 422: %s", code, b)
	}
}

// multipartImage builds a multipart body carrying one file plus the licence
// fields, returning the body and its Content-Type.
func multipartImage(t *testing.T, data []byte, license, attribution string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	// A well-behaved client declares the part's type. The endpoint does not
	// rely on it — see the File field's comment — but sending it keeps this
	// test representative of a real caller rather than of the one shortcut
	// Go's helper happens to take.
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="file"; filename="upload.png"`)
	h.Set("Content-Type", "image/png")
	fw, err := w.CreatePart(h)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatalf("write file part: %v", err)
	}
	if license != "" {
		if err := w.WriteField("license", license); err != nil {
			t.Fatalf("write license: %v", err)
		}
	}
	if attribution != "" {
		if err := w.WriteField("attribution", attribution); err != nil {
			t.Fatalf("write attribution: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return buf.Bytes(), w.FormDataContentType()
}
