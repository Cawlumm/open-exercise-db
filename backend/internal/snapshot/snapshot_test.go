package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cawlumm/open-exercise-db/internal/api"
)

type fakeAPI struct {
	en         []string
	locales    map[string][]string // non-en code -> ids
	status     int                 // non-zero: every request answers this
	total      map[string]int      // override reported totals
	fail429    int32               // number of initial 429s
	badStatus  string
	fail5xx    int32
	retryHdr   string
	noStatus   bool
	noLocTotal bool
	hits       int32
	uas        []string
}

func ex(id string) string {
	return fmt.Sprintf(`{"id":%q,"status":"published","name":"n-%s","images":[{"url":"/api/v1/images/%s"}]}`, id, id, id)
}

func (f *fakeAPI) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.hits, 1)
		f.uas = append(f.uas, r.Header.Get("User-Agent"))
		if f.status != 0 {
			w.WriteHeader(f.status)
			return
		}
		if atomic.LoadInt32(&f.fail5xx) > 0 {
			atomic.AddInt32(&f.fail5xx, -1)
			w.WriteHeader(503)
			return
		}
		if atomic.LoadInt32(&f.fail429) > 0 {
			atomic.AddInt32(&f.fail429, -1)
			hdr := f.retryHdr
			if hdr == "" {
				hdr = "999"
			}
			w.Header().Set("Retry-After", hdr)
			w.WriteHeader(429)
			return
		}
		body := func(ids []string, key string, total int) {
			var parts []string
			for _, id := range ids {
				if f.noStatus {
					parts = append(parts, strings.Replace(ex(id), `"status":"published",`, "", 1))
				} else if f.badStatus != "" {
					parts = append(parts, strings.Replace(ex(id), "published", f.badStatus, 1))
				} else {
					parts = append(parts, ex(id))
				}
			}
			if t, ok := f.total[key]; ok {
				total = t
			}
			fmt.Fprintf(w, `{"$schema":"x","exercises":[%s],"total":%d,"page":1,"per_page":100,"generated_at":"t"}`, strings.Join(parts, ","), total)
		}
		switch r.URL.Path {
		case "/api/v1/meta/locales":
			var ls []string
			ls = append(ls, `{"code":"en"}`)
			for c := range f.locales {
				ls = append(ls, fmt.Sprintf(`{"code":%q}`, c))
			}
			if f.noLocTotal {
				fmt.Fprintf(w, `{"locales":[%s]}`, strings.Join(ls, ","))
				return
			}
			fmt.Fprintf(w, `{"locales":[%s],"total":%d}`, strings.Join(ls, ","), len(ls))
		case "/api/v1/exercises/export":
			body(f.en, "en", len(f.en))
		case "/api/v1/exercises":
			code := r.URL.Query().Get("locale")
			ids := f.locales[code]
			page := 1
			fmt.Sscan(r.URL.Query().Get("page"), &page)
			per := 2 // force pagination
			lo, hi := (page-1)*per, page*per
			if lo > len(ids) {
				lo = len(ids)
			}
			if hi > len(ids) {
				hi = len(ids)
			}
			body(ids[lo:hi], code, len(ids))
		default:
			w.WriteHeader(404)
		}
	})
}

func run(t *testing.T, f *fakeAPI, out string, confirm bool) (*Result, *Config, error) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	var slept []time.Duration
	cfg := &Config{
		API: srv.URL, Out: out, ConfirmRemovals: confirm,
		Now:   func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) },
		Sleep: func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil },
	}
	res, err := Build(context.Background(), cfg)
	t.Logf("slept=%v", slept)
	return res, cfg, err
}

func newAPI() *fakeAPI {
	return &fakeAPI{en: []string{"c", "a", "b"}, locales: map[string][]string{"es": {"b", "c", "a"}}}
}

func listFiles(t *testing.T, out string) []string {
	t.Helper()
	var names []string
	filepath.Walk(out, func(p string, i os.FileInfo, err error) error {
		if err == nil && !i.IsDir() {
			rel, _ := filepath.Rel(out, p)
			names = append(names, rel)
		}
		return nil
	})
	return names
}

