package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// The export dump is what mirrors consume precisely so they need not poll. If
// approving a submission does not rebuild it, those mirrors serve the
// pre-approval dataset and have no way to know a newer one exists — the stale
// copy looks exactly like a current one.
//
// These drive the real HTTP endpoints rather than calling Cache.Invalidate
// directly, because the bug was never in the cache: Invalidate worked, nothing
// called it.

func TestExportCache_ApprovalRebuildsTheDump(t *testing.T) {
	_, _, api, adminToken, appToken := newModerationAPI(t)

	// Warm the cache so there is something stale to catch.
	before := api.Get("/api/v1/exercises/export", "User-Agent: "+testUA)
	if before.Code != http.StatusOK {
		t.Fatalf("initial export = %d", before.Code)
	}
	if strings.Contains(before.Body.String(), "Export Cache Probe") {
		t.Fatal("fixture leaked into the export before it was approved")
	}

	id := submitAs(t, api, appToken, `{"type":"create","payload":{"name":"Export Cache Probe","muscle_group":"abdominals","category":"strength","equipment":"cable","instructions":["Probe."]}}`)

	// Still absent while pending — the queue is separate from published data,
	// and the dump is published data.
	pending := api.Get("/api/v1/exercises/export", "User-Agent: "+testUA)
	if strings.Contains(pending.Body.String(), "Export Cache Probe") {
		t.Fatal("a pending submission appeared in the export")
	}

	if code, b := postJSON(t, api, "/api/v1/admin/submissions/"+id+"/approve", adminToken, `{}`); code != http.StatusOK {
		t.Fatalf("approve = %d: %s", code, b)
	}

	// The assertion that matters. Without the Invalidate call this fails for
	// up to DefaultCacheTTL — fifteen minutes of every mirror being wrong.
	after := api.Get("/api/v1/exercises/export", "User-Agent: "+testUA)
	if after.Code != http.StatusOK {
		t.Fatalf("export after approval = %d", after.Code)
	}
	if !strings.Contains(after.Body.String(), "Export Cache Probe") {
		t.Error("approved exercise is missing from the export — the cache was not invalidated")
	}
}

func TestExportCache_RevertRebuildsTheDump(t *testing.T) {
	_, _, api, adminToken, appToken := newModerationAPI(t)

	createID := submitAs(t, api, appToken, `{"type":"create","payload":{"name":"Revert Dump Probe","muscle_group":"abdominals","category":"strength","equipment":"cable","instructions":["First."]}}`)
	_, body := postJSON(t, api, "/api/v1/admin/submissions/"+createID+"/approve", adminToken, `{}`)
	var first struct {
		ExerciseID string `json:"exercise_id"`
		RevisionID string `json:"revision_id"`
	}
	if err := json.Unmarshal([]byte(body), &first); err != nil {
		t.Fatalf("decode: %v", err)
	}

	editID := submitAs(t, api, appToken, fmt.Sprintf(
		`{"type":"edit","target_exercise_id":%q,"payload":{"name":"Renamed In Dump","muscle_group":"abdominals","category":"strength","equipment":"cable","instructions":["Second."]}}`, first.ExerciseID))
	if code, b := postJSON(t, api, "/api/v1/admin/submissions/"+editID+"/approve", adminToken, `{}`); code != http.StatusOK {
		t.Fatalf("approve edit = %d: %s", code, b)
	}
	if !strings.Contains(api.Get("/api/v1/exercises/export", "User-Agent: "+testUA).Body.String(), "Renamed In Dump") {
		t.Fatal("edit missing from export")
	}

	if code, b := postJSON(t, api, "/api/v1/admin/exercises/"+first.ExerciseID+"/revert/"+first.RevisionID, adminToken, `{}`); code != http.StatusOK {
		t.Fatalf("revert = %d: %s", code, b)
	}

	// A revert changes what the API returns, so the dump has to follow it back.
	dump := api.Get("/api/v1/exercises/export", "User-Agent: "+testUA).Body.String()
	if strings.Contains(dump, "Renamed In Dump") {
		t.Error("reverted name still in the export")
	}
	if !strings.Contains(dump, "Revert Dump Probe") {
		t.Error("original name did not return to the export after revert")
	}
}

