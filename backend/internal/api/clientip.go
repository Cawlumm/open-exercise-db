package api

import (
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/Cawlumm/open-exercise-db/internal/config"
)

// clientKey is the identity internal/ratelimit.Limiter buckets on: the client's
// address, derived according to mode (config.Config.TrustedProxyMode).
//
// A forwarding header is only as honest as whoever wrote it, so which one to
// read depends on what actually sits in front of this process:
//
//   - direct (default): nothing is believed; the TCP peer is the client.
//   - cloudrun: Cloud Run's front end appends the address it saw to
//     X-Forwarded-For, so the right-most entry is the only one not controlled
//     by the client. The left-most is whatever the caller typed, and using it
//     gave every request a fresh bucket for the price of a random header.
//   - cloudflare: CF-Connecting-IP, which Cloudflare overwrites. Only correct
//     when the origin is reachable solely through Cloudflare; in any other mode
//     the header is ignored, because anyone can send it.
//
// Anything that is not a literal IP falls back to the TCP peer rather than
// becoming a bucket key.
func clientKey(ctx huma.Context, mode string) string {
	switch mode {
	case config.ProxyCloudflare:
		if ip := parseIP(ctx.Header("CF-Connecting-IP")); ip != "" {
			return ip
		}
	case config.ProxyCloudRun:
		if ip := parseIP(lastForwarded(ctx.Header("X-Forwarded-For"))); ip != "" {
			return ip
		}
	}
	if host, _, err := net.SplitHostPort(ctx.RemoteAddr()); err == nil && host != "" {
		return host
	}
	return ctx.RemoteAddr()
}

func lastForwarded(xff string) string {
	if i := strings.LastIndex(xff, ","); i >= 0 {
		xff = xff[i+1:]
	}
	return xff
}

func parseIP(v string) string {
	addr, err := netip.ParseAddr(strings.TrimSpace(v))
	if err != nil {
		return ""
	}
	return addr.Unmap().String()
}

const (
	negativeKeyTTL = 30 * time.Second
	negativeKeyMax = 4096
)

// negativeKeyCache remembers key ids the database has just said it does not
// hold. Bounded: when full it is cleared, which only costs a few repeat lookups.
type negativeKeyCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	entries map[string]time.Time
}

func newNegativeKeyCache(ttl time.Duration, max int) *negativeKeyCache {
	return &negativeKeyCache{ttl: ttl, max: max, entries: make(map[string]time.Time)}
}

func (c *negativeKeyCache) seen(keyID string, now time.Time) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	at, ok := c.entries[keyID]
	if !ok {
		return false
	}
	if now.Sub(at) > c.ttl {
		delete(c.entries, keyID)
		return false
	}
	return true
}

func (c *negativeKeyCache) add(keyID string, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.max {
		clear(c.entries)
	}
	c.entries[keyID] = now
}
