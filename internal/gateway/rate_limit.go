package gateway

// P5-5: a small per-key / per-IP sliding-window limiter. It is deliberately
// in-memory and coarse: heavy protection belongs at the reverse proxy
// (Caddy rate_limit / fail2ban), while this stops one client from exhausting
// the host's own CPU before the proxy sees it.

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

const rateWindow = time.Minute

type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string][]int64 // key → accepted timestamps (unix seconds)
	lastGC  int64
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{buckets: make(map[string][]int64)}
}

// allow records one request and reports whether it is inside the limit.
// limit <= 0 means "off". retryAfter is the seconds until the oldest hit of
// the window expires.
func (l *rateLimiter) allow(key string, limit int, now int64) (allowed bool, retryAfter int) {
	if limit <= 0 || key == "" {
		return true, 0
	}
	cutoff := now - int64(rateWindow/time.Second)
	l.mu.Lock()
	defer l.mu.Unlock()
	hits := l.buckets[key]
	kept := hits[:0]
	for _, at := range hits {
		if at > cutoff {
			kept = append(kept, at)
		}
	}
	if len(kept) >= limit {
		l.buckets[key] = kept
		wait := kept[0] + int64(rateWindow/time.Second) - now
		if wait < 1 {
			wait = 1
		}
		return false, int(wait)
	}
	l.buckets[key] = append(kept, now)
	l.gcLocked(now)
	return true, 0
}

// gcLocked drops empty/stale buckets at most once a minute so a burst of
// one-off IPs cannot grow the map forever.
func (l *rateLimiter) gcLocked(now int64) {
	if now-l.lastGC < 60 {
		return
	}
	l.lastGC = now
	cutoff := now - int64(rateWindow/time.Second)
	for key, hits := range l.buckets {
		if len(hits) == 0 || hits[len(hits)-1] <= cutoff {
			delete(l.buckets, key)
		}
	}
}

// SetRateLimits applies the per-key and per-IP limits live (0 disables one).
func (g *Gateway) SetRateLimits(perKey, perIP int) {
	if g == nil {
		return
	}
	g.ratePerKey.Store(int64(perKey))
	g.ratePerIP.Store(int64(perIP))
}

func (g *Gateway) ipRateAllowed(r *http.Request) (bool, int) {
	limit := int(g.ratePerIP.Load())
	if limit <= 0 {
		return true, 0
	}
	ip := g.clientIP(r)
	if ip == nil {
		return true, 0
	}
	return g.limiter.allow("ip:"+ip.String(), limit, g.now().Unix())
}

func (g *Gateway) keyRateAllowed(keyID string) (bool, int) {
	limit := int(g.ratePerKey.Load())
	if limit <= 0 || keyID == "" {
		return true, 0
	}
	return g.limiter.allow("key:"+keyID, limit, g.now().Unix())
}

func writeRateLimited(w http.ResponseWriter, retryAfter int) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
}
