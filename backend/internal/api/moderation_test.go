package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// newModerationAPI gives an admin session (admins moderate too) plus an app
// credential to submit with.
func newModerationAPI(t *testing.T) (*store.Store, *pgxpool.Pool, humatest.TestAPI, string, string) {
	t.Helper()
	s, pool, api := newSignInAPI(t, "cawlumm", "cawlumm", 7000)
	adminToken, _, role := signIn(t, api)
	if role != "admin" {
		t.Fatalf("setup: role = %q, want admin", role)
	}
	appToken := registerTestApp(t, s, "submitting-app")
	return s, pool, api, adminToken, appToken
}

// submitAs queues a submission and returns its id.
func submitAs(t *testing.T, api humatest.TestAPI, appToken, body string) string {
	t.Helper()
	code, resp := postJSON(t, api, "/api/v1/submissions", appToken, body)
	if code != http.StatusCreated {
		t.Fatalf("submit = %d: %s", code, resp)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		t.Fatalf("decode submission: %v", err)
	}
	return out.ID
}

const createBody = `{"type":"create","payload":{"name":"Cable Woodchop","muscle_group":"abdominals","category":"strength","equipment":"cable","instructions":["Set the pulley high."]},"app_user_ref":"u-1"}`

func TestModeration_ApprovePublishesToTheReadAPI(t *testing.T) {
	_, _, api, adminToken, appToken := newModerationAPI(t)
	id := submitAs(t, api, appToken, createBody)

	// Not public yet — the guarantee the whole queue exists for.
	before := api.Get("/api/v1/exercises?q=woodchop", "User-Agent: "+testUA)
	if !strings.Contains(before.Body.String(), `"total":0`) {
		t.Fatalf("submission was public before approval: %s", before.Body.String())
	}

	code, body := postJSON(t, api, "/api/v1/admin/submissions/"+id+"/approve", adminToken, `{}`)
	if code != http.StatusOK {
		t.Fatalf("approve = %d: %s", code, body)
	}
	var res struct {
		ExerciseID string `json:"exercise_id"`
		RevisionID string `json:"revision_id"`
		Created    bool   `json:"created"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !res.Created || res.ExerciseID == "" || res.RevisionID == "" {
		t.Fatalf("approve result = %+v, want a newly created exercise", res)
	}

	// Now it is public. Approval is the only path from queue to public data,
	// so this is what proves that path actually works end to end.
	after := api.Get("/api/v1/exercises?q=woodchop", "User-Agent: "+testUA)
	if strings.Contains(after.Body.String(), `"total":0`) {
		t.Fatalf("approved submission is still not public: %s", after.Body.String())
	}
	if !strings.Contains(after.Body.String(), "Cable Woodchop") {
		t.Errorf("published exercise does not carry the proposed name: %s", after.Body.String())
	}
}

func TestModeration_RejectPublishesNothing(t *testing.T) {
	_, _, api, adminToken, appToken := newModerationAPI(t)
	id := submitAs(t, api, appToken, createBody)

	code, body := postJSON(t, api, "/api/v1/admin/submissions/"+id+"/reject", adminToken,
		`{"notes":"duplicate of an existing exercise"}`)
	if code != http.StatusOK {
		t.Fatalf("reject = %d: %s", code, body)
	}

	resp := api.Get("/api/v1/exercises?q=woodchop", "User-Agent: "+testUA)
	if !strings.Contains(resp.Body.String(), `"total":0`) {
		t.Errorf("rejected submission became public: %s", resp.Body.String())
	}
}

func TestModeration_DecidingTwiceIsAConflict(t *testing.T) {
	_, _, api, adminToken, appToken := newModerationAPI(t)
	id := submitAs(t, api, appToken, createBody)

	if code, b := postJSON(t, api, "/api/v1/admin/submissions/"+id+"/approve", adminToken, `{}`); code != http.StatusOK {
		t.Fatalf("first approve = %d: %s", code, b)
	}
	// 409, not 200 and not 404. A moderator who double-clicks, or a second
	// moderator arriving late, must be told nothing happened rather than left
	// believing they published something.
	code, body := postJSON(t, api, "/api/v1/admin/submissions/"+id+"/approve", adminToken, `{}`)
	if code != http.StatusConflict {
		t.Fatalf("second approve = %d, want 409: %s", code, body)
	}
	if code, _ := postJSON(t, api, "/api/v1/admin/submissions/"+id+"/reject", adminToken, `{}`); code != http.StatusConflict {
		t.Errorf("reject after approve = %d, want 409", code)
	}
}

func TestModeration_ConcurrentApprovalsPublishOnce(t *testing.T) {
	s, pool, api, adminToken, appToken := newModerationAPI(t)
	_ = adminToken
	id := submitAs(t, api, appToken, createBody)

	// What this proves, precisely, because an earlier version of it claimed
	// more than it delivered: eight callers racing to approve one submission
	// produce exactly one published exercise and one success.
	//
	// It does NOT prove the FOR UPDATE lock or the conditional status update
	// are what achieve that. Removing both leaves this test green, because
	// exercises.slug is UNIQUE — two concurrent creates collide on the
	// constraint and Postgres rejects the loser regardless. The locking is
	// still correct and still wanted: the *edit* path writes a revision with
	// no unique constraint to catch it, so nothing in the schema would stop
	// two concurrent edit approvals from stacking two revisions. That case is
	// not exercised here, and saying so is better than implying it is.
	const racers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	var succeeded int
	var exerciseIDs []string

	start := make(chan struct{})
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release them together
			res, err := s.Moderation.Approve(context.Background(), store.Decision{
				SubmissionID: id, ReviewerID: mustReviewerID(t, pool),
			})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				succeeded++
				exerciseIDs = append(exerciseIDs, res.ExerciseID)
			}
		}()
	}
	close(start)
	wg.Wait()

	if succeeded != 1 {
		t.Errorf("%d of %d concurrent approvals succeeded, want exactly 1", succeeded, racers)
	}

	// The stronger assertion: one submission must never become two exercises,
	// however many callers raced.
	var published int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM exercises e JOIN exercise_revisions r ON r.id = e.current_revision_id
		 WHERE r.name = 'Cable Woodchop'`).Scan(&published); err != nil {
		t.Fatalf("count published: %v", err)
	}
	if published != 1 {
		t.Errorf("one submission produced %d published exercises, want 1 (ids: %v)", published, exerciseIDs)
	}
}

