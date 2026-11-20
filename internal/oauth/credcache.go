package oauth

import (
	"sync"
	"time"
)

// credCache holds recently-decrypted hlk_ keys keyed by user and grant
// credential (see loadHLK) so the hot /mcp path avoids a Postgres read plus an
// HKDF/AES-GCM derivation on every JSON-RPC call (initialize, tools/list, every
// tools/call). Entries are short-lived so a credential rotation or revocation
// propagates quickly, and the map is hard-capped so a flood of distinct grants
// cannot grow it without bound.
//
// Caching the plaintext key in memory is acceptable: the process already holds
// the master key and decrypts the key transiently per request today.
type credCache struct {
	mu      sync.Mutex
	entries map[string]credEntry
	ttl     time.Duration
	max     int
	now     func() time.Time
}

type credEntry struct {
	hlk string
	exp time.Time
}

func newCredCache(ttl time.Duration, max int, now func() time.Time) *credCache {
	return &credCache{entries: make(map[string]credEntry), ttl: ttl, max: max, now: now}
}

// get returns the cached hlk for key if present and unexpired.
func (c *credCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || c.now().After(e.exp) {
		return "", false
	}
	return e.hlk, true
}

// put stores hlk under key with the cache TTL, evicting expired (then, if still
// at capacity, an arbitrary) entry so the map stays bounded.
func (c *credCache) put(key, hlk string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.max {
		c.evictExpiredLocked()
		if len(c.entries) >= c.max {
			for k := range c.entries { // bounded fallback: drop one entry
				delete(c.entries, k)
				break
			}
		}
	}
	c.entries[key] = credEntry{hlk: hlk, exp: c.now().Add(c.ttl)}
}

// invalidate drops a cached key (e.g. on a decrypt failure).
func (c *credCache) invalidate(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// setNow swaps the clock under the lock so the background janitor's sweep (which
// reads c.now concurrently) never races a test-only clock override.
func (c *credCache) setNow(now func() time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// sweep removes all expired entries; called periodically by the janitor.
func (c *credCache) sweep() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evictExpiredLocked()
}

func (c *credCache) evictExpiredLocked() {
	now := c.now()
	for k, e := range c.entries {
		if now.After(e.exp) {
			delete(c.entries, k)
		}
	}
}
