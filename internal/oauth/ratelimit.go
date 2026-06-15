package oauth

import (
	"sync"
	"time"
)

// maxLimiterKeys hard-caps the number of distinct keys a limiter tracks so a
// flood of unique source IPs cannot grow the map without bound between the
// time-based GC sweeps. ~80 B/entry keeps the worst case in single-digit MB.
const maxLimiterKeys = 100_000

// keyedLimiter is a small token-bucket rate limiter keyed by an arbitrary
// string (client IP, client_id). It avoids an external dependency and is safe
// for concurrent use. Stale buckets are evicted opportunistically and the map
// is size-capped (fail-closed) so it can never grow without bound.
type keyedLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // tokens added per second
	burst   float64 // max tokens
	max     int
	now     func() time.Time
	lastGC  time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// newKeyedLimiter allows `burst` requests instantly and refills at
// `perMinute/60` tokens/sec.
func newKeyedLimiter(perMinute, burst int) *keyedLimiter {
	return &keyedLimiter{
		buckets: map[string]*bucket{},
		rate:    float64(perMinute) / 60.0,
		burst:   float64(burst),
		max:     maxLimiterKeys,
		now:     time.Now,
	}
}

// Allow reports whether a request keyed by k may proceed.
func (l *keyedLimiter) Allow(k string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()

	if now.Sub(l.lastGC) > 10*time.Minute {
		for key, b := range l.buckets {
			if now.Sub(b.last) > 10*time.Minute {
				delete(l.buckets, key)
			}
		}
		l.lastGC = now
	}

	b, ok := l.buckets[k]
	if !ok {
		// At capacity for a new key: aggressively evict recently-idle buckets to
		// make room; if the map is still full, fail closed (deny) rather than grow.
		if len(l.buckets) >= l.max {
			for key, bb := range l.buckets {
				if now.Sub(bb.last) > time.Minute {
					delete(l.buckets, key)
				}
			}
			if len(l.buckets) >= l.max {
				return false
			}
		}
		l.buckets[k] = &bucket{tokens: l.burst - 1, last: now}
		return true
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
