package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2/humatest"

	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// countingBuild returns a build func that records how often it ran and
// produces an entry of the given size.
func countingBuild(calls *atomic.Int32, size int) func(context.Context) (*listEntry, error) {
	return func(context.Context) (*listEntry, error) {
		n := calls.Add(1)
		return &listEntry{tag: fmt.Sprintf("tag-%d", n), size: size}, nil
	}
}

func TestListCache_HitDoesNotBuild(t *testing.T) {
	c := newListCache(time.Hour, 1<<20, 100)
	var calls atomic.Int32
	ctx := context.Background()

	first, err := c.get(ctx, "k", countingBuild(&calls, 10))
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.get(ctx, "k", countingBuild(&calls, 10))
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("build ran %d times, want 1", calls.Load())
	}
	if first.tag != second.tag {
		t.Errorf("hit returned tag %q, want the stored %q", second.tag, first.tag)
	}
}

func TestListCache_ExpiredEntryRebuilds(t *testing.T) {
	c := newListCache(time.Millisecond, 1<<20, 100)
	var calls atomic.Int32
	ctx := context.Background()

	if _, err := c.get(ctx, "k", countingBuild(&calls, 10)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := c.get(ctx, "k", countingBuild(&calls, 10)); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("build ran %d times, want 2 (the first entry had expired)", calls.Load())
	}
}

func TestListCache_InvalidateClears(t *testing.T) {
	c := newListCache(0, 1<<20, 100)
	var calls atomic.Int32
	ctx := context.Background()

	for _, k := range []string{"a", "b"} {
		if _, err := c.get(ctx, k, countingBuild(&calls, 10)); err != nil {
			t.Fatal(err)
		}
	}
	c.invalidate()
	if c.lru.Len() != 0 || c.bytes != 0 || len(c.entries) != 0 {
		t.Fatalf("after invalidate: %d entries, %d bytes; want empty", c.lru.Len(), c.bytes)
	}
	if _, err := c.get(ctx, "a", countingBuild(&calls, 10)); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("build ran %d times, want 3 (a, b, then a again after invalidate)", calls.Load())
	}
}

// A build that was already running when a moderator published read the old
// data. Storing it would undo the invalidation.
func TestListCache_BuildInFlightAcrossInvalidateIsNotStored(t *testing.T) {
	c := newListCache(0, 1<<20, 100)
	ctx := context.Background()
	started, release := make(chan struct{}), make(chan struct{})

	go func() {
		_, _ = c.get(ctx, "k", func(context.Context) (*listEntry, error) {
			close(started)
			<-release
			return &listEntry{tag: "stale", size: 10}, nil
		})
	}()
	<-started
	c.invalidate()
	close(release)

	var calls atomic.Int32
	e, err := c.get(ctx, "k", countingBuild(&calls, 10))
	if err != nil {
		t.Fatal(err)
	}
	if e.tag == "stale" || calls.Load() != 1 {
		t.Fatalf("served %q after invalidate with %d builds; want a fresh build", e.tag, calls.Load())
	}
}

