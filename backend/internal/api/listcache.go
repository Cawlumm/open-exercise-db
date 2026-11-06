package api

import (
	"container/list"
	"context"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// listCache holds fully built GET /exercises responses, keyed by the
// normalized filter that produced them.
//
// It exists for database egress, not latency. One page of 100 ids costs ~400
// queries and ~350 KB out of Postgres, because every row resolves its
// translation and images separately (resolveExercise), and Supabase meters
// that traffic uncompressed. Lyftr installs re-sync their exercises by id
// every day, in identical sorted-id pages per locale, so nearly every one of
// those requests asks for something this process has already built. A 304
// never helped: the ETag is a hash of the built body, so answering one still
// paid for the whole build.
//
// Bounded by bytes and by entries, least recently used evicted first, so a
// caller walking every search term cannot grow it without limit. Errors are
// never stored. Identical concurrent misses share one build.
type listCache struct {
	ttl        time.Duration
	maxBytes   int
	maxEntries int

	mu       sync.Mutex
	lru      *list.List // of *listEntry, most recently used at the front
	entries  map[string]*list.Element
	bytes    int
	inflight map[string]*listFlight
	// gen is bumped by invalidate. A build that started before an
	// invalidation finishes after it holding pre-publish data; comparing
	// generations is what stops it being stored.
	gen uint64
}

// listCacheMaxBytes bounds the cache by the JSON size of what it holds. The
// Go values behind that JSON take roughly as much heap again, and Cloud Run
// gives this process 512 MiB (docs/DEPLOYMENT.md), so 32 MB of responses is
// at most ~64 MB resident: about ninety full 100-row pages, several days of
// Lyftr's sync traffic across every locale it ships.
const listCacheMaxBytes = 32 << 20

// listCacheMaxEntries bounds the count separately, because a type-ahead
// search produces many tiny responses that the byte bound alone would let
// accumulate into a large map of small values.
const listCacheMaxEntries = 2048

// errListBuildAborted is what waiters receive if the build they were sharing
// never returned, e.g. because it panicked.
var errListBuildAborted = errors.New("list response build did not complete")

type listEntry struct {
	key     string
	out     ListExercisesOutput // complete 200 response; copied, never mutated
	tag     string              // bare ETag, as checkNotModified expects
	size    int
	builtAt time.Time
}

type listFlight struct {
	done  chan struct{}
	entry *listEntry
	err   error
}

func newListCache(ttl time.Duration, maxBytes, maxEntries int) *listCache {
	return &listCache{
		ttl:        ttl,
		maxBytes:   maxBytes,
		maxEntries: maxEntries,
		lru:        list.New(),
		entries:    make(map[string]*list.Element),
		inflight:   make(map[string]*listFlight),
	}
}

// get returns the entry for key, building it with build on a miss.
//
// build runs on a context detached from the caller's cancellation: other
// requests may be waiting on the same build, and one client hanging up must
// not turn their responses into errors.
func (c *listCache) get(ctx context.Context, key string, build func(context.Context) (*listEntry, error)) (*listEntry, error) {
	c.mu.Lock()
	if el, ok := c.entries[key]; ok {
		e := el.Value.(*listEntry)
		if c.ttl <= 0 || time.Since(e.builtAt) < c.ttl {
			c.lru.MoveToFront(el)
			c.mu.Unlock()
			return e, nil
		}
		c.remove(el)
	}
	if f, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		select {
		case <-f.done:
			return f.entry, f.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f := &listFlight{done: make(chan struct{}), err: errListBuildAborted}
	c.inflight[key] = f
	gen := c.gen
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		if c.inflight[key] == f {
			delete(c.inflight, key)
		}
		if f.err == nil && c.gen == gen {
			c.add(f.entry)
		}
		c.mu.Unlock()
		close(f.done)
	}()

	f.entry, f.err = build(context.WithoutCancel(ctx))
	if f.err == nil {
		f.entry.key = key
		f.entry.builtAt = time.Now()
	}
	return f.entry, f.err
}

// invalidate drops every stored response and detaches in-flight builds, so
// the next request for any key reads the database again.
func (c *listCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	c.lru.Init()
	c.entries = make(map[string]*list.Element)
	c.inflight = make(map[string]*listFlight)
	c.bytes = 0
}

// add stores e, then evicts from the least recently used end until both
// bounds hold. An entry larger than the whole budget is not stored at all.
// Caller holds c.mu.
func (c *listCache) add(e *listEntry) {
	if e.size > c.maxBytes {
		return
	}
	if el, ok := c.entries[e.key]; ok {
		c.remove(el)
	}
	c.entries[e.key] = c.lru.PushFront(e)
	c.bytes += e.size
	for c.bytes > c.maxBytes || c.lru.Len() > c.maxEntries {
		c.remove(c.lru.Back())
	}
}

// remove deletes el. Caller holds c.mu.
func (c *listCache) remove(el *list.Element) {
	e := c.lru.Remove(el).(*listEntry)
	delete(c.entries, e.key)
	c.bytes -= e.size
}

// listCacheKey canonicalizes everything that decides a list response's body.
//
// It is built from the normalized filter rather than the raw query string,
// so parameter order, comma-separated versus repeated values, value order
// within a filter, and out-of-range paging all collapse onto one entry. Value
// order is safe to discard because each filter is matched as a set and the
// rows come back in the store's own ORDER BY. q is kept verbatim: it is
// tokenized and ranked, and normalizing it here would be a second copy of the
// store's rules. No request header changes the body (Accept-Language is not
// read; locale is a query parameter), and If-None-Match only decides between
// 200 and 304 after the lookup, so neither is part of the key.
func listCacheKey(f store.ExerciseFilter) string {
	v := url.Values{}
	set := func(name string, values []string) {
		if len(values) > 0 {
			v[name] = slices.Sorted(slices.Values(values))
		}
	}
	v.Set("q", f.Q)
	v.Set("locale", f.Locale)
	v.Set("page", strconv.Itoa(f.Page))
	v.Set("per_page", strconv.Itoa(f.PerPage))
	set("slug", f.Slug)
	set("muscle_group", f.MuscleGroup)
	set("equipment", f.Equipment)
	set("category", f.Category)
	set("mechanic", f.Mechanic)
	set("level", f.Level)
	set("ids", f.IDs)
	set("source_ids", f.SourceIDs)
	return v.Encode()
}
