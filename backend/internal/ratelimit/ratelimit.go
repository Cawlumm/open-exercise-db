package ratelimit

import (
	"math"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Tier names one of the cost-based limiter buckets from
// docs/PHASE1_BUILD_PLAN.md WO-7: cheap cached single-item/meta reads,
// paginated search (sized for a debounced type-ahead client, not a
// per-keystroke one), and the full-catalog export (heaviest single
// request in the API, meant to be mirrored on a schedule rather than
// polled).
type Tier string

const (
	// TierRead covers GET /exercises/{id} and every GET /meta/* route — a
	// single cached row or a short taxonomy list, the cheapest reads in the
	// API.
	TierRead Tier = "read"
	// TierSearch covers GET /exercises — filtered/paginated search, the
	// endpoint a debounced type-ahead client hits repeatedly.
	TierSearch Tier = "search"
	// TierExport covers GET /exercises/export — the full-catalog dump.
	TierExport Tier = "export"

	// Unidentified is not a cost tier — it's an orthogonal, always-strict
	// bucket applied *instead of* (never in addition to) a request's real
	// cost tier when the caller didn't identify itself with a conformant
	// User-Agent. See internal/api/ratelimit.go's isIdentifiedUA for the
	// policy this enforces and docs/API_POLICY.md for the public
	// documentation of it.
	Unidentified Tier = "unidentified"
)

// Rule is one tier's configured rate: RPM requests/minute, refilled
// continuously (converted to golang.org/x/time/rate's per-second Limit
// internally), and Burst tokens available immediately before the
// steady-state refill rate takes over. Burst matters most for TierSearch: a
// debounced type-ahead client firing several keystroke-triggered requests
// within a second or two needs burst headroom, not just a low steady-state
// rate — see internal/config.RateLimitConfig for the configured defaults
// and their rationale, and docs/API_POLICY.md for the numbers as published.
type Rule struct {
	RPM   float64
	Burst int
}

func (r Rule) valid() bool {
	return r.RPM > 0 && r.Burst > 0
}

func (r Rule) perSecond() rate.Limit {
	return rate.Limit(r.RPM / 60.0)
}

// DefaultIdleTTL bounds how long an idle (key, tier) bucket is kept before
// Limiter's cleanup loop evicts it — see Config.IdleTTL.
const DefaultIdleTTL = 10 * time.Minute

// DefaultCleanupInterval is how often Limiter sweeps for idle buckets.
// Deliberately much shorter than DefaultIdleTTL so eviction actually keeps
// up with churn rather than lagging behind it by a full TTL window.
const DefaultCleanupInterval = 2 * time.Minute

// Config holds the resolved tuning knobs for a Limiter. Built by
// cmd/api-server/main.go from internal/config.RateLimitConfig — this
// package deliberately doesn't import internal/config itself (see
// package doc) so it stays usable and testable without pulling in
// env-var parsing.
type Config struct {
	Read         Rule
	Search       Rule
	Export       Rule
	Unidentified Rule

	// IdleTTL is how long a (key, tier) bucket survives with no traffic
	// before Limiter's cleanup loop evicts it, bounding memory in a
	// long-running process against an ever-changing population of client
	// IPs. Zero uses DefaultIdleTTL.
	IdleTTL time.Duration
}

// DefaultConfig returns this package's own fallback numbers — used to fill
// any zero-valued (invalid) Rule left in a Config passed to NewLimiter, so a
// missing or malformed env var (internal/config.getEnvFloat/getEnvInt
// already fall back to a sane default themselves) can never produce a
// limiter that allows zero requests per tier. See internal/config's
// RateLimitConfig doc comment for why these specific numbers.
func DefaultConfig() Config {
	return Config{
		Read:         Rule{RPM: 60, Burst: 20},
		Search:       Rule{RPM: 30, Burst: 15},
		Export:       Rule{RPM: 5, Burst: 2},
		Unidentified: Rule{RPM: 5, Burst: 2},
		IdleTTL:      DefaultIdleTTL,
	}
}

func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if !c.Read.valid() {
		c.Read = d.Read
	}
	if !c.Search.valid() {
		c.Search = d.Search
	}
	if !c.Export.valid() {
		c.Export = d.Export
	}
	if !c.Unidentified.valid() {
		c.Unidentified = d.Unidentified
	}
	if c.IdleTTL <= 0 {
		c.IdleTTL = d.IdleTTL
	}
	return c
}

