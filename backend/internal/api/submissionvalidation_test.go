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

	"github.com/Cawlumm/open-exercise-db/internal/auth"
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

func TestSubmission_ProvenanceFieldsAreValidatedOnEveryType(t *testing.T) {
	s, _, _, api, _ := newImageGateAPI(t)
	appToken := registerTestApp(t, s, "provenance-app")
	exerciseID := seedPublishedExercise(t, api, s, appToken)

	long := func(n int) string { return strings.Repeat("a", n) }
	bad := []struct{ name, fields string }{
		{"http url", `"source_url":"http://example.test/x"`},
		{"javascript url", `"source_url":"javascript:alert(1)"`},
		{"relative url", `"source_url":"/x"`},
		{"url with credentials", `"source_url":"https://user:pw@example.test/x"`},
		{"huge url", `"source_url":"https://example.test/` + long(2100) + `"`},
		{"cc-by license", `"license":"CC-BY-4.0"`},
		{"all rights reserved", `"license":"All rights reserved"`},
		{"lowercase cc0", `"license":"cc0-1.0"`},
		{"long attribution", `"attribution_text":"` + long(501) + `"`},
		{"long change summary", `"change_summary":"` + long(501) + `"`},
	}
	for _, c := range bad {
		for _, typ := range []string{"create", "edit", "translation"} {
			payload := `{"name":"Valid Name","muscle_group":"abdominals","category":"strength","equipment":"cable","instructions":["Do."],` + c.fields + `}`
			var body string
			switch typ {
			case "create":
				body = `{"type":"create","payload":` + payload + `}`
			case "edit":
				body = fmt.Sprintf(`{"type":"edit","target_exercise_id":%q,"payload":%s}`, exerciseID, payload)
			default:
				body = fmt.Sprintf(`{"type":"translation","locale":"es","target_exercise_id":%q,"payload":%s}`, exerciseID, payload)
			}
			if code, b := postJSON(t, api, "/api/v1/submissions", appToken, body); code != http.StatusUnprocessableEntity {
				t.Errorf("%s on %s = %d, want 422: %s", c.name, typ, code, b)
			}
		}
	}

	ok := `{"name":"Valid Name","muscle_group":"abdominals","category":"strength","equipment":"cable","instructions":["Do."],` +
		`"license":"Unlicense","source_url":"https://example.test/x","attribution_text":"me","change_summary":"typo"}`
	if code, b := postJSON(t, api, "/api/v1/submissions", appToken, `{"type":"create","payload":`+ok+`}`); code != http.StatusCreated {
		t.Errorf("valid provenance = %d: %s", code, b)
	}
	// Blank license is "not stated", which is allowed.
	blank := `{"name":"Blank License","muscle_group":"abdominals","category":"strength","equipment":"cable","instructions":["Do."],"license":""}`
	if code, b := postJSON(t, api, "/api/v1/submissions", appToken, `{"type":"create","payload":`+blank+`}`); code != http.StatusCreated {
		t.Errorf("blank license = %d: %s", code, b)
	}
}

// seedPublishedExercise publishes one exercise directly through the store so
// translation and edit submissions have a target.
func seedPublishedExercise(t *testing.T, api humatest.TestAPI, s *store.Store, appToken string) string {
	t.Helper()
	// Directly through the API's own create + approve would need an admin;
	// the import path is the supported way to make an exercise without one.
	res, err := s.Exercises.UpsertImport(context.Background(), store.UpsertImportInput{
		SourceID: "seed-" + t.Name(), Slug: "seed-target",
		Revision: store.ImportRevisionInput{
			Name: "Seed Target", MuscleGroup: "abdominals", Category: "strength", Equipment: "cable",
			Aliases: []string{}, SecondaryMuscles: []string{}, Instructions: []string{"Do."}, ImageIDs: []string{},
		},
	})
	if err != nil {
		t.Fatalf("seed exercise: %v", err)
	}
	return res.ExerciseID
}

func TestSubmission_TranslationUsesTheSameFieldRules(t *testing.T) {
	s, _, _, api, _ := newImageGateAPI(t)
	appToken := registerTestApp(t, s, "translation-rules-app")
	exerciseID := seedPublishedExercise(t, api, s, appToken)

	cases := map[string]string{
		"blank name":       `{"name":"  ","instructions":["Hacer."]}`,
		"missing name":     `{"instructions":["Hacer."]}`,
		"long name":        `{"name":"` + strings.Repeat("n", 121) + `","instructions":["Hacer."]}`,
		"blank alias":      `{"name":"Prensa","aliases":[" "],"instructions":["Hacer."]}`,
		"long instruction": `{"name":"Prensa","instructions":["` + strings.Repeat("i", 2001) + `"]}`,
		"too many steps":   `{"name":"Prensa","instructions":[` + strings.TrimSuffix(strings.Repeat(`"x",`, 41), ",") + `]}`,
	}
	for name, payload := range cases {
		body := fmt.Sprintf(`{"type":"translation","locale":"es","target_exercise_id":%q,"payload":%s}`, exerciseID, payload)
		if code, b := postJSON(t, api, "/api/v1/submissions", appToken, body); code != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d, want 422: %s", name, code, b)
		}
	}

	// Non-English content with no taxonomy fields is still fine.
	good := fmt.Sprintf(`{"type":"translation","locale":"es","target_exercise_id":%q,"payload":{"name":"Prensa Landmine","instructions":["Preparar."]}}`, exerciseID)
	if code, b := postJSON(t, api, "/api/v1/submissions", appToken, good); code != http.StatusCreated {
		t.Errorf("valid translation = %d: %s", code, b)
	}
}

