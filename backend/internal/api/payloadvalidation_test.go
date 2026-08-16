package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2/humatest"
)

// newValidationAPI gives a submitting app, a seeded vocabulary, and one
// published exercise to aim edits at.
//
// The vocabulary matters more than it looks: store.UnknownSlugs deliberately
// fails OPEN on a facet with no terms at all, so an empty taxonomy makes the
// whole taxonomy check a no-op. An earlier version of these tests asserted
// rejections against an unseeded database and passed nothing — every "invalid
// value" sailed through with a 201, and the tests were measuring the fixture
// rather than the code.
func newValidationAPI(t *testing.T) (humatest.TestAPI, string, string) {
	t.Helper()
	s, pool, api := newSignInAPI(t, "cawlumm", "cawlumm", 8100)
	adminToken, _, _ := signIn(t, api)
	appToken := registerTestApp(t, s, "validating-app")

	ctx := context.Background()
	for _, term := range []struct{ facet, slug string }{
		{"muscle_group", "abdominals"}, {"muscle_group", "shoulders"},
		{"category", "strength"}, {"equipment", "cable"},
		{"mechanic", "compound"}, {"force", "pull"}, {"level", "beginner"},
	} {
		if _, err := s.Taxonomy.UpsertTerm(ctx, term.facet, term.slug); err != nil {
			t.Fatalf("seed %s/%s: %v", term.facet, term.slug, err)
		}
	}

	// A published exercise for edits and translations to target. Created
	// through the real approve path so it is a genuine published record
	// rather than a hand-built row.
	subID := submitAs(t, api, appToken,
		`{"type":"create","payload":{"name":"Seed Exercise","muscle_group":"abdominals","category":"strength","equipment":"cable","instructions":["Do the thing."]}}`)
	_, body := postJSON(t, api, "/api/v1/admin/submissions/"+subID+"/approve", adminToken, `{}`)
	var created struct {
		ExerciseID string `json:"exercise_id"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("decode approve: %v (%s)", err, body)
	}
	if created.ExerciseID == "" {
		t.Fatalf("no exercise created: %s", body)
	}
	_ = pool
	return api, appToken, created.ExerciseID
}

func TestValidation_RejectsUnknownTaxonomyValues(t *testing.T) {
	testAPI, token, targetID := newValidationAPI(t)
	_ = targetID

	// The case this whole file exists for. muscle_group is plain TEXT with no
	// foreign key, so without this check an approved "banana" publishes an
	// exercise that appears in no filter and no /meta listing — invisible
	// rather than broken, which is worse because nothing reports it.
	code, body := postSubmissionRaw(t, testAPI, token,
		`{"type":"create","payload":{"name":"Nonsense Row","muscle_group":"banana","category":"strength","equipment":"cable","instructions":["Pull."]}}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", code, body)
	}
	if !strings.Contains(body, "banana") || !strings.Contains(body, "muscle-groups") {
		t.Errorf("error names neither the bad value nor where to look: %s", body)
	}
}

func TestValidation_ReportsEveryProblemAtOnce(t *testing.T) {
	testAPI, token, targetID := newValidationAPI(t)
	_ = targetID

	// Three bad facets. A caller fixing them one round trip at a time is a
	// worse experience than one message listing all three.
	code, body := postSubmissionRaw(t, testAPI, token,
		`{"type":"create","payload":{"name":"Bad","muscle_group":"banana","category":"nonsense","equipment":"unobtainium","instructions":["Do."]}}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", code, body)
	}
	for _, want := range []string{"banana", "nonsense", "unobtainium"} {
		if !strings.Contains(body, want) {
			t.Errorf("error omits %q, so a caller would fix one problem per request: %s", want, body)
		}
	}
}

func TestValidation_CreateRequiresTheEssentialFields(t *testing.T) {
	testAPI, token, targetID := newValidationAPI(t)
	_ = targetID

	code, body := postSubmissionRaw(t, testAPI, token,
		`{"type":"create","payload":{"name":"Only A Name"}}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", code, body)
	}
	for _, want := range []string{"muscle_group", "category", "equipment", "instructions"} {
		if !strings.Contains(body, want) {
			t.Errorf("error does not mention missing %s: %s", want, body)
		}
	}
}