func (c Config) rule(tier Tier) Rule {
	switch tier {
	case TierRead:
		return c.Read
	case TierSearch:
		return c.Search
	case TierExport:
		return c.Export
	case Unidentified:
		return c.Unidentified
	default:
		// Not a tier this package defines. internal/api/ratelimit.go fails
		// closed to TierExport (the strictest cost tier) before ever
		// calling Allow with an unrecognized value, so reaching this in
		// production would mean that safeguard was bypassed — Export is
		// still the safest of the four configured rules to fall back to.
		return c.Export
	}
}

// bucket is one (key, tier) pair's token-bucket state, plus when it was
// last touched — the latter is all Limiter's cleanup loop needs to decide
// whether to evict it.
type bucket struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// Limiter owns one golang.org/x/time/rate.Limiter per distinct (key, tier)
// pair, built lazily on first use and evicted after Config.IdleTTL of
// inactivity. See the package doc for the honest per-instance scope this
// type operates under.
type Limiter struct {
	cfg Config

	mu      sync.Mutex
	buckets map[string]*bucket

	// now is time.Now by default; tests override it to avoid sleeping
	// through real refill windows.
	now func() time.Time

	stop      chan struct{}
	stopOnce  sync.Once
	closeWait sync.WaitGroup
}

// NewLimiter constructs a Limiter from cfg, starting its background
// cleanup loop. Call Close when done (cmd/api-server/main.go does this on
// shutdown) to stop that goroutine.
func NewLimiter(cfg Config) *Limiter {
	l := &Limiter{
		cfg:     cfg.withDefaults(),
		buckets: make(map[string]*bucket),
		now:     time.Now,
		stop:    make(chan struct{}),
	}
	l.closeWait.Add(1)
	go l.cleanupLoop()
	return l
}

// Close stops the background cleanup loop. Safe to call more than once;
// safe to omit entirely in short-lived tests that don't care about the
// goroutine outliving them, but cmd/api-server/main.go calls it on
// shutdown so the process can exit cleanly.
func (l *Limiter) Close() {
	l.stopOnce.Do(func() { close(l.stop) })
	l.closeWait.Wait()
}

func (l *Limiter) cleanupLoop() {
	defer l.closeWait.Done()
	ticker := time.NewTicker(DefaultCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			l.evictIdle()
		}
	}
}

func (l *Limiter) evictIdle() {
	cutoff := l.now().Add(-l.cfg.IdleTTL)
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.buckets {
		if b.lastSeen.Before(cutoff) {
			delete(l.buckets, k)
		}
	}
}

// Decision is the result of a single Allow call: whether the request may
// proceed, and the RateLimit-*/Retry-After values internal/api/ratelimit.go
// puts on the wire either way (this package's Decision doesn't include
// "successfully served on every 200" plumbing decisions — that's an HTTP
// concern internal/api owns; see checkNotModified's split for the same
// division of labor between an HTTP-shaped internal/api.go and a
// transport-agnostic package underneath it).
type Decision struct {
	Allowed bool
	// EffectiveTier is the tier the decision was actually evaluated
	// against — equal to the requested tier for an identified caller, or
	// Unidentified when the caller wasn't (see Allow's identified
	// parameter).
	EffectiveTier Tier
	// Limit is the tier's configured requests-per-minute, rounded to the
	// nearest whole number for the RateLimit-Limit header.
	Limit int
	// Remaining is the number of additional requests currently permitted
	// in this bucket, floored to a whole number for RateLimit-Remaining.
	Remaining int
	// ResetSeconds is RateLimit-Reset: seconds until this bucket reports a
	// fresh Remaining if no further requests are made. On a denied
	// decision this is the same value as RetryAfterSeconds — see Allow's
	// comment for why those are deliberately equal in that case.
	ResetSeconds int
	// RetryAfterSeconds is only meaningful when Allowed is false — the
	// value internal/api/ratelimit.go puts on the wire as Retry-After.
	RetryAfterSeconds int
}