func TestModeration_DiffShowsOnlyProposedFields(t *testing.T) {
	_, _, api, adminToken, appToken := newModerationAPI(t)

	// Publish something to edit.
	createID := submitAs(t, api, appToken, createBody)
	_, body := postJSON(t, api, "/api/v1/admin/submissions/"+createID+"/approve", adminToken, `{}`)
	var created struct {
		ExerciseID string `json:"exercise_id"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// An edit changing only the name. `level` is absent, so it is proposing
	// nothing about level — showing it as a change would invent one.
	editID := submitAs(t, api, appToken, fmt.Sprintf(
		`{"type":"edit","target_exercise_id":%q,"payload":{"name":"Cable Wood Chop"},"app_user_ref":"u-2"}`, created.ExerciseID))

	resp := api.Get("/api/v1/admin/submissions/"+editID, "User-Agent: "+testUA, "Authorization: Bearer "+adminToken)
	if resp.Code != http.StatusOK {
		t.Fatalf("get queued = %d: %s", resp.Code, resp.Body.String())
	}
	var out struct {
		HasCurrent bool `json:"has_current"`
		Diff       []struct {
			Field    string `json:"field"`
			Current  any    `json:"current"`
			Proposed any    `json:"proposed"`
		} `json:"diff"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode diff: %v", err)
	}
	if !out.HasCurrent {
		t.Error("has_current = false for an edit against a published exercise")
	}
	if len(out.Diff) != 1 {
		t.Fatalf("diff has %d entries, want exactly 1 (only name was proposed): %+v", len(out.Diff), out.Diff)
	}
	if out.Diff[0].Field != "name" {
		t.Errorf("diff field = %q, want name", out.Diff[0].Field)
	}
	if out.Diff[0].Current != "Cable Woodchop" || out.Diff[0].Proposed != "Cable Wood Chop" {
		t.Errorf("diff = %+v, want the old and new name", out.Diff[0])
	}
}