func TestSubmission_LocaleIsParsedAndCanonicalized(t *testing.T) {
	s, pool, _, api, _ := newImageGateAPI(t)
	appToken := registerTestApp(t, s, "locale-app")
	exerciseID := seedPublishedExercise(t, api, s, appToken)

	submit := func(locale string) (int, string) {
		return postJSON(t, api, "/api/v1/submissions", appToken, fmt.Sprintf(
			`{"type":"translation","locale":%q,"target_exercise_id":%q,"payload":{"name":"Prensa","instructions":["Hacer."]}}`, locale, exerciseID))
	}

	for _, bad := range []string{"not a locale", "e", "123", "en-", "und", "xx-yy-zz-toolong-1234567890"} {
		if code, b := submit(bad); code != http.StatusUnprocessableEntity {
			t.Errorf("locale %q = %d, want 422: %s", bad, code, b)
		}
	}

	for in, want := range map[string]string{"en_us": "en-US", "ES": "es", "pt-br": "pt-BR", "zh-hant-tw": "zh-Hant-TW"} {
		code, b := submit(in)
		if code != http.StatusCreated {
			t.Errorf("locale %q = %d: %s", in, code, b)
			continue
		}
		var stored string
		if err := pool.QueryRow(context.Background(),
			`SELECT locale FROM submissions WHERE payload->>'name' = 'Prensa' ORDER BY created_at DESC LIMIT 1`).Scan(&stored); err != nil {
			t.Fatalf("read locale: %v", err)
		}
		if stored != want {
			t.Errorf("locale %q stored as %q, want %q", in, stored, want)
		}
	}
}

func TestSubmission_NULBytesAreA422Not500(t *testing.T) {
	s, _, _, api, _ := newImageGateAPI(t)
	appToken := registerTestApp(t, s, "nul-app")

	for name, body := range map[string]string{
		"in a value":        `{"type":"create","payload":{"name":"Bad\u0000Name","muscle_group":"abdominals","category":"strength","equipment":"cable","instructions":["Do."]}}`,
		"in a nested value": `{"type":"create","payload":{"name":"Fine","muscle_group":"abdominals","category":"strength","equipment":"cable","instructions":["Do.\u0000"]}}`,
		"in a key":          `{"type":"create","payload":{"name":"Fine","bad\u0000key":1,"muscle_group":"abdominals","category":"strength","equipment":"cable","instructions":["Do."]}}`,
		"in app_user_ref":   `{"type":"create","payload":{"name":"Fine","muscle_group":"abdominals","category":"strength","equipment":"cable","instructions":["Do."]},"app_user_ref":"u\u00001"}`,
	} {
		if code, b := postJSON(t, api, "/api/v1/submissions", appToken, body); code != http.StatusUnprocessableEntity {
			t.Errorf("NUL %s = %d, want 422: %s", name, code, b)
		}
	}

	// An escaped backslash followed by the text u0000 is not a NUL.
	ok := `{"type":"create","payload":{"name":"Back\\u0000slash","muscle_group":"abdominals","category":"strength","equipment":"cable","instructions":["Do."]}}`
	if code, b := postJSON(t, api, "/api/v1/submissions", appToken, ok); code != http.StatusCreated {
		t.Errorf("escaped backslash = %d, want 201: %s", code, b)
	}
}

// The cap check and the insert are one serialized step per app. Without the
// advisory lock every racer reads the same count and all of them pass.
func TestSubmission_ConcurrentSubmissionsCannotOvershootTheCap(t *testing.T) {
	s, pool := newTestStore(t)

	const cap, racers = 3, 16
	maxPending := cap
	keyID, _, hash, err := auth.GenerateAppCredential()
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	app, err := s.Apps.CreateApp(context.Background(), store.CreateAppInput{
		Name: "racing", ContactEmail: "r@example.test", KeyID: keyID, KeyHash: hash, MaxPending: &maxPending,
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted, refused := 0, 0
	start := make(chan struct{})
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.Submissions.CreateAppSubmissionWithinCaps(context.Background(), store.CreateAppSubmissionInput{
				Type: "create", AppID: app.ID, Payload: json.RawMessage(`{"name":"Racer"}`),
			}, capWindow, func(u store.AppUsage) error { return checkCaps(app, u) })
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				admitted++
			default:
				refused++
			}
		}()
	}
	close(start)
	wg.Wait()

	var stored int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM submissions WHERE submitted_by_app = $1`, app.ID).Scan(&stored); err != nil {
		t.Fatalf("count: %v", err)
	}
	if admitted != cap || stored != cap || refused != racers-cap {
		t.Errorf("admitted=%d stored=%d refused=%d, want exactly %d admitted and %d refused", admitted, stored, refused, cap, racers-cap)
	}
}