// Allow records one request attempt against key's bucket for tier (or for
// Unidentified instead, if identified is false — see Unidentified's doc
// comment) and reports whether it's within budget.
//
// The reserve-then-cancel pattern below (rate.Limiter.ReserveN, checked,
// then CancelAt if it would have required a wait) is the standard way to
// get an accurate immediate allow/deny decision plus a correct
// Retry-After out of golang.org/x/time/rate: Allow()/AllowN() alone only
// return a bool, with no way to recover how long the caller should wait,
// and Wait()/WaitN() would block this goroutine for that duration instead
// of returning a 429 immediately — neither fits an HTTP handler.
func (l *Limiter) Allow(key string, tier Tier, identified bool) Decision {
	effectiveTier := tier
	if !identified {
		effectiveTier = Unidentified
	}
	rule := l.cfg.rule(effectiveTier)
	lim := l.bucketFor(key, effectiveTier, rule)

	now := l.now()
	limit := int(math.Round(rule.RPM))

	reservation := lim.ReserveN(now, 1)
	if !reservation.OK() {
		// n=1 can never be satisfied by this limiter's configured burst
		// (Burst < 1) — only reachable with a misconfigured Rule, since
		// withDefaults/valid already reject Burst <= 0. Fail closed rather
		// than panic or silently allow.
		return Decision{
			Allowed:           false,
			EffectiveTier:     effectiveTier,
			Limit:             limit,
			Remaining:         0,
			ResetSeconds:      1,
			RetryAfterSeconds: 1,
		}
	}

	if delay := reservation.DelayFrom(now); delay > 0 {
		// Give the token back — this request is being denied, not made to
		// wait, so it must not consume budget a subsequent request could
		// have used.
		reservation.CancelAt(now)
		wait := ceilSeconds(delay)
		return Decision{
			Allowed:           false,
			EffectiveTier:     effectiveTier,
			Limit:             limit,
			Remaining:         0,
			ResetSeconds:      wait,
			RetryAfterSeconds: wait,
		}
	}

	remaining := int(math.Floor(lim.TokensAt(now)))
	if remaining < 0 {
		remaining = 0
	}
	return Decision{
		Allowed:       true,
		EffectiveTier: effectiveTier,
		Limit:         limit,
		Remaining:     remaining,
		ResetSeconds:  refillSeconds(lim, rule, now),
	}
}

func (l *Limiter) bucketFor(key string, tier Tier, rule Rule) *rate.Limiter {
	mapKey := key + "\x00" + string(tier)

	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[mapKey]
	if !ok {
		b = &bucket{limiter: rate.NewLimiter(rule.perSecond(), rule.Burst)}
		l.buckets[mapKey] = b
	}
	b.lastSeen = l.now()
	return b.limiter
}

// refillSeconds estimates seconds until lim's bucket would report itself
// fully topped up to rule.Burst again if no further requests arrive — the
// "RateLimit-Reset" value on a successful response. Token-bucket limiting
// has no discrete window the way a fixed-window counter does, so this is a
// deliberate, documented interpretation ("time to a completely fresh
// budget"), not a value pulled from an authoritative window boundary.
func refillSeconds(lim *rate.Limiter, rule Rule, now time.Time) int {
	deficit := float64(rule.Burst) - lim.TokensAt(now)
	if deficit <= 0 {
		return 0
	}
	perSecond := rule.RPM / 60.0
	if perSecond <= 0 {
		return 0
	}
	return int(math.Ceil(deficit / perSecond))
}

func ceilSeconds(d time.Duration) int {
	secs := int(math.Ceil(d.Seconds()))
	if secs < 1 {
		secs = 1
	}
	return secs
}
