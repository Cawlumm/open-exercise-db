package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2/humatest"
)

func postJSON(t *testing.T, api humatest.TestAPI, path, token, body string) (int, string) {
	t.Helper()
	resp := api.Post(path,
		"User-Agent: "+testUA,
		"Authorization: Bearer "+token,
		"Content-Type: application/json",
		strings.NewReader(body),
	)
	return resp.Code, resp.Body.String()
}

func TestRegisterApp_AdminCanRegisterAndTheTokenWorks(t *testing.T) {
	_, _, api := newSignInAPI(t, "cawlumm", "cawlumm", 900)
	adminToken, _, role := signIn(t, api)
	if role != "admin" {
		t.Fatalf("setup: role = %q, want admin", role)
	}

	code, body := postJSON(t, api, "/api/v1/admin/apps", adminToken,
		`{"name":"lyftr","contact_email":"dev@lyftr.test"}`)
	if code != http.StatusCreated {
		t.Fatalf("register status = %d, want 201: %s", code, body)
	}

	var out struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		KeyID string `json:"key_id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if out.Token == "" {
		t.Fatal("no token returned; the credential would be unrecoverable")
	}
	// The hash is secret material and must never be published.
	if strings.Contains(body, "key_hash") {
		t.Errorf("registration response leaks key_hash: %s", body)
	}

	// The whole point: the freshly minted credential can actually submit.
	subCode, subBody := postJSON(t, api, "/api/v1/submissions", out.Token,
		fmt.Sprintf(`{"type":"create","payload":%s,"app_user_ref":"u-1"}`, validPayload))
	if subCode != http.StatusCreated {
		t.Fatalf("submitting with the new credential = %d, want 201: %s", subCode, subBody)
	}
}

func TestRegisterApp_RefusedWithoutAdminRole(t *testing.T) {
	s, _, api := newSignInAPI(t, "", "contributor-person", 901)
	contributorToken, _, role := signIn(t, api)
	if role != "contributor" {
		t.Fatalf("setup: role = %q, want contributor", role)
	}

	// A signed-in person who is not an admin.
	code, body := postJSON(t, api, "/api/v1/admin/apps", contributorToken,
		`{"name":"sneaky","contact_email":"a@b.test"}`)
	if code != http.StatusForbidden {
		t.Errorf("contributor register status = %d, want 403: %s", code, body)
	}

	// An app credential, which authenticates but can never hold a role —
	// otherwise any app could mint further apps and multiply its own budget.
	appToken := registerTestApp(t, s, "an-app")
	code, body = postJSON(t, api, "/api/v1/admin/apps", appToken,
		`{"name":"sneakier","contact_email":"a@b.test"}`)
	if code != http.StatusForbidden {
		t.Errorf("app-credential register status = %d, want 403: %s", code, body)
	}

	// And anonymously.
	resp := api.Post("/api/v1/admin/apps",
		"User-Agent: "+testUA,
		"Content-Type: application/json",
		strings.NewReader(`{"name":"anon","contact_email":"a@b.test"}`),
	)
	if resp.Code != http.StatusUnauthorized {
		t.Errorf("anonymous register status = %d, want 401: %s", resp.Code, resp.Body.String())
	}
}

func TestRegisterApp_DuplicateNameIsConflict(t *testing.T) {
	_, _, api := newSignInAPI(t, "cawlumm", "cawlumm", 902)
	adminToken, _, _ := signIn(t, api)

	body := `{"name":"duplicate","contact_email":"a@b.test"}`
	if code, b := postJSON(t, api, "/api/v1/admin/apps", adminToken, body); code != http.StatusCreated {
		t.Fatalf("first register = %d: %s", code, b)
	}
	// 409, not 500: "pick another name" is something the caller can act on.
	code, b := postJSON(t, api, "/api/v1/admin/apps", adminToken, body)
	if code != http.StatusConflict {
		t.Fatalf("duplicate register = %d, want 409: %s", code, b)
	}
}

func TestRotateAppKey_OldCredentialStopsWorking(t *testing.T) {
	_, _, api := newSignInAPI(t, "cawlumm", "cawlumm", 903)
	adminToken, _, _ := signIn(t, api)

	_, body := postJSON(t, api, "/api/v1/admin/apps", adminToken,
		`{"name":"rotating","contact_email":"a@b.test"}`)
	var created struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	code, rotBody := postJSON(t, api, "/api/v1/admin/apps/"+created.ID+"/rotate", adminToken, `{}`)
	if code != http.StatusOK {
		t.Fatalf("rotate = %d, want 200: %s", code, rotBody)
	}
	var rotated struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(rotBody), &rotated); err != nil {
		t.Fatalf("decode rotated: %v", err)
	}
	if rotated.Token == created.Token {
		t.Fatal("rotation returned the same token")
	}

	// The old credential must stop working immediately — that is what makes
	// rotation a response to a leak rather than a formality.
	oldCode, _ := postJSON(t, api, "/api/v1/submissions", created.Token,
		fmt.Sprintf(`{"type":"create","payload":%s}`, validPayload))
	if oldCode != http.StatusUnauthorized {
		t.Errorf("old credential status = %d, want 401", oldCode)
	}
	newCode, newBody := postJSON(t, api, "/api/v1/submissions", rotated.Token,
		fmt.Sprintf(`{"type":"create","payload":%s}`, validPayload))
	if newCode != http.StatusCreated {
		t.Errorf("new credential status = %d, want 201: %s", newCode, newBody)
	}
}

func TestSetAppStatus_SuspendBlocksWritesNotReads(t *testing.T) {
	_, _, api := newSignInAPI(t, "cawlumm", "cawlumm", 904)
	adminToken, _, _ := signIn(t, api)

	_, body := postJSON(t, api, "/api/v1/admin/apps", adminToken,
		`{"name":"suspendable","contact_email":"a@b.test"}`)
	var created struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	code, b := postJSON(t, api, "/api/v1/admin/apps/"+created.ID+"/status", adminToken, `{"status":"suspended"}`)
	if code != http.StatusOK {
		t.Fatalf("suspend = %d, want 200: %s", code, b)
	}

	// Writes stop...
	wCode, _ := postJSON(t, api, "/api/v1/submissions", created.Token,
		fmt.Sprintf(`{"type":"create","payload":%s}`, validPayload))
	if wCode != http.StatusForbidden {
		t.Errorf("suspended write = %d, want 403", wCode)
	}
	// ...and reads keep working, because suspension is a moderation decision
	// about submissions, not an outage for that app's users.
	rResp := api.Get("/api/v1/meta/equipment", "User-Agent: "+testUA, "Authorization: Bearer "+created.Token)
	if rResp.Code != http.StatusOK {
		t.Errorf("suspended read = %d, want 200: %s", rResp.Code, rResp.Body.String())
	}

	// Reactivation restores writes.
	if code, b := postJSON(t, api, "/api/v1/admin/apps/"+created.ID+"/status", adminToken, `{"status":"active"}`); code != http.StatusOK {
		t.Fatalf("reactivate = %d: %s", code, b)
	}
	if wCode, wBody := postJSON(t, api, "/api/v1/submissions", created.Token,
		fmt.Sprintf(`{"type":"create","payload":%s}`, validPayload)); wCode != http.StatusCreated {
		t.Errorf("reactivated write = %d, want 201: %s", wCode, wBody)
	}
}

func TestAdminApps_UnknownIDIs404(t *testing.T) {
	_, _, api := newSignInAPI(t, "cawlumm", "cawlumm", 905)
	adminToken, _, _ := signIn(t, api)

	const missing = "3fa85f64-5717-4562-b3fc-2c963f66afa6"
	// An UPDATE affecting no rows is not a failed scan, so without an explicit
	// sentinel these would be 500s.
	if code, b := postJSON(t, api, "/api/v1/admin/apps/"+missing+"/rotate", adminToken, `{}`); code != http.StatusNotFound {
		t.Errorf("rotate unknown = %d, want 404: %s", code, b)
	}
	if code, b := postJSON(t, api, "/api/v1/admin/apps/"+missing+"/status", adminToken, `{"status":"suspended"}`); code != http.StatusNotFound {
		t.Errorf("status unknown = %d, want 404: %s", code, b)
	}
}

func TestRegisterApp_HonoursExplicitCaps(t *testing.T) {
	_, _, api := newSignInAPI(t, "cawlumm", "cawlumm", 906)
	adminToken, _, _ := signIn(t, api)

	_, body := postJSON(t, api, "/api/v1/admin/apps", adminToken,
		`{"name":"tight","contact_email":"a@b.test","max_pending":1,"max_per_day":5}`)
	var created struct {
		Token      string `json:"token"`
		MaxPending int    `json:"max_pending"`
		MaxPerDay  int    `json:"max_per_day"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if created.MaxPending != 1 || created.MaxPerDay != 5 {
		t.Fatalf("caps = (%d,%d), want (1,5)", created.MaxPending, created.MaxPerDay)
	}

	// And the cap is actually enforced for this app specifically, rather than
	// the default being silently applied.
	if code, b := postJSON(t, api, "/api/v1/submissions", created.Token,
		fmt.Sprintf(`{"type":"create","payload":%s}`, validPayload)); code != http.StatusCreated {
		t.Fatalf("first submission = %d: %s", code, b)
	}
	if code, _ := postJSON(t, api, "/api/v1/submissions", created.Token,
		fmt.Sprintf(`{"type":"create","payload":%s}`, validPayload)); code != http.StatusTooManyRequests {
		t.Errorf("second submission = %d, want 429 at max_pending=1", code)
	}
}

// Guard the invariant that makes all of the above meaningful.
func TestAdminRoutes_AreNotReachableWithoutSignInConfigured(t *testing.T) {
	s, testAPI := newAuthTestAPI(t) // AuthConfig{} — no JWT secret at all
	appToken := registerTestApp(t, s, "an-app")

	// With sign-in unconfigured there is no way to hold a role, so the admin
	// surface must be closed rather than accidentally open.
	code, body := postJSON(t, testAPI, "/api/v1/admin/apps", appToken,
		`{"name":"x","contact_email":"a@b.test"}`)
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", code, body)
	}
}
