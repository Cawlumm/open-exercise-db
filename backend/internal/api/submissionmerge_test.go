package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// uploadTestImage uploads a real PNG as app and returns its image_upload_id.
func uploadTestImage(t *testing.T, api humatest.TestAPI, appToken string) string {
	t.Helper()
	body, ct := multipartImage(t, realPNG(t, 64, 48), "CC0-1.0", "")
	resp := api.Post("/api/v1/images", "User-Agent: "+testUA, "Authorization: Bearer "+appToken,
		"Content-Type: "+ct, bytes.NewReader(body))
	if resp.Code != http.StatusCreated {
		t.Fatalf("upload = %d: %s", resp.Code, resp.Body.String())
	}
	var up struct {
		ImageUploadID string `json:"image_upload_id"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &up); err != nil {
		t.Fatalf("decode upload: %v", err)
	}
	return up.ImageUploadID
}

func approve(t *testing.T, api humatest.TestAPI, adminToken, submissionID string) (exerciseID string) {
	t.Helper()
	code, body := postJSON(t, api, "/api/v1/admin/submissions/"+submissionID+"/approve", adminToken, `{}`)
	if code != http.StatusOK {
		t.Fatalf("approve = %d: %s", code, body)
	}
	var res struct {
		ExerciseID string `json:"exercise_id"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatalf("decode approve: %v", err)
	}
	return res.ExerciseID
}

type currentRevision struct {
	Name, MuscleGroup, Category, Equipment string
	Instructions, Aliases, Secondary       []string
	ImageIDs                               []string
	License, Attribution, SourceURL        *string
	Revisions                              int
}

func readCurrent(t *testing.T, pool *pgxpool.Pool, exerciseID string) currentRevision {
	t.Helper()
	var c currentRevision
	err := pool.QueryRow(context.Background(), `
		SELECT r.name, r.muscle_group, r.category, r.equipment, r.instructions, r.aliases,
		       r.secondary_muscles, r.image_ids::text[], r.license, r.attribution_text, r.source_url,
		       (SELECT count(*) FROM exercise_revisions WHERE exercise_id = e.id)
		FROM exercises e JOIN exercise_revisions r ON r.id = e.current_revision_id
		WHERE e.id = $1`, exerciseID).Scan(
		&c.Name, &c.MuscleGroup, &c.Category, &c.Equipment, &c.Instructions, &c.Aliases,
		&c.Secondary, &c.ImageIDs, &c.License, &c.Attribution, &c.SourceURL, &c.Revisions)
	if err != nil {
		t.Fatalf("read current revision: %v", err)
	}
	return c
}