func TestListCache_ErrorIsNotCached(t *testing.T) {
	c := newListCache(0, 1<<20, 100)
	ctx := context.Background()
	boom := errors.New("boom")
	var calls atomic.Int32

	_, err := c.get(ctx, "k", func(context.Context) (*listEntry, error) {
		calls.Add(1)
		return nil, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if c.lru.Len() != 0 {
		t.Fatal("an error was stored")
	}
	if _, err := c.get(ctx, "k", countingBuild(&calls, 10)); err != nil {
		t.Fatalf("retry after error: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("build ran %d times, want 2 (the error must not be served again)", calls.Load())
	}
}

func TestListCache_SizeBoundsEvictLeastRecentlyUsed(t *testing.T) {
	ctx := context.Background()

	t.Run("bytes", func(t *testing.T) {
		c := newListCache(0, 100, 100)
		var calls atomic.Int32
		for _, k := range []string{"a", "b"} {
			if _, err := c.get(ctx, k, countingBuild(&calls, 40)); err != nil {
				t.Fatal(err)
			}
		}
		// Touch a so b is the least recently used, then overflow.
		if _, err := c.get(ctx, "a", countingBuild(&calls, 40)); err != nil {
			t.Fatal(err)
		}
		if _, err := c.get(ctx, "c", countingBuild(&calls, 40)); err != nil {
			t.Fatal(err)
		}
		if _, ok := c.entries["b"]; ok {
			t.Error("b survived; it was least recently used and should have been evicted")
		}
		if _, ok := c.entries["a"]; !ok {
			t.Error("a was evicted despite being used more recently than b")
		}
		if c.bytes > 100 {
			t.Errorf("bytes = %d, over the 100-byte bound", c.bytes)
		}
	})

	t.Run("entries", func(t *testing.T) {
		c := newListCache(0, 1<<20, 2)
		var calls atomic.Int32
		for _, k := range []string{"a", "b", "c"} {
			if _, err := c.get(ctx, k, countingBuild(&calls, 1)); err != nil {
				t.Fatal(err)
			}
		}
		if c.lru.Len() != 2 {
			t.Fatalf("len = %d, want 2", c.lru.Len())
		}
		if _, ok := c.entries["a"]; ok {
			t.Error("a survived; it was the oldest")
		}
	})

	t.Run("an entry bigger than the whole budget is served but not stored", func(t *testing.T) {
		c := newListCache(0, 100, 100)
		var calls atomic.Int32
		if _, err := c.get(ctx, "small", countingBuild(&calls, 10)); err != nil {
			t.Fatal(err)
		}
		e, err := c.get(ctx, "huge", countingBuild(&calls, 101))
		if err != nil || e == nil {
			t.Fatalf("huge: %v", err)
		}
		if _, ok := c.entries["huge"]; ok {
			t.Error("an over-budget entry was stored")
		}
		if _, ok := c.entries["small"]; !ok {
			t.Error("an over-budget entry evicted everything else on its way to not fitting")
		}
	})
}

func TestListCache_ConcurrentMissesShareOneBuild(t *testing.T) {
	c := newListCache(0, 1<<20, 100)
	var calls atomic.Int32
	release := make(chan struct{})
	build := func(context.Context) (*listEntry, error) {
		calls.Add(1)
		<-release
		return &listEntry{tag: "shared", size: 10}, nil
	}

	const n = 20
	var wg sync.WaitGroup
	tags := make(chan string, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, err := c.get(context.Background(), "k", build)
			if err != nil {
				t.Error(err)
				return
			}
			tags <- e.tag
		}()
	}
	// Let every goroutine reach the cache before the build may finish.
	for {
		c.mu.Lock()
		_, inflight := c.inflight["k"]
		c.mu.Unlock()
		if inflight {
			break
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	close(tags)

	if calls.Load() != 1 {
		t.Fatalf("build ran %d times for %d concurrent identical misses, want 1", calls.Load(), n)
	}
	for tag := range tags {
		if tag != "shared" {
			t.Errorf("a waiter got %q, want the shared result", tag)
		}
	}
}

func TestListCacheKey(t *testing.T) {
	base := store.ExerciseFilter{Page: 1, PerPage: 100, IDs: []string{"b", "a"}, Locale: "es"}
	key := listCacheKey(base)

	reordered := base
	reordered.IDs = []string{"a", "b"}
	if listCacheKey(reordered) != key {
		t.Error("the same ids in a different order produced a different key")
	}

	tests := []struct {
		name   string
		mutate func(f *store.ExerciseFilter)
	}{
		{"locale", func(f *store.ExerciseFilter) { f.Locale = "de" }},
		{"no locale", func(f *store.ExerciseFilter) { f.Locale = "" }},
		{"ids", func(f *store.ExerciseFilter) { f.IDs = []string{"a", "c"} }},
		{"ids moved to source_ids", func(f *store.ExerciseFilter) { f.SourceIDs, f.IDs = f.IDs, nil }},
		{"page", func(f *store.ExerciseFilter) { f.Page = 2 }},
		{"per_page", func(f *store.ExerciseFilter) { f.PerPage = 50 }},
		{"q", func(f *store.ExerciseFilter) { f.Q = "bench" }},
		{"slug", func(f *store.ExerciseFilter) { f.Slug = []string{"x"} }},
		{"muscle_group", func(f *store.ExerciseFilter) { f.MuscleGroup = []string{"x"} }},
		{"equipment", func(f *store.ExerciseFilter) { f.Equipment = []string{"x"} }},
		{"category", func(f *store.ExerciseFilter) { f.Category = []string{"x"} }},
		{"mechanic", func(f *store.ExerciseFilter) { f.Mechanic = []string{"x"} }},
		{"level", func(f *store.ExerciseFilter) { f.Level = []string{"x"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := base
			tt.mutate(&f)
			if listCacheKey(f) == key {
				t.Errorf("changing %s did not change the key", tt.name)
			}
		})
	}
}

// The database-backed half: the registered operation must serve repeat
// requests, including revalidations, without Postgres, and must not let one
// locale or id set answer for another.
func TestListExercises_CachedOverHTTP(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	idA, revA := seedExercise(t, ctx, pool, exerciseFixture{
		slug: "cache-bench", name: "Cache Bench",
		muscleGroup: "chest", equipment: "barbell", category: "strength",
	})
	idB, _ := seedExercise(t, ctx, pool, exerciseFixture{
		slug: "cache-row", name: "Cache Row",
		muscleGroup: "lats", equipment: "barbell", category: "strength",
	})
	seedTranslation(t, ctx, pool, revA, "es", "Press de banca", []string{}, []string{"Uno."}, "human")

	cache := newListCache(0, listCacheMaxBytes, listCacheMaxEntries)
	_, testAPI := humatest.New(t)
	registerExerciseRoutes(testAPI, s, "", cache)

	get := func(path string, headers ...any) (int, string, string) {
		resp := testAPI.Get(path, headers...)
		return resp.Code, resp.Header().Get("ETag"), resp.Body.String()
	}
	names := func(body string) []string {
		var b struct {
			Exercises []struct {
				Translation struct {
					Name string `json:"name"`
				} `json:"translation"`
			} `json:"exercises"`
		}
		if err := json.Unmarshal([]byte(body), &b); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		out := make([]string, 0, len(b.Exercises))
		for _, e := range b.Exercises {
			out = append(out, e.Translation.Name)
		}
		return out
	}

	esPath := "/api/v1/exercises?locale=es&ids=" + idB + "," + idA
	code, esTag, esBody := get(esPath)
	if code != http.StatusOK {
		t.Fatalf("es = %d: %s", code, esBody)
	}
	if got := names(esBody); len(got) != 2 || !slices.Contains(got, "Press de banca") {
		t.Fatalf("es names = %v, want the Spanish name", got)
	}

	code, enTag, enBody := get("/api/v1/exercises?ids=" + idA + "," + idB)
	if code != http.StatusOK {
		t.Fatalf("en = %d: %s", code, enBody)
	}
	if slices.Contains(names(enBody), "Press de banca") || enTag == esTag {
		t.Fatal("the English request was answered from the Spanish entry")
	}

	code, _, oneBody := get("/api/v1/exercises?locale=es&ids=" + idA)
	if code != http.StatusOK || len(names(oneBody)) != 1 {
		t.Fatalf("a different id set shared an entry: %d %s", code, oneBody)
	}

	// From here on there is no database. Anything that still answers is
	// answering from the cache.
	pool.Close()

	t.Run("a hit needs no database, whatever the parameter order", func(t *testing.T) {
		code, tag, body := get("/api/v1/exercises?ids=" + idA + "&ids=" + idB + "&locale=es")
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 from the cache: %s", code, body)
		}
		if tag != esTag || body != esBody {
			t.Error("the cached response differs from the one first built")
		}
	})

	t.Run("a revalidation is a 304 with no database", func(t *testing.T) {
		resp := testAPI.Get(esPath, "If-None-Match: "+esTag)
		if resp.Code != http.StatusNotModified {
			t.Fatalf("status = %d, want 304: %s", resp.Code, resp.Body.String())
		}
		if resp.Header().Get("ETag") != esTag || resp.Header().Get("Cache-Control") != cacheControl {
			t.Errorf("304 headers = %v", resp.Header())
		}
		if resp.Body.Len() != 0 {
			t.Errorf("304 carried a body: %q", resp.Body.String())
		}
	})

	t.Run("a stale If-None-Match still gets the full cached body", func(t *testing.T) {
		code, _, body := get(esPath, `If-None-Match: "old"`)
		if code != http.StatusOK || body != esBody {
			t.Fatalf("status = %d", code)
		}
	})

	t.Run("an uncached request still reaches the database", func(t *testing.T) {
		code, _, body := get("/api/v1/exercises?locale=de&ids=" + idA)
		if code == http.StatusOK {
			t.Fatalf("an uncached request answered 200 with the pool closed: %s", body)
		}
	})

	t.Run("invalidate drops the entries", func(t *testing.T) {
		cache.invalidate()
		if code, _, _ := get(esPath); code == http.StatusOK {
			t.Fatal("answered 200 after invalidate with the pool closed — the entry survived")
		}
	})
}

// An error response is never stored: the same request made once the cause is
// fixed must be built, not replayed.
func TestListExercises_ErrorsAreNotCached(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	cache := newListCache(0, listCacheMaxBytes, listCacheMaxEntries)
	_, testAPI := humatest.New(t)
	registerExerciseRoutes(testAPI, s, "", cache)

	// The facet needs some vocabulary, or validation fails open. An unknown
	// slug is then a 422 — until the term exists.
	seedTaxonomyTerm(t, ctx, pool, "equipment", "cache-probe-known", nil)
	path := "/api/v1/exercises?equipment=cache-probe-gear"
	if resp := testAPI.Get(path); resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", resp.Code, resp.Body.String())
	}
	if cache.lru.Len() != 0 {
		t.Fatal("a 422 was stored in the cache")
	}

	seedTaxonomyTerm(t, ctx, pool, "equipment", "cache-probe-gear", nil)
	if resp := testAPI.Get(path); resp.Code != http.StatusOK {
		t.Fatalf("status after the term exists = %d, want 200: %s", resp.Code, resp.Body.String())
	}
}
