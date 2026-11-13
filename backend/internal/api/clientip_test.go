package api

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2/humatest"

	"github.com/Cawlumm/open-exercise-db/internal/config"
	"github.com/Cawlumm/open-exercise-db/internal/export"
	"github.com/Cawlumm/open-exercise-db/internal/media"
	"github.com/Cawlumm/open-exercise-db/internal/ratelimit"
)

func newProxyModeAPI(t *testing.T, mode string) humatest.TestAPI {
	t.Helper()
	s, _ := newTestStore(t)
	limiter := ratelimit.NewLimiter(ratelimit.Config{
		Flood:        ratelimit.Rule{RPM: 6000, Burst: 6000},
		App:          ratelimit.Rule{RPM: 50, Burst: 50},
		Read:         ratelimit.Rule{RPM: 3, Burst: 3},
		Search:       ratelimit.Rule{RPM: 3, Burst: 3},
		Export:       ratelimit.Rule{RPM: 3, Burst: 3},
		Unidentified: ratelimit.Rule{RPM: 3, Burst: 3},
	})
	t.Cleanup(limiter.Close)
	mediaStore, err := media.NewFilesystemStore(t.TempDir())
	if err != nil {
		t.Fatalf("init media store: %v", err)
	}
	_, hapi := humatest.New(t)
	Register(hapi, s, export.NewCache(export.DefaultCacheTTL, ""), limiter, mediaStore, "", AuthConfig{TrustedProxyMode: mode})
	return hapi
}

// allowedWith sends n requests, header i built by hdr(i), and counts the 200s.
func allowedWith(t *testing.T, api humatest.TestAPI, n int, hdr func(i int) []any) int {
	t.Helper()
	allowed := 0
	for i := range n {
		args := append([]any{"User-Agent: " + identifyingUA}, hdr(i)...)
		switch resp := api.Get(readPath, args...); resp.Code {
		case http.StatusOK:
			allowed++
		case http.StatusTooManyRequests:
		default:
			t.Fatalf("unexpected status %d", resp.Code)
		}
	}
	return allowed
}

func TestClientKey_DirectIgnoresEveryForwardingHeader(t *testing.T) {
	api := newProxyModeAPI(t, config.ProxyDirect)
	got := allowedWith(t, api, 20, func(i int) []any {
		return []any{
			fmt.Sprintf("CF-Connecting-IP: 203.0.113.%d", i+1),
			fmt.Sprintf("X-Forwarded-For: 198.51.100.%d, 192.0.2.9", i+1),
		}
	})
	if got != 3 {
		t.Fatalf("allowed %d of 20 with rotating spoofed headers, want 3", got)
	}
}

func TestClientKey_CloudRunUsesRightmostForwardedEntry(t *testing.T) {
	t.Run("a forged left-most entry buys no new bucket", func(t *testing.T) {
		api := newProxyModeAPI(t, config.ProxyCloudRun)
		got := allowedWith(t, api, 20, func(i int) []any {
			return []any{
				fmt.Sprintf("X-Forwarded-For: 198.51.100.%d, 203.0.113.7", i+1),
				fmt.Sprintf("CF-Connecting-IP: 198.51.100.%d", i+1),
			}
		})
		if got != 3 {
			t.Fatalf("allowed %d of 20, want 3: the client-controlled left-most entry or CF-Connecting-IP was believed", got)
		}
	})

	t.Run("distinct right-most entries are distinct clients", func(t *testing.T) {
		api := newProxyModeAPI(t, config.ProxyCloudRun)
		got := allowedWith(t, api, 4, func(i int) []any {
			return []any{fmt.Sprintf("X-Forwarded-For: 1.1.1.1, 203.0.113.%d", i+1)}
		})
		if got != 4 {
			t.Fatalf("allowed %d of 4 distinct clients, want 4", got)
		}
	})

	t.Run("garbage falls back to the peer", func(t *testing.T) {
		api := newProxyModeAPI(t, config.ProxyCloudRun)
		got := allowedWith(t, api, 20, func(i int) []any {
			return []any{fmt.Sprintf("X-Forwarded-For: not-an-ip-%d", i)}
		})
		if got != 3 {
			t.Fatalf("allowed %d of 20, want 3", got)
		}
	})
}

func TestClientKey_CloudflareTrustsOnlyItsHeader(t *testing.T) {
	api := newProxyModeAPI(t, config.ProxyCloudflare)
	got := allowedWith(t, api, 4, func(i int) []any {
		return []any{fmt.Sprintf("CF-Connecting-IP: 203.0.113.%d", i+1)}
	})
	if got != 4 {
		t.Fatalf("cloudflare mode allowed %d of 4 distinct clients, want 4", got)
	}

	api = newProxyModeAPI(t, config.ProxyCloudflare)
	got = allowedWith(t, api, 20, func(i int) []any {
		return []any{fmt.Sprintf("X-Forwarded-For: 203.0.113.%d", i+1)}
	})
	if got != 3 {
		t.Fatalf("cloudflare mode believed X-Forwarded-For: allowed %d of 20, want 3", got)
	}
}

func TestNegativeKeyCache(t *testing.T) {
	c := newNegativeKeyCache(time.Second, 2)
	now := time.Now()
	if c.seen("a", now) {
		t.Fatal("empty cache reported a hit")
	}
	c.add("a", now)
	if !c.seen("a", now.Add(500*time.Millisecond)) {
		t.Fatal("fresh entry not reported")
	}
	if c.seen("a", now.Add(2*time.Second)) {
		t.Fatal("expired entry reported")
	}
	c.add("a", now)
	c.add("b", now)
	c.add("c", now) // over max: cleared, then c stored
	if c.seen("a", now) || !c.seen("c", now) {
		t.Fatal("bounded cache did not reset on overflow")
	}
	var nilCache *negativeKeyCache
	nilCache.add("x", now)
	if nilCache.seen("x", now) {
		t.Fatal("nil cache reported a hit")
	}
}
