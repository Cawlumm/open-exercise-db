package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2/humatest"

	"github.com/Cawlumm/open-exercise-db/internal/auth"
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// newSubmissionsAPI wires the real Register (no test-only routes) plus an app
// to submit as.
func newSubmissionsAPI(t *testing.T) (*store.Store, humatest.TestAPI, string) {
	t.Helper()
	s, testAPI := newAuthTestAPI(t)
	return s, testAPI, registerTestApp(t, s, "lyftr")
}

func postSubmission(t *testing.T, api humatest.TestAPI, token, body string) *httptestResponse {
	t.Helper()
	resp := api.Post("/api/v1/submissions",
		"User-Agent: "+testUA,
		"Authorization: Bearer "+token,
		"Content-Type: application/json",
		strings.NewReader(body),
	)
	return &httptestResponse{Code: resp.Code, Body: resp.Body.String()}
}

type httptestResponse struct {
	Code int
	Body string
}

const validPayload = `{"name":"Cable Fly","muscle_group":"chest","instructions":["Stand between the cables."]}`

func TestCreateSubmission_RequiresCredential(t *testing.T) {
	_, testAPI := newAuthTestAPI(t)

	// The write API is the whole reason auth exists. If this ever passes
	// anonymously, everything else in Phase 2 is decoration.
	resp := testAPI.Post("/api/v1/submissions",
		"User-Agent: "+testUA,
		"Content-Type: application/json",
		strings.NewReader(fmt.Sprintf(`{"type":"create","payload":%s}`, validPayload)),
	)
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous POST status = %d, want 401: %s", resp.Code, resp.Body.String())
	}
}

func TestCreateSubmission_QueuesPendingAndAttributesToApp(t *testing.T) {
	s, testAPI, token := newSubmissionsAPI(t)

	resp := postSubmission(t, testAPI, token,
		fmt.Sprintf(`{"type":"create","payload":%s,"app_user_ref":"u-42"}`, validPayload))
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.Code, resp.Body)
	}

	var got struct {
		ID         string `json:"id"`
		Status     string `json:"status"`
		AppUserRef string `json:"app_user_ref"`
	}
	if err := json.Unmarshal([]byte(resp.Body), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, resp.Body)
	}
	// Pending, always. A submitter must not be able to influence review state.
	if got.Status != "pending" {
		t.Errorf("status = %q, want pending", got.Status)
	}
	if got.AppUserRef != "u-42" {
		t.Errorf("app_user_ref = %q, want u-42 (echoed so an app can reconcile)", got.AppUserRef)
	}

	// The row must be attributed to the app, not to a user — that is what
	// every downstream abuse control keys on.
	keyID, _, _ := auth.ParseToken(token)
	app, err := s.Apps.GetByKeyID(context.Background(), keyID)
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	subs, total, err := s.Submissions.ListByApp(context.Background(), app.ID, nil, 10, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(subs) != 1 {
		t.Fatalf("stored %d submissions, want 1", total)
	}
	if subs[0].SubmittedBy != nil {
		t.Error("submission attributed to a user as well as an app")
	}
	if subs[0].SubmittedByApp == nil || *subs[0].SubmittedByApp != app.ID {
		t.Error("submission not attributed to the submitting app")
	}
}

func TestCreateSubmission_ValidationRejections(t *testing.T) {
	_, testAPI, token := newSubmissionsAPI(t)

	cases := map[string]string{
		"unknown type":            `{"type":"delete","payload":` + validPayload + `}`,
		"edit without target":     `{"type":"edit","payload":` + validPayload + `}`,
		"translation w/o locale":  `{"type":"translation","target_exercise_id":"3fa85f64-5717-4562-b3fc-2c963f66afa6","payload":` + validPayload + `}`,
		"create with target":      `{"type":"create","target_exercise_id":"3fa85f64-5717-4562-b3fc-2c963f66afa6","payload":` + validPayload + `}`,
		"payload is a string":     `{"type":"create","payload":"hello"}`,
		"payload is an array":     `{"type":"create","payload":[1,2,3]}`,
		"payload is empty object": `{"type":"create","payload":{}}`,
	}
	for name, body := range cases {
		resp := postSubmission(t, testAPI, token, body)
		if resp.Code != http.StatusUnprocessableEntity && resp.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 4xx validation: %s", name, resp.Code, resp.Body)
		}
	}
}

