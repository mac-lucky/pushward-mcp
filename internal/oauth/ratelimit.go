package oauth

import (
	"net/netip"
	"sync"
	"time"
)

// maxLimiterKeys hard-caps the number of distinct keys a limiter tracks so a
// flood of unique source IPs cannot grow the map without bound between the
// time-based GC sweeps. ~80 B/entry keeps the worst case in single-digit MB.
const maxLimiterKeys = 100_000

// keyedLimiter is a small token-bucket rate limiter keyed by an arbitrary
// string (see rateKey). It avoids an external dependency and is safe for
// concurrent use. Stale buckets are evicted opportunistically and the map is
// size-capped; a full map makes room rather than refusing new keys, so a flood
// of addresses cannot lock out legitimate ones.
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

// Allow reports whether a request keyed by k may proceed, spending a token if
// so.
func (l *keyedLimiter) Allow(k string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.bucketLocked(k)
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Peek reports whether k has a token left, without spending it. With Charge it
// limits only the requests that turn out to fail.
func (l *keyedLimiter) Peek(k string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[k]
	if !ok {
		return true
	}
	l.refillLocked(b, l.now())
	return b.tokens >= 1
}

// Charge spends one of k's tokens, if it has one left.
func (l *keyedLimiter) Charge(k string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.bucketLocked(k)
	b.tokens = max(b.tokens-1, 0)
}

// bucketLocked returns k's bucket refilled up to now, creating a full one if k
// has none. Caller holds l.mu.
func (l *keyedLimiter) bucketLocked(k string) *bucket {
	now := l.now()
	if now.Sub(l.lastGC) > 10*time.Minute {
		for key, b := range l.buckets {
			if now.Sub(b.last) > 10*time.Minute {
				delete(l.buckets, key)
			}
		}
		l.lastGC = now
	}
	if b, ok := l.buckets[k]; ok {
		l.refillLocked(b, now)
		return b
	}
	if len(l.buckets) >= l.max {
		// A bucket idle for a minute has refilled, so dropping it loses nothing.
		// If every key is that active, drop an arbitrary one: at worst it gets a
		// fresh burst, where refusing the new key would lock its owner out.
		for key, b := range l.buckets {
			if now.Sub(b.last) > time.Minute {
				delete(l.buckets, key)
			}
		}
		if len(l.buckets) >= l.max {
			for key := range l.buckets {
				delete(l.buckets, key)
				break
			}
		}
	}
	b := &bucket{tokens: l.burst, last: now}
	l.buckets[k] = b
	return b
}

func (l *keyedLimiter) refillLocked(b *bucket, now time.Time) {
	b.tokens = min(b.tokens+now.Sub(b.last).Seconds()*l.rate, l.burst)
	b.last = now
}

// rateKey is the limiter key of a client address: an IPv4 address as is, an
// IPv6 address by its /64, which one subscriber usually holds whole and can
// rotate through freely. Anything unparseable is used verbatim.
func rateKey(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	if a = a.Unmap(); a.Is4() {
		return a.String()
	}
	p, err := a.WithZone("").Prefix(64)
	if err != nil {
		return ip
	}
	return p.String()
}