func TestHappyPathAndDeterminism(t *testing.T) {
	out := t.TempDir()
	f := newAPI()
	res, cfg, err := run(t, f, out, false)
	if err != nil || !res.Changed {
		t.Fatalf("first run: changed=%v err=%v", res != nil && res.Changed, err)
	}
	if cfg.Requests != 4 {
		t.Fatalf("expected paged fetches, got %d requests", cfg.Requests)
	}
	if _, err := os.Stat(filepath.Join(out, ".nojekyll")); err != nil {
		t.Fatal(err)
	}
	mb, _ := os.ReadFile(filepath.Join(out, "v1/manifest.json"))
	var m Manifest
	if err := json.Unmarshal(mb, &m); err != nil {
		t.Fatal(err)
	}
	if m.SchemaVersion != 1 || m.GeneratedAt != "2026-01-02T03:04:05Z" || len(m.Locales) != 2 || m.Locales[0].Code != "en" || m.Locales[1].Code != "es" {
		t.Fatalf("bad manifest: %s", mb)
	}
	if !strings.HasSuffix(string(mb), "}\n") || !strings.Contains(string(mb), "\n  ") {
		t.Fatalf("manifest not pretty with trailing newline")
	}
	en, _ := os.ReadFile(filepath.Join(out, "v1", m.Locales[0].File))
	if !strings.HasPrefix(m.Locales[0].File, "exercises.en."+m.Locales[0].SHA256[:16]) || !strings.HasSuffix(string(en), "}\n") {
		t.Fatalf("bad en file name/ending: %s", m.Locales[0].File)
	}
	var doc struct {
		Exercises []struct{ ID string } `json:"exercises"`
		Total     int
	}
	if err := json.Unmarshal(en, &doc); err != nil || doc.Total != 3 || doc.Exercises[0].ID != "a" || doc.Exercises[2].ID != "c" {
		t.Fatalf("en content wrong: %v %s", err, en)
	}
	if strings.Contains(string(en), "$schema") || strings.Contains(string(en), "generated_at") || strings.Contains(string(en), "per_page") {
		t.Fatal("wrapper fields leaked")
	}
	for _, ua := range f.uas {
		if ua != UserAgent {
			t.Fatalf("UA %q", ua)
		}
	}

	before := map[string][]byte{}
	for _, n := range listFiles(t, out) {
		before[n], _ = os.ReadFile(filepath.Join(out, n))
	}
	cfg2 := &Config{API: cfg.API, Out: out, Now: func() time.Time { return time.Date(2026, 1, 2, 4, 0, 0, 0, time.UTC) }}
	res2, err := Build(context.Background(), cfg2)
	if err != nil || res2.Changed {
		t.Fatalf("second run should be unchanged: %v", err)
	}
	for n, b := range before {
		got, _ := os.ReadFile(filepath.Join(out, n))
		if string(got) != string(b) {
			t.Fatalf("%s changed", n)
		}
	}
	if len(listFiles(t, out)) != len(before) {
		t.Fatal("extra files written")
	}
}

func TestChangedCatalogKeepsOldDataFiles(t *testing.T) {
	out := t.TempDir()
	f := newAPI()
	run(t, f, out, false)
	n := len(listFiles(t, out))
	f.en = append(f.en, "d")
	f.locales["es"] = append(f.locales["es"], "d")
	res, _, err := run(t, f, out, false)
	if err != nil || !res.Changed {
		t.Fatal(err)
	}
	if got := len(listFiles(t, out)); got != n+2 {
		t.Fatalf("expected old files kept (%d), got %d", n+2, got)
	}
}

func TestRefusals(t *testing.T) {
	cases := map[string]func(f *fakeAPI){
		"http error":       func(f *fakeAPI) { f.status = 500 },
		"count != total":   func(f *fakeAPI) { f.total = map[string]int{"en": 5} },
		"zero exercises":   func(f *fakeAPI) { f.en = nil; f.locales = map[string][]string{} },
		"duplicate ids":    func(f *fakeAPI) { f.en = []string{"a", "a", "b"} },
		"locale id drift":  func(f *fakeAPI) { f.locales["es"] = []string{"a", "b", "zzz"} },
		"locale missing":   func(f *fakeAPI) { f.locales["es"] = []string{"a", "b"} },
		"unpublished":      func(f *fakeAPI) { f.badStatus = "draft" },
		"status missing":   func(f *fakeAPI) { f.noStatus = true },
		"locales no total": func(f *fakeAPI) { f.noLocTotal = true },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			out := t.TempDir()
			f := newAPI()
			mut(f)
			_, _, err := run(t, f, out, false)
			if err == nil {
				t.Fatal("expected refusal")
			}
			if len(listFiles(t, out)) != 0 {
				t.Fatalf("wrote files despite refusal: %v", listFiles(t, out))
			}
		})
	}
}

