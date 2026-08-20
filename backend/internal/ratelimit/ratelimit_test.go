package ratelimit

import (
	"testing"
	"time"
)

// fixedClock lets tests advance time deterministically instead of sleeping
// through real refill windows — Limiter.now is swapped out for one of
// these in every test below.
type fixedClock struct {
	t time.Time
}

func (c *fixedClock) now() time.Time          { return c.t }
func (c *fixedClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestLimiter(cfg Config) (*Limiter, *fixedClock) {
	l := &Limiter{
		cfg:     cfg.withDefaults(),
		buckets: make(map[string]*bucket),
		stop:    make(chan struct{}),
	}
	clock := &fixedClock{t: time.Unix(0, 0)}
	l.now = clock.now
	// Deliberately not starting cleanupLoop: these tests drive time via the
	// fixed clock, not a real ticker, and don't exercise eviction here (see
	// TestLimiter_EvictsIdleBuckets for that, which does start it).
	return l, clock
}

func TestLimiter_AllowsWithinBurstThenDenies(t *testing.T) {
	l, clock := newTestLimiter(Config{Read: Rule{RPM: 60, Burst: 3}})

	for i := 0; i < 3; i++ {
		d := l.Allow("1.2.3.4", TierRead, true)
		if !d.Allowed {
			t.Fatalf("request %d: Allowed = false, want true (within burst of 3)", i+1)
		}
	}

	d := l.Allow("1.2.3.4", TierRead, true)
	if d.Allowed {
		t.Fatal("4th request within the same instant: Allowed = true, want false (burst exhausted)")
	}
	if d.RetryAfterSeconds < 1 {
		t.Errorf("RetryAfterSeconds = %d, want >= 1", d.RetryAfterSeconds)
	}
	if d.Remaining != 0 {
		t.Errorf("Remaining on a denied decision = %d, want 0", d.Remaining)
	}
	_ = clock
}

func TestLimiter_RefillsOverTime(t *testing.T) {
	// 60 RPM = 1 token/second, burst 1 — easy to reason about.
	l, clock := newTestLimiter(Config{Read: Rule{RPM: 60, Burst: 1}})

	if d := l.Allow("1.2.3.4", TierRead, true); !d.Allowed {
		t.Fatal("first request denied, want allowed")
	}
	if d := l.Allow("1.2.3.4", TierRead, true); d.Allowed {
		t.Fatal("second immediate request allowed, want denied (burst 1, no refill yet)")
	}

	clock.advance(1100 * time.Millisecond)

	if d := l.Allow("1.2.3.4", TierRead, true); !d.Allowed {
		t.Fatal("request after 1.1s (>= 1 token refilled at 1/sec): Allowed = false, want true")
	}
}

func TestLimiter_TiersAreIndependentPerKey(t *testing.T) {
	// docs/PHASE1_BUILD_PLAN.md WO-7 acceptance criteria: hammering /export
	// must not exhaust the /meta (read) budget, and vice versa.
	l, _ := newTestLimiter(Config{
		Read:   Rule{RPM: 60, Burst: 1},
		Export: Rule{RPM: 60, Burst: 1},
	})

	if d := l.Allow("1.2.3.4", TierExport, true); !d.Allowed {
		t.Fatal("first export request denied, want allowed")
	}
	if d := l.Allow("1.2.3.4", TierExport, true); d.Allowed {
		t.Fatal("second immediate export request allowed, want denied")
	}

	// Same key, different tier — must have its own untouched budget.
	if d := l.Allow("1.2.3.4", TierRead, true); !d.Allowed {
		t.Fatal("read request for the same key as an exhausted export bucket: Allowed = false, want true — tiers must not share budget")
	}
}

func TestLimiter_KeysAreIndependentPerTier(t *testing.T) {
	l, _ := newTestLimiter(Config{Read: Rule{RPM: 60, Burst: 1}})

	if d := l.Allow("1.1.1.1", TierRead, true); !d.Allowed {
		t.Fatal("first client denied, want allowed")
	}
	if d := l.Allow("1.1.1.1", TierRead, true); d.Allowed {
		t.Fatal("first client's second immediate request allowed, want denied")
	}

	// A different key must not be affected by the first key's exhausted
	// bucket — this is what keeps one abusive IP from collapsing the
	// budget for every other consumer.
	if d := l.Allow("2.2.2.2", TierRead, true); !d.Allowed {
		t.Fatal("second, unrelated client denied, want allowed")
	}
}

func TestLimiter_UnidentifiedUsesStrictBucketRegardlessOfTier(t *testing.T) {
	l, _ := newTestLimiter(Config{
		Read:         Rule{RPM: 60, Burst: 100}, // generous
		Unidentified: Rule{RPM: 60, Burst: 1},   // strict
	})

	d := l.Allow("1.2.3.4", TierRead, false) // identified=false
	if !d.Allowed {
		t.Fatal("first unidentified request denied, want allowed (burst 1)")
	}
	if d.EffectiveTier != Unidentified {
		t.Errorf("EffectiveTier = %q, want %q", d.EffectiveTier, Unidentified)
	}

	d = l.Allow("1.2.3.4", TierRead, false)
	if d.Allowed {
		t.Fatal("second immediate unidentified request allowed, want denied — the generous TierRead burst must not apply to an unidentified caller")
	}

	// The same key, now identified, gets TierRead's own separate (and much
	// larger) budget — proves Unidentified and TierRead are genuinely
	// separate buckets for the same key, not the same bucket relabeled.
	d = l.Allow("1.2.3.4", TierRead, true)
	if !d.Allowed {
		t.Fatal("identified request for the same key as an exhausted unidentified bucket: Allowed = false, want true")
	}
}

func TestLimiter_UnknownTierFallsBackToExport(t *testing.T) {
	l, _ := newTestLimiter(Config{Export: Rule{RPM: 60, Burst: 1}})

	d := l.Allow("1.2.3.4", Tier("not-a-real-tier"), true)
	if !d.Allowed {
		t.Fatal("first request on an unrecognized tier denied, want allowed (falls back to Export's burst of 1)")
	}
	d = l.Allow("1.2.3.4", Tier("not-a-real-tier"), true)
	if d.Allowed {
		t.Fatal("second immediate request on an unrecognized tier allowed, want denied — confirms it used Export's Burst of 1, not some unlimited default")
	}
}

func TestLimiter_EvictsIdleBuckets(t *testing.T) {
	l := NewLimiter(Config{
		Read:    Rule{RPM: 60, Burst: 1},
		IdleTTL: 50 * time.Millisecond,
	})
	defer l.Close()

	clock := &fixedClock{t: time.Now()}
	l.now = clock.now

	l.Allow("1.2.3.4", TierRead, true)

	l.mu.Lock()
	before := len(l.buckets)
	l.mu.Unlock()
	if before != 1 {
		t.Fatalf("bucket count after one request = %d, want 1", before)
	}

	clock.advance(1 * time.Hour) // well past IdleTTL
	l.evictIdle()

	l.mu.Lock()
	after := len(l.buckets)
	l.mu.Unlock()
	if after != 0 {
		t.Fatalf("bucket count after eviction past IdleTTL = %d, want 0", after)
	}
}

func TestConfig_WithDefaultsFillsInvalidRules(t *testing.T) {
	cfg := Config{
		Read: Rule{RPM: 5, Burst: 5}, // valid, must survive untouched
		// Search, Export, Unidentified left zero-valued (invalid).
	}
	resolved := cfg.withDefaults()

	if resolved.Read != (Rule{RPM: 5, Burst: 5}) {
		t.Errorf("Read = %+v, want the explicitly configured rule preserved", resolved.Read)
	}
	def := DefaultConfig()
	if resolved.Search != def.Search {
		t.Errorf("Search = %+v, want default %+v (zero-valued input must fall back)", resolved.Search, def.Search)
	}
	if resolved.Export != def.Export {
		t.Errorf("Export = %+v, want default %+v", resolved.Export, def.Export)
	}
	if resolved.Unidentified != def.Unidentified {
		t.Errorf("Unidentified = %+v, want default %+v", resolved.Unidentified, def.Unidentified)
	}
	if resolved.IdleTTL != def.IdleTTL {
		t.Errorf("IdleTTL = %v, want default %v", resolved.IdleTTL, def.IdleTTL)
	}
}

// An <img> tag cannot send headers, so every image a browser loads is
// unidentified by construction. Downgrading those to the strict tier meant a
// grid rendered two thumbnails and then received 429s carrying a JSON body,
// which Chrome's opaque-response blocking rejects — a blank image with no
// explanation available to the page.
func TestAllow_ImagesAreExemptFromTheUnidentifiedDowngrade(t *testing.T) {
	l := NewLimiter(Config{
		Image:        Rule{RPM: 600, Burst: 600},
		Read:         Rule{RPM: 120, Burst: 120},
		Unidentified: Rule{RPM: 5, Burst: 5},
	})
	defer l.Close()

	// Fifty thumbnails, the size of one picker screen, all unidentified.
	for i := range 50 {
		d := l.Allow("1.2.3.4", TierImage, false)
		if !d.Allowed {
			t.Fatalf("image %d of 50 was rejected; a grid cannot render", i+1)
		}
		if d.EffectiveTier != TierImage {
			t.Fatalf("image %d billed to %q, want the image tier", i+1, d.EffectiveTier)
		}
	}

	// Every other tier still downgrades — the exemption is for images only,
	// because they are the only requests that cannot comply.
	if d := l.Allow("5.6.7.8", TierRead, false); d.EffectiveTier != Unidentified {
		t.Errorf("unidentified read billed to %q, want the unidentified tier", d.EffectiveTier)
	}
	if d := l.Allow("5.6.7.8", TierSearch, false); d.EffectiveTier != Unidentified {
		t.Errorf("unidentified search billed to %q, want the unidentified tier", d.EffectiveTier)
	}
}

// Exempt from identification is not exempt from limiting: the image tier is
// generous, not unbounded.
func TestAllow_ImagesAreStillLimited(t *testing.T) {
	l := NewLimiter(Config{Image: Rule{RPM: 3, Burst: 3}})
	defer l.Close()

	for i := range 3 {
		if d := l.Allow("1.2.3.4", TierImage, false); !d.Allowed {
			t.Fatalf("image %d was rejected inside the burst", i+1)
		}
	}
	if d := l.Allow("1.2.3.4", TierImage, false); d.Allowed {
		t.Error("image tier allowed a request past its burst; it must still be bounded")
	}
}