func TestExportCache_BulkApprovalRebuildsOnce(t *testing.T) {
	_, _, api, adminToken, appToken := newModerationAPI(t)

	a := submitAs(t, api, appToken, `{"type":"create","payload":{"name":"Bulk Dump One","muscle_group":"abdominals","category":"strength","equipment":"cable","instructions":["A."]}}`)
	b := submitAs(t, api, appToken, `{"type":"create","payload":{"name":"Bulk Dump Two","muscle_group":"abdominals","category":"strength","equipment":"cable","instructions":["B."]}}`)

	api.Get("/api/v1/exercises/export", "User-Agent: "+testUA) // warm

	if code, resp := postJSON(t, api, "/api/v1/admin/submissions/bulk", adminToken,
		fmt.Sprintf(`{"action":"approve","ids":[%q,%q]}`, a, b)); code != http.StatusOK {
		t.Fatalf("bulk = %d: %s", code, resp)
	}

	dump := api.Get("/api/v1/exercises/export", "User-Agent: "+testUA).Body.String()
	for _, name := range []string{"Bulk Dump One", "Bulk Dump Two"} {
		if !strings.Contains(dump, name) {
			t.Errorf("%q missing from the export after bulk approval", name)
		}
	}
}

func TestExportCache_RejectionDoesNotAppear(t *testing.T) {
	_, _, api, adminToken, appToken := newModerationAPI(t)

	id := submitAs(t, api, appToken, `{"type":"create","payload":{"name":"Rejected Dump Probe","muscle_group":"abdominals","category":"strength","equipment":"cable","instructions":["No."]}}`)
	if code, b := postJSON(t, api, "/api/v1/admin/submissions/"+id+"/reject", adminToken, `{}`); code != http.StatusOK {
		t.Fatalf("reject = %d: %s", code, b)
	}

	// Rejection publishes nothing, so it must not add anything to the dump.
	// The invalidation itself is harmless either way; what would not be
	// harmless is a rejected proposal turning up in the export.
	if strings.Contains(api.Get("/api/v1/exercises/export", "User-Agent: "+testUA).Body.String(), "Rejected Dump Probe") {
		t.Error("a rejected submission appeared in the export")
	}
}

// GET /exercises is cached by the same rule as the export: every write that
// publishes must clear it, or a client asking the same question gets the
// pre-publish answer for up to a day.
func TestListCache_PublishingRefreshesTheList(t *testing.T) {
	_, _, api, adminToken, appToken := newModerationAPI(t)
	list := func(query string) string {
		resp := api.Get("/api/v1/exercises?"+query, "User-Agent: "+testUA)
		if resp.Code != http.StatusOK {
			t.Fatalf("list %s = %d: %s", query, resp.Code, resp.Body.String())
		}
		return resp.Body.String()
	}

	createID := submitAs(t, api, appToken, `{"type":"create","payload":{"name":"List Cache Probe","muscle_group":"abdominals","category":"strength","equipment":"cable","instructions":["First."]}}`)
	if strings.Contains(list("q=list+cache+probe"), "List Cache Probe") {
		t.Fatal("a pending submission appeared in the list")
	}
	_, body := postJSON(t, api, "/api/v1/admin/submissions/"+createID+"/approve", adminToken, `{}`)
	var first struct {
		ExerciseID string `json:"exercise_id"`
		RevisionID string `json:"revision_id"`
	}
	if err := json.Unmarshal([]byte(body), &first); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if !strings.Contains(list("q=list+cache+probe"), "List Cache Probe") {
		t.Fatal("approve: the cached empty search was served after publishing")
	}

	byID := "ids=" + first.ExerciseID
	list(byID) // warm
	editID := submitAs(t, api, appToken, fmt.Sprintf(
		`{"type":"edit","target_exercise_id":%q,"payload":{"name":"List Cache Renamed","muscle_group":"abdominals","category":"strength","equipment":"cable","instructions":["Second."]}}`, first.ExerciseID))
	if code, resp := postJSON(t, api, "/api/v1/admin/submissions/bulk", adminToken,
		fmt.Sprintf(`{"action":"approve","ids":[%q]}`, editID)); code != http.StatusOK {
		t.Fatalf("bulk = %d: %s", code, resp)
	}
	if !strings.Contains(list(byID), "List Cache Renamed") {
		t.Fatal("bulk approve: the list still served the pre-edit name")
	}

	if code, b := postJSON(t, api, "/api/v1/admin/exercises/"+first.ExerciseID+"/revert/"+first.RevisionID, adminToken, `{}`); code != http.StatusOK {
		t.Fatalf("revert = %d: %s", code, b)
	}
	if got := list(byID); strings.Contains(got, "List Cache Renamed") || !strings.Contains(got, "List Cache Probe") {
		t.Fatal("revert: the list still served the reverted name")
	}
}