func imageStatus(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var st string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM exercise_images WHERE id = $1`, id).Scan(&st); err != nil {
		t.Fatalf("image status: %v", err)
	}
	return st
}

func fullCreate(imageID string) string {
	return `{"type":"create","payload":{"name":"Landmine Press","muscle_group":"shoulders","category":"strength","equipment":"barbell",` +
		`"instructions":["Brace.","Press."],"aliases":["Angled Press"],"secondary_muscles":["triceps"],` +
		`"license":"CC0-1.0","attribution_text":"Original text","source_url":"https://example.test/landmine"},` +
		`"image_upload_ids":["` + imageID + `"]}`
}

func TestEdit_NameOnlyKeepsEveryOtherField(t *testing.T) {
	s, pool, _, api, adminToken := newImageGateAPI(t)
	appToken := registerTestApp(t, s, "edit-merge-app")
	imageID := uploadTestImage(t, api, appToken)

	exerciseID := approve(t, api, adminToken, submitAs(t, api, appToken, fullCreate(imageID)))
	before := readCurrent(t, pool, exerciseID)
	if len(before.ImageIDs) != 1 || before.License == nil {
		t.Fatalf("setup: created revision = %+v", before)
	}

	editID := submitAs(t, api, appToken, fmt.Sprintf(
		`{"type":"edit","target_exercise_id":%q,"payload":{"name":"Landmine Shoulder Press"}}`, exerciseID))
	approve(t, api, adminToken, editID)

	after := readCurrent(t, pool, exerciseID)
	if after.Revisions != 2 {
		t.Fatalf("revisions = %d, want 2", after.Revisions)
	}
	if after.Name != "Landmine Shoulder Press" {
		t.Errorf("name = %q, edit did not apply", after.Name)
	}
	// The regression: every one of these used to be blanked, because the
	// edit payload was decoded into value types and published whole.
	if after.MuscleGroup != "shoulders" || after.Category != "strength" || after.Equipment != "barbell" {
		t.Errorf("taxonomy blanked: %+v", after)
	}
	if strings.Join(after.Instructions, "|") != "Brace.|Press." {
		t.Errorf("instructions = %v", after.Instructions)
	}
	if strings.Join(after.Aliases, "|") != "Angled Press" || strings.Join(after.Secondary, "|") != "triceps" {
		t.Errorf("aliases/secondary = %v / %v", after.Aliases, after.Secondary)
	}
	if len(after.ImageIDs) != 1 || after.ImageIDs[0] != imageID {
		t.Errorf("image_ids = %v, want [%s]", after.ImageIDs, imageID)
	}
	if after.License == nil || *after.License != "CC0-1.0" ||
		after.Attribution == nil || *after.Attribution != "Original text" ||
		after.SourceURL == nil || *after.SourceURL != "https://example.test/landmine" {
		t.Errorf("provenance blanked: %+v", after)
	}
}

func TestEdit_NewImageIsAppendedNotReplacing(t *testing.T) {
	s, pool, _, api, adminToken := newImageGateAPI(t)
	appToken := registerTestApp(t, s, "edit-append-app")
	first := uploadTestImage(t, api, appToken)
	exerciseID := approve(t, api, adminToken, submitAs(t, api, appToken, fullCreate(first)))

	second := uploadTestImage(t, api, appToken)
	approve(t, api, adminToken, submitAs(t, api, appToken, fmt.Sprintf(
		`{"type":"edit","target_exercise_id":%q,"payload":{"change_summary":"add photo"},"image_upload_ids":[%q]}`, exerciseID, second)))

	got := readCurrent(t, pool, exerciseID)
	if len(got.ImageIDs) != 2 || got.ImageIDs[0] != first || got.ImageIDs[1] != second {
		t.Errorf("image_ids = %v, want [%s %s]", got.ImageIDs, first, second)
	}
}

func TestDiff_ShowsEveryStoredFieldThatWouldChange(t *testing.T) {
	s, _, _, api, adminToken := newImageGateAPI(t)
	appToken := registerTestApp(t, s, "diff-app")
	imageID := uploadTestImage(t, api, appToken)
	exerciseID := approve(t, api, adminToken, submitAs(t, api, appToken, fullCreate(imageID)))

	newImage := uploadTestImage(t, api, appToken)
	editID := submitAs(t, api, appToken, fmt.Sprintf(
		`{"type":"edit","target_exercise_id":%q,"payload":{"license":"Unlicense","attribution_text":"New text","source_url":"https://example.test/other"},"image_upload_ids":[%q]}`,
		exerciseID, newImage))

	resp := api.Get("/api/v1/admin/submissions/"+editID, "User-Agent: "+testUA, "Authorization: Bearer "+adminToken)
	if resp.Code != http.StatusOK {
		t.Fatalf("get queued = %d: %s", resp.Code, resp.Body.String())
	}
	var out struct {
		Diff []struct {
			Field string `json:"field"`
		} `json:"diff"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var got []string
	for _, d := range out.Diff {
		got = append(got, d.Field)
	}
	want := "image_ids,license,attribution_text,source_url"
	if strings.Join(got, ",") != want {
		t.Errorf("diff fields = %v, want %s (and nothing the payload did not touch)", got, want)
	}
}

func TestApprove_PublishesTheSubmissionsImages(t *testing.T) {
	s, pool, _, api, adminToken := newImageGateAPI(t)
	appToken := registerTestApp(t, s, "approve-image-app")
	imageID := uploadTestImage(t, api, appToken)

	pending, err := s.Media.CountPendingUploadsByApp(context.Background(), appIDByName(t, pool, "approve-image-app"))
	if err != nil || pending != 1 {
		t.Fatalf("setup: pending uploads = %d (%v), want 1", pending, err)
	}
	if st := imageStatus(t, pool, imageID); st != "pending" {
		t.Fatalf("setup: image status = %q", st)
	}

	approve(t, api, adminToken, submitAs(t, api, appToken, fullCreate(imageID)))

	if st := imageStatus(t, pool, imageID); st != "approved" {
		t.Errorf("image status after approval = %q, want approved", st)
	}
	pending, err = s.Media.CountPendingUploadsByApp(context.Background(), appIDByName(t, pool, "approve-image-app"))
	if err != nil || pending != 0 {
		t.Errorf("pending uploads after approval = %d (%v), want 0", pending, err)
	}
}

func TestReject_LeavesTheSubmissionsImagesUnapproved(t *testing.T) {
	s, pool, _, api, adminToken := newImageGateAPI(t)
	appToken := registerTestApp(t, s, "reject-image-app")
	imageID := uploadTestImage(t, api, appToken)
	id := submitAs(t, api, appToken, fullCreate(imageID))

	if code, b := postJSON(t, api, "/api/v1/admin/submissions/"+id+"/reject", adminToken, `{"notes":"no"}`); code != http.StatusOK {
		t.Fatalf("reject = %d: %s", code, b)
	}
	if st := imageStatus(t, pool, imageID); st != "pending" {
		t.Errorf("image status after rejection = %q, want pending (never published)", st)
	}
}

func appIDByName(t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `SELECT id FROM apps WHERE name = $1`, name).Scan(&id); err != nil {
		t.Fatalf("find app %s: %v", name, err)
	}
	return id
}
