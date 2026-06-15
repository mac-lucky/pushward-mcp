package oauth

import (
	"sync"
	"time"
)

// credCache holds recently-decrypted hlk_ keys keyed by user id so the hot
// /mcp path avoids a Postgres read plus an HKDF/AES-GCM derivation on every
// JSON-RPC call (initialize, tools/list, every tools/call). Entries are short-
// lived so a credential rotation or revocation propagates quickly, and the map
// is hard-capped so a flood of distinct users cannot grow it without bound.
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

// get returns the cached key for userID if present and unexpired.
func (c *credCache) get(userID string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[userID]
	if !ok || c.now().After(e.exp) {
		return "", false
	}
	return e.hlk, true
}

// put stores hlk for userID with the cache TTL, evicting expired (then, if still
// at capacity, an arbitrary) entry so the map stays bounded.
func (c *credCache) put(userID, hlk string) {
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
	c.entries[userID] = credEntry{hlk: hlk, exp: c.now().Add(c.ttl)}
}

// invalidate drops a user's cached key (e.g. on a decrypt failure).
func (c *credCache) invalidate(userID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, userID)
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