func TestValidationFailureRetriesOnce(t *testing.T) {
	f := newAPI()
	f.locales["es"] = []string{"a", "b"}
	_, cfg, err := run(t, f, t.TempDir(), false)
	if err == nil {
		t.Fatal("expected failure")
	}
	// one pass = 1 locales + 1 export + 2 es pages(+empty/none) ; two passes doubles it
	if cfg.Requests%2 != 0 || cfg.Requests < 6 {
		t.Fatalf("expected whole collection twice, requests=%d", cfg.Requests)
	}
}

func Test429RetryAfter(t *testing.T) {
	f := newAPI()
	f.fail429 = 2
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	var slept []time.Duration
	cfg := &Config{API: srv.URL, Out: t.TempDir(), Sleep: func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }}
	if _, err := Build(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(slept) != 2 || slept[0] != 60*time.Second {
		t.Fatalf("Retry-After not honoured/capped: %v", slept)
	}

	f2 := newAPI()
	f2.fail429 = 99
	srv2 := httptest.NewServer(f2.handler())
	defer srv2.Close()
	cfg2 := &Config{API: srv2.URL, Out: t.TempDir(), Sleep: func(context.Context, time.Duration) error { return nil }}
	if _, err := Build(context.Background(), cfg2); err == nil || f2.hits != 3 {
		t.Fatalf("expected failure after 3 tries, hits=%d err=%v", f2.hits, err)
	}
}

func TestLocaleWithNoTranslationsStillEmitted(t *testing.T) {
	// A listed locale with no translations: API returns the exercises untranslated? No:
	// it returns an empty list. That is a refusal (id set != en), not a silent skip.
	f := newAPI()
	f.locales["fr"] = []string{}
	if _, _, err := run(t, f, t.TempDir(), false); err == nil {
		t.Fatal("empty non-en locale must be refused as an id mismatch")
	}
}

func TestLocaleWithUntranslatedFallbackEmitted(t *testing.T) {
	f := newAPI()
	f.locales["fr"] = []string{"a", "b", "c"}
	out := t.TempDir()
	res, _, err := run(t, f, out, false)
	if err != nil || len(res.Locales) != 3 || res.Locales[2].Code != "fr" {
		t.Fatalf("fr not emitted: %v", err)
	}
}

func TestShrinkGuard(t *testing.T) {
	out := t.TempDir()
	f := &fakeAPI{en: []string{"a", "b", "c", "d"}, locales: map[string][]string{}}
	if _, _, err := run(t, f, out, false); err != nil {
		t.Fatal(err)
	}
	f.en = []string{"a"}
	_, _, err := run(t, f, out, false)
	if err == nil || !strings.Contains(err.Error(), "confirm-removals") {
		t.Fatalf("expected shrink refusal, got %v", err)
	}
	f.en = []string{"a", "b"} // exactly half is allowed
	if _, _, err := run(t, f, out, false); err != nil {
		t.Fatalf("half should pass: %v", err)
	}
	f.en = []string{"a"}
	res, _, err := run(t, f, out, true)
	if err != nil || !res.Changed {
		t.Fatalf("confirm flag should allow: %v", err)
	}
}

func TestUserAgentMatchesAPIPolicy(t *testing.T) {
	if !api.IsIdentifiedUA(UserAgent) {
		t.Fatalf("UA %q is not accepted as identified by the API", UserAgent)
	}
	if UserAgent != "open-exercise-db-snapshot/1 (+https://github.com/Cawlumm/open-exercise-db)" {
		t.Fatal("UA changed; update the docs")
	}
}

