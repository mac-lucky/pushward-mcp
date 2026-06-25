package oauth

import (
	"context"
	"sync"
	"time"
)

// memoryStore is an in-process Store for single-replica development and tests.
// Production multi-replica deployments must use the Postgres store so codes and
// refresh tokens are shared across pods.
type memoryStore struct {
	mu      sync.Mutex
	clients map[string]*Client
	codes   map[string]*memCode
	refresh map[string]*RefreshToken
	creds   map[string][]byte
	now     func() time.Time
}

// memCode tracks an auth code's single-use state so a replay is distinguishable
// from an absent/expired code (the Postgres store distinguishes these via
// used_at; the memory store must match that contract).
type memCode struct {
	ac     AuthCode
	usedAt *time.Time
}

func newMemoryStore() *memoryStore {
	return &memoryStore{
		clients: map[string]*Client{},
		codes:   map[string]*memCode{},
		refresh: map[string]*RefreshToken{},
		creds:   map[string][]byte{},
		now:     time.Now,
	}
}

func (m *memoryStore) SaveClient(_ context.Context, c *Client) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	cp := *c
	cp.UpdatedAt = now
	if existing, ok := m.clients[c.ID]; ok {
		cp.CreatedAt = existing.CreatedAt // preserve original creation time on upsert
	} else {
		if cp.CreatedAt.IsZero() {
			cp.CreatedAt = now
		}
		// New key: keep the map bounded. Evict expired-by-retention clients, then,
		// if still at the cap, the least-recently-written one.
		if len(m.clients) >= clientCacheMax {
			m.evictStaleClientsLocked(now)
			if len(m.clients) >= clientCacheMax {
				m.evictOldestClientLocked()
			}
		}
	}
	m.clients[c.ID] = &cp
	return nil
}

// evictOldestClientLocked removes the client with the oldest UpdatedAt. Caller
// holds m.mu.
func (m *memoryStore) evictOldestClientLocked() {
	var oldestID string
	var oldest time.Time
	for id, c := range m.clients {
		if oldestID == "" || c.UpdatedAt.Before(oldest) {
			oldestID, oldest = id, c.UpdatedAt
		}
	}
	if oldestID != "" {
		delete(m.clients, oldestID)
	}
}

// evictStaleClientsLocked drops clients past clientRetention that hold no refresh
// token and no live auth code, mirroring the Postgres Cleanup. Caller holds m.mu.
func (m *memoryStore) evictStaleClientsLocked(now time.Time) {
	for id, c := range m.clients {
		if now.Sub(c.UpdatedAt) <= clientRetention {
			continue
		}
		if m.clientHasTokenLocked(id) || m.clientHasCodeLocked(id) {
			continue
		}
		delete(m.clients, id)
	}
}

func (m *memoryStore) clientHasTokenLocked(clientID string) bool {
	for _, rt := range m.refresh {
		if rt.ClientID == clientID {
			return true
		}
	}
	return false
}

func (m *memoryStore) clientHasCodeLocked(clientID string) bool {
	for _, mc := range m.codes {
		if mc.ac.ClientID == clientID {
			return true
		}
	}
	return false
}

func (m *memoryStore) GetClient(_ context.Context, id string) (*Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.clients[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *c
	return &cp, nil
}

func (m *memoryStore) SaveAuthCode(_ context.Context, ac *AuthCode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.codes[ac.CodeHash] = &memCode{ac: *ac}
	return nil
}

func (m *memoryStore) ConsumeAuthCode(_ context.Context, codeHash string) (*AuthCode, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mc, ok := m.codes[codeHash]
	if !ok {
		return nil, ErrNotFound
	}
	// Check reuse BEFORE expiry (and do not delete on read) so a replay of an
	// expired-but-used code is still reported as ErrCodeAlreadyUsed with its
	// user_id — matching the Postgres store's retention. Cleanup handles eviction.
	if mc.usedAt != nil {
		return &AuthCode{CodeHash: codeHash, UserID: mc.ac.UserID}, ErrCodeAlreadyUsed
	}
	if mc.ac.ExpiresAt.Before(m.now()) {
		return nil, ErrNotFound
	}
	t := m.now()
	mc.usedAt = &t
	cp := mc.ac
	return &cp, nil
}

func (m *memoryStore) SaveRefreshToken(_ context.Context, rt *RefreshToken) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *rt
	m.refresh[rt.TokenHash] = &cp
	return nil
}

func (m *memoryStore) GetRefreshToken(_ context.Context, tokenHash string) (*RefreshToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rt, ok := m.refresh[tokenHash]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *rt
	return &cp, nil
}

func (m *memoryStore) RevokeRefreshToken(_ context.Context, tokenHash string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rt, ok := m.refresh[tokenHash]
	if !ok || rt.RevokedAt != nil {
		return false, nil
	}
	t := m.now()
	rt.RevokedAt = &t
	return true, nil
}

func (m *memoryStore) RevokeUserRefreshTokens(_ context.Context, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.now()
	for _, rt := range m.refresh {
		if rt.UserID == userID && rt.RevokedAt == nil {
			rt.RevokedAt = &t
		}
	}
	return nil
}

func (m *memoryStore) SaveUserCredential(_ context.Context, userID string, encryptedHLK []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := make([]byte, len(encryptedHLK))
	copy(b, encryptedHLK)
	m.creds[userID] = b
	return nil
}

func (m *memoryStore) GetUserCredential(_ context.Context, userID string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.creds[userID]
	if !ok {
		return nil, ErrNotFound
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	return cp, nil
}

func (m *memoryStore) Cleanup(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	// Keep codes (used or not) until 1h past expiry so a replay stays detectable as
	// ErrCodeAlreadyUsed, matching the Postgres retention window.
	for h, mc := range m.codes {
		if mc.ac.ExpiresAt.Before(now.Add(-time.Hour)) {
			delete(m.codes, h)
		}
	}
	for h, rt := range m.refresh {
		if rt.ExpiresAt.Before(now) || (rt.RevokedAt != nil && now.Sub(*rt.RevokedAt) > time.Hour) {
			delete(m.refresh, h)
		}
	}
	m.evictStaleClientsLocked(now)
	return nil
}

// setNow swaps the clock under the lock so a concurrent reader (the janitor's
// sweep, or a request) never races the test-only clock override.
func (m *memoryStore) setNow(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = now
}

func (m *memoryStore) Close() {}
