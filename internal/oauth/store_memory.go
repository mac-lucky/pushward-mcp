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
	cp := *c
	m.clients[c.ID] = &cp
	return nil
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
	if mc.ac.ExpiresAt.Before(m.now()) {
		delete(m.codes, codeHash)
		return nil, ErrNotFound
	}
	if mc.usedAt != nil {
		return nil, ErrCodeAlreadyUsed
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
	for h, mc := range m.codes {
		if mc.ac.ExpiresAt.Before(now) || (mc.usedAt != nil && now.Sub(*mc.usedAt) > authCodeTTL) {
			delete(m.codes, h)
		}
	}
	for h, rt := range m.refresh {
		if rt.ExpiresAt.Before(now) || (rt.RevokedAt != nil && now.Sub(*rt.RevokedAt) > refreshGrace) {
			delete(m.refresh, h)
		}
	}
	return nil
}

func (m *memoryStore) Close() {}