func TestPublishedAtHeartbeat(t *testing.T) {
	out := t.TempDir()
	f := newAPI()
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	build := func(now time.Time) *Result {
		t.Helper()
		res, err := Build(context.Background(), &Config{API: srv.URL, Out: out, Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	read := func() Manifest {
		var m Manifest
		b, _ := os.ReadFile(filepath.Join(out, "v1/manifest.json"))
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if r := build(t0); !r.Changed {
		t.Fatal("first run must write")
	}
	m := read()
	if m.GeneratedAt != m.PublishedAt || m.PublishedAt != "2026-01-02T03:04:05Z" {
		t.Fatalf("first run must set both: %+v", m)
	}
	before, _ := os.ReadFile(filepath.Join(out, "v1/manifest.json"))
	if r := build(t0.Add(23 * time.Hour)); r.Changed {
		t.Fatal("under 24h must be unchanged")
	}
	after, _ := os.ReadFile(filepath.Join(out, "v1/manifest.json"))
	if string(before) != string(after) {
		t.Fatal("manifest rewritten under 24h")
	}
	t1 := t0.Add(25 * time.Hour)
	if r := build(t1); !r.Changed {
		t.Fatal("over 24h must bump")
	}
	m = read()
	if m.GeneratedAt != "2026-01-02T03:04:05Z" || m.PublishedAt != t1.Format(time.RFC3339) {
		t.Fatalf("heartbeat must bump published_at only: %+v", m)
	}
	f.en = append(f.en, "d")
	f.locales["es"] = append(f.locales["es"], "d")
	t2 := t1.Add(time.Hour)
	build(t2)
	m = read()
	if m.GeneratedAt != t2.Format(time.RFC3339) || m.PublishedAt != m.GeneratedAt {
		t.Fatalf("content change must bump both: %+v", m)
	}
}

func manifestPath(out string) string { return filepath.Join(out, "v1", "manifest.json") }

func TestUnreadableManifestRefuses(t *testing.T) {
	out := t.TempDir()
	os.MkdirAll(filepath.Join(out, "v1"), 0o755)
	os.WriteFile(manifestPath(out), []byte("{not json"), 0o644)
	if _, _, err := run(t, newAPI(), out, true); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("expected refusal even with confirm, got %v", err)
	}
	os.Remove(manifestPath(out))
	os.Mkdir(manifestPath(out), 0o755) // exists but cannot be read as a file
	if _, _, err := run(t, newAPI(), out, false); err == nil {
		t.Fatal("expected refusal for unreadable manifest path")
	}
}

func TestDroppedLocaleRefused(t *testing.T) {
	out := t.TempDir()
	f := newAPI()
	if _, _, err := run(t, f, out, false); err != nil {
		t.Fatal(err)
	}
	f.locales = map[string][]string{}
	_, _, err := run(t, f, out, false)
	if err == nil || !strings.Contains(err.Error(), "locale es") {
		t.Fatalf("expected dropped-locale refusal, got %v", err)
	}
	res, _, err := run(t, f, out, true)
	if err != nil || !res.Changed || len(res.Locales) != 1 {
		t.Fatalf("confirm should allow: %v", err)
	}
}

func TestTransient5xxRetriedOnce(t *testing.T) {
	f := newAPI()
	f.fail5xx = 1
	if _, _, err := run(t, f, t.TempDir(), false); err != nil {
		t.Fatalf("one 5xx should be retried: %v", err)
	}
	f2 := newAPI()
	f2.fail5xx = 2
	srv := httptest.NewServer(f2.handler())
	defer srv.Close()
	cfg := &Config{API: srv.URL, Out: t.TempDir(), Sleep: func(context.Context, time.Duration) error { return nil }}
	if _, err := Build(context.Background(), cfg); err == nil || f2.hits != 2 {
		t.Fatalf("two 5xx in a row should fail after one retry, hits=%d err=%v", f2.hits, err)
	}
}

func TestNetworkErrorRetriedOnce(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			hj, _ := w.(http.Hijacker)
			c, _, _ := hj.Hijack()
			c.Close()
			return
		}
		fmt.Fprint(w, `{"locales":[],"total":0}`)
	}))
	defer srv.Close()
	var dst listBody
	cfg := &Config{API: srv.URL, Client: srv.Client(), Now: time.Now, Log: os.Stderr, Sleep: func(context.Context, time.Duration) error { return nil }}
	if err := getJSON(context.Background(), cfg, "/x", &dst); err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestRetryAfterHTTPDate(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if d := retryAfter(now.Add(30*time.Second).Format(http.TimeFormat), now); d != 30*time.Second {
		t.Fatalf("got %v", d)
	}
	if d := retryAfter(now.Add(time.Hour).Format(http.TimeFormat), now); d != 60*time.Second {
		t.Fatalf("cap: %v", d)
	}
	if d := retryAfter("junk", now); d != time.Second {
		t.Fatalf("junk: %v", d)
	}
}

func TestSleepIsContextAware(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); err == nil {
		t.Fatal("cancelled sleep must return")
	}
}