func TestValidation_EditMayProposeASubset(t *testing.T) {
	testAPI, token, targetID := newValidationAPI(t)
	_ = targetID

	// An edit correcting one typo must not have to resubmit the whole record.
	code, body := postSubmissionRaw(t, testAPI, token, fmt.Sprintf(
		`{"type":"edit","target_exercise_id":%q,"payload":{"name":"A Better Name"}}`, targetID))
	if code != http.StatusCreated {
		t.Fatalf("partial edit = %d, want 201: %s", code, body)
	}
}

func TestValidation_RejectsBlankAndOversizeText(t *testing.T) {
	testAPI, token, targetID := newValidationAPI(t)
	_ = targetID

	cases := map[string]string{
		"blank name":        `{"type":"create","payload":{"name":"   ","muscle_group":"abdominals","category":"strength","equipment":"cable","instructions":["Do."]}}`,
		"blank instruction": `{"type":"create","payload":{"name":"X","muscle_group":"abdominals","category":"strength","equipment":"cable","instructions":["Do.","  "]}}`,
		"oversize name":     fmt.Sprintf(`{"type":"create","payload":{"name":%q,"muscle_group":"abdominals","category":"strength","equipment":"cable","instructions":["Do."]}}`, strings.Repeat("x", maxNameLen+1)),
		"too many aliases":  `{"type":"create","payload":{"name":"X","muscle_group":"abdominals","category":"strength","equipment":"cable","instructions":["Do."],"aliases":["a","b","c","d","e","f","g","h","i","j","k","l","m","n","o","p","q","r","s","t","u"]}}`,
	}
	for name, body := range cases {
		code, resp := postSubmissionRaw(t, testAPI, token, body)
		if code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422: %s", name, code, resp)
		}
	}
}

func TestValidation_WrongTypesNameTheField(t *testing.T) {
	testAPI, token, targetID := newValidationAPI(t)
	_ = targetID

	// instructions as a string rather than an array. The message should say
	// which field, not echo a decoder error.
	code, body := postSubmissionRaw(t, testAPI, token,
		`{"type":"create","payload":{"name":"X","muscle_group":"abdominals","category":"strength","equipment":"cable","instructions":"do the thing"}}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", code, body)
	}
	if !strings.Contains(body, "instructions") {
		t.Errorf("error does not name the offending field: %s", body)
	}
}

func TestValidation_AcceptsAWellFormedSubmission(t *testing.T) {
	testAPI, token, targetID := newValidationAPI(t)
	_ = targetID

	// The guard against over-strict validation: a legitimate submission using
	// real vocabulary must still sail through. Every rejection test above is
	// worthless if this one fails.
	code, body := postSubmissionRaw(t, testAPI, token,
		`{"type":"create","payload":{"name":"Cable Woodchop","muscle_group":"abdominals","category":"strength","equipment":"cable","mechanic":"compound","instructions":["Set the pulley high.","Rotate through the torso."],"secondary_muscles":["abdominals"]},"app_user_ref":"u-1"}`)
	if code != http.StatusCreated {
		t.Fatalf("valid submission rejected with %d: %s", code, body)
	}
}

func TestValidation_TranslationsSkipTaxonomyChecks(t *testing.T) {
	testAPI, token, targetID := newValidationAPI(t)
	_ = targetID

	// A translation carries a name and instructions in another language, not
	// taxonomy. Running it through the English vocabulary would reject every
	// translation ever submitted.
	code, body := postSubmissionRaw(t, testAPI, token, fmt.Sprintf(
		`{"type":"translation","target_exercise_id":%q,"locale":"es","payload":{"name":"Leñador con polea","instructions":["Coloca la polea arriba."]}}`, targetID))
	if code != http.StatusCreated {
		t.Fatalf("translation rejected with %d: %s", code, body)
	}
}

// postSubmissionRaw posts a body verbatim and returns status plus body.
func postSubmissionRaw(t *testing.T, api humatest.TestAPI, token, body string) (int, string) {
	t.Helper()
	return postJSON(t, api, "/api/v1/submissions", token, body)
}