func TestModeration_BulkReportsEachOutcome(t *testing.T) {
	_, _, api, adminToken, appToken := newModerationAPI(t)

	good1 := submitAs(t, api, appToken, createBody)
	good2 := submitAs(t, api, appToken, strings.Replace(createBody, "Cable Woodchop", "Rope Pushdown", 1))
	const missing = "3fa85f64-5717-4562-b3fc-2c963f66afa6"

	code, body := postJSON(t, api, "/api/v1/admin/submissions/bulk", adminToken,
		fmt.Sprintf(`{"action":"reject","ids":[%q,%q,%q],"notes":"batch"}`, good1, missing, good2))
	if code != http.StatusOK {
		t.Fatalf("bulk = %d: %s", code, body)
	}
	var out struct {
		Succeeded int `json:"succeeded"`
		Failed    int `json:"failed"`
		Outcomes  []struct {
			SubmissionID string `json:"submission_id"`
			OK           bool   `json:"ok"`
			Error        string `json:"error"`
		} `json:"outcomes"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// One bad id must not discard the other two decisions, or a moderator has
	// to bisect their own batch to find the poison row.
	if out.Succeeded != 2 || out.Failed != 1 {
		t.Errorf("succeeded=%d failed=%d, want 2/1: %+v", out.Succeeded, out.Failed, out.Outcomes)
	}
	if len(out.Outcomes) != 3 {
		t.Errorf("outcomes = %d, want one per id", len(out.Outcomes))
	}
}

func TestModeration_RequiresModeratorRole(t *testing.T) {
	s, _, contribAPI := newSignInAPI(t, "", "not-a-moderator", 7001)
	contribToken, _, role := signIn(t, contribAPI)
	if role != "contributor" {
		t.Fatalf("setup role = %q", role)
	}
	appToken := registerTestApp(t, s, "app-for-role-test")

	for _, path := range []string{
		"/api/v1/admin/submissions/3fa85f64-5717-4562-b3fc-2c963f66afa6/approve",
		"/api/v1/admin/submissions/bulk",
	} {
		if code, b := postJSON(t, contribAPI, path, contribToken, `{"action":"approve","ids":["3fa85f64-5717-4562-b3fc-2c963f66afa6"]}`); code != http.StatusForbidden {
			t.Errorf("contributor %s = %d, want 403: %s", path, code, b)
		}
		// An app credential can never moderate — moderation is a judgement a
		// person makes.
		if code, b := postJSON(t, contribAPI, path, appToken, `{"action":"approve","ids":["3fa85f64-5717-4562-b3fc-2c963f66afa6"]}`); code != http.StatusForbidden {
			t.Errorf("app credential %s = %d, want 403: %s", path, code, b)
		}
	}

	resp := contribAPI.Get("/api/v1/admin/submissions", "User-Agent: "+testUA, "Authorization: Bearer "+contribToken)
	if resp.Code != http.StatusForbidden {
		t.Errorf("contributor queue list = %d, want 403", resp.Code)
	}
}

func TestModeration_RevertRestoresAnEarlierRevision(t *testing.T) {
	_, _, api, adminToken, appToken := newModerationAPI(t)

	createID := submitAs(t, api, appToken, createBody)
	_, body := postJSON(t, api, "/api/v1/admin/submissions/"+createID+"/approve", adminToken, `{}`)
	var first struct {
		ExerciseID string `json:"exercise_id"`
		RevisionID string `json:"revision_id"`
	}
	if err := json.Unmarshal([]byte(body), &first); err != nil {
		t.Fatalf("decode: %v", err)
	}

	editID := submitAs(t, api, appToken, fmt.Sprintf(
		`{"type":"edit","target_exercise_id":%q,"payload":{"name":"Renamed Entirely","muscle_group":"abdominals","category":"strength","equipment":"cable","instructions":["Changed."]},"app_user_ref":"u-3"}`, first.ExerciseID))
	if code, b := postJSON(t, api, "/api/v1/admin/submissions/"+editID+"/approve", adminToken, `{}`); code != http.StatusOK {
		t.Fatalf("approve edit = %d: %s", code, b)
	}

	renamed := api.Get("/api/v1/exercises/"+first.ExerciseID, "User-Agent: "+testUA)
	if !strings.Contains(renamed.Body.String(), "Renamed Entirely") {
		t.Fatalf("edit did not take effect: %s", renamed.Body.String())
	}

	// Revert is one pointer write — nothing was mutated, so the original
	// revision is still there to go back to.
	if code, b := postJSON(t, api,
		"/api/v1/admin/exercises/"+first.ExerciseID+"/revert/"+first.RevisionID, adminToken, `{}`); code != http.StatusOK {
		t.Fatalf("revert = %d: %s", code, b)
	}
	back := api.Get("/api/v1/exercises/"+first.ExerciseID, "User-Agent: "+testUA)
	if !strings.Contains(back.Body.String(), "Cable Woodchop") {
		t.Errorf("revert did not restore the original: %s", back.Body.String())
	}
}

func TestModeration_RevertRejectsAnotherExercisesRevision(t *testing.T) {
	_, _, api, adminToken, appToken := newModerationAPI(t)

	aID := submitAs(t, api, appToken, createBody)
	_, aBody := postJSON(t, api, "/api/v1/admin/submissions/"+aID+"/approve", adminToken, `{}`)
	bID := submitAs(t, api, appToken, strings.Replace(createBody, "Cable Woodchop", "Face Pull", 1))
	_, bBody := postJSON(t, api, "/api/v1/admin/submissions/"+bID+"/approve", adminToken, `{}`)

	var a, b struct {
		ExerciseID string `json:"exercise_id"`
		RevisionID string `json:"revision_id"`
	}
	_ = json.Unmarshal([]byte(aBody), &a)
	_ = json.Unmarshal([]byte(bBody), &b)

	// Without the ownership check this would silently replace exercise A's
	// entire content with exercise B's, and both ids look perfectly valid.
	code, body := postJSON(t, api, "/api/v1/admin/exercises/"+a.ExerciseID+"/revert/"+b.RevisionID, adminToken, `{}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("cross-exercise revert = %d, want 422: %s", code, body)
	}
}

// mustReviewerID returns any existing user id, for tests that need a reviewer
// but do not care which.
func mustReviewerID(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `SELECT id FROM users LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("find a reviewer: %v", err)
	}
	return id
}