func TestCreateSubmission_OversizePayloadRejected(t *testing.T) {
	_, testAPI, token := newSubmissionsAPI(t)

	// The count caps do not address this at all: 50 pending submissions of
	// 10MB each is inside every limit and still unreviewable.
	huge := strings.Repeat("x", maxPayloadBytes+1)
	resp := postSubmission(t, testAPI, token,
		fmt.Sprintf(`{"type":"create","payload":{"name":%q}}`, huge))
	if resp.Code != http.StatusUnprocessableEntity && resp.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want the oversize payload rejected: %s", resp.Code, resp.Body[:min(200, len(resp.Body))])
	}
}

func TestCreateSubmission_PendingCapRefusesWith429(t *testing.T) {
	s, testAPI := newAuthTestAPI(t)

	// A deliberately tiny cap, so this exercises the limit rather than the
	// patience of whoever runs the suite.
	two := 2
	keyID, token, hash, err := auth.GenerateAppCredential()
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	if _, err := s.Apps.CreateApp(context.Background(), store.CreateAppInput{
		Name: "capped", ContactEmail: "capped@example.test",
		KeyID: keyID, KeyHash: hash, MaxPending: &two,
	}); err != nil {
		t.Fatalf("create app: %v", err)
	}

	for i := range 2 {
		resp := postSubmission(t, testAPI, token,
			fmt.Sprintf(`{"type":"create","payload":%s,"app_user_ref":"u-1"}`, validPayload))
		if resp.Code != http.StatusCreated {
			t.Fatalf("submission %d status = %d, want 201: %s", i+1, resp.Code, resp.Body)
		}
	}

	// The third exceeds the pending cap. 429 rather than 403: this is
	// temporary and resolves itself as the queue is reviewed, which is what
	// 429 tells a client and 403 does not.
	resp := postSubmission(t, testAPI, token,
		fmt.Sprintf(`{"type":"create","payload":%s,"app_user_ref":"u-1"}`, validPayload))
	if resp.Code != http.StatusTooManyRequests {
		t.Fatalf("over-cap status = %d, want 429: %s", resp.Code, resp.Body)
	}
	if !strings.Contains(resp.Body, "awaiting review") {
		t.Errorf("429 body does not explain the pending cap: %s", resp.Body)
	}
}

func TestListMySubmissions_ScopedToTheCallingApp(t *testing.T) {
	s, testAPI := newAuthTestAPI(t)
	mine := registerTestApp(t, s, "mine")
	theirs := registerTestApp(t, s, "theirs")

	for range 2 {
		if resp := postSubmission(t, testAPI, mine,
			fmt.Sprintf(`{"type":"create","payload":%s,"app_user_ref":"u-1"}`, validPayload)); resp.Code != http.StatusCreated {
			t.Fatalf("seed mine: %d %s", resp.Code, resp.Body)
		}
	}
	if resp := postSubmission(t, testAPI, theirs,
		fmt.Sprintf(`{"type":"create","payload":%s,"app_user_ref":"u-9"}`, validPayload)); resp.Code != http.StatusCreated {
		t.Fatalf("seed theirs: %d %s", resp.Code, resp.Body)
	}

	// An app must never be able to page through another app's queue, however
	// it phrases the request.
	resp := testAPI.Get("/api/v1/submissions/mine",
		"User-Agent: "+testUA, "Authorization: Bearer "+mine)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body.String())
	}
	var got struct {
		Submissions []struct {
			AppUserRef string `json:"app_user_ref"`
		} `json:"submissions"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Total != 2 {
		t.Errorf("total = %d, want 2 (only this app's own submissions)", got.Total)
	}
	for _, sub := range got.Submissions {
		if sub.AppUserRef == "u-9" {
			t.Error("another app's submission leaked into /submissions/mine")
		}
	}
}

func TestListMySubmissions_RequiresCredentialDespiteBeingAGet(t *testing.T) {
	_, testAPI := newAuthTestAPI(t)

	// A GET, so the method-based default would make it public. It carries an
	// explicit tag precisely because an app's queue is its own.
	resp := testAPI.Get("/api/v1/submissions/mine", "User-Agent: "+testUA)
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", resp.Code, resp.Body.String())
	}
}

func TestCreateSubmission_SuspendedAppRefused(t *testing.T) {
	s, testAPI, token := newSubmissionsAPI(t)
	suspend(t, s, token)

	// Suspension is a write-side lever, and this is the write side.
	resp := postSubmission(t, testAPI, token,
		fmt.Sprintf(`{"type":"create","payload":%s}`, validPayload))
	if resp.Code != http.StatusForbidden {
		t.Fatalf("suspended app status = %d, want 403: %s", resp.Code, resp.Body)
	}
}
