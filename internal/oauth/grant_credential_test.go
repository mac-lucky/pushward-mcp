package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// mcpKey calls /mcp with access and returns the PushWard key WrapMCP injected.
func mcpKey(t *testing.T, srv *httptest.Server, access string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("/mcp status = %d", res.StatusCode)
	}
	var seen map[string]string
	_ = json.NewDecoder(res.Body).Decode(&seen)
	return seen["token"]
}

// connect runs a full consent and code exchange for a new client with apiKey.
func connect(t *testing.T, srv *httptest.Server, apiKey string) (access, refresh string) {
	t.Helper()
	clientID, verifier, redirect, code := obtainCode(t, srv, apiKey)
	return exchangeForTokens(t, srv, code, clientID, verifier, redirect)
}

func refreshGrant(t *testing.T, srv *httptest.Server, refresh string) (string, string) {
	t.Helper()
	status, m := tokenPost(t, srv, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
	if status != http.StatusOK {
		t.Fatalf("refresh failed: %d %v", status, m)
	}
	a, _ := m["access_token"].(string)
	r, _ := m["refresh_token"].(string)
	return a, r
}

func TestGrantKey_SecondConsentKeepsFirstClientKey(t *testing.T) {
	srv, p := newTestProvider(t)
	clk := &fakeClock{t: time.Now()}
	p.setClock(clk.now)

	access1, refresh1 := connect(t, srv, "hlk_k1")
	if got := mcpKey(t, srv, access1); got != "hlk_k1" {
		t.Fatalf("client 1 sees %q, want hlk_k1", got)
	}
	access2, _ := connect(t, srv, "hlk_k0")
	// Both grants of the one user are cached now; neither may read the other's.
	for range 2 {
		if got := mcpKey(t, srv, access2); got != "hlk_k0" {
			t.Fatalf("client 2 sees %q, want hlk_k0", got)
		}
		if got := mcpKey(t, srv, access1); got != "hlk_k1" {
			t.Fatalf("client 1 sees %q after client 2 connected, want hlk_k1", got)
		}
	}

	clk.advance(credCacheTTL + time.Second)
	if got := mcpKey(t, srv, access1); got != "hlk_k1" {
		t.Fatalf("client 1 sees %q past the cache TTL, want hlk_k1", got)
	}
	access1b, _ := refreshGrant(t, srv, refresh1)
	if got := mcpKey(t, srv, access1b); got != "hlk_k1" {
		t.Fatalf("client 1 sees %q after a refresh, want hlk_k1", got)
	}
}

func TestGrantKey_AbandonedConsentChangesNothing(t *testing.T) {
	srv, p := newTestProvider(t)
	clk := &fakeClock{t: time.Now()}
	p.setClock(clk.now)
	m := p.store.(*memoryStore)

	access1, _ := connect(t, srv, "hlk_k1")
	_, _, _, _ = obtainCode(t, srv, "hlk_k0") // consent, never exchanged

	clk.advance(credCacheTTL + time.Second)
	if got := mcpKey(t, srv, access1); got != "hlk_k1" {
		t.Fatalf("client 1 sees %q after an abandoned consent, want hlk_k1", got)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.creds) != 1 || len(m.userCreds) != 0 {
		t.Fatalf("an abandoned consent stored a credential: %d grant, %d per-user", len(m.creds), len(m.userCreds))
	}
}

func TestGrantKey_LegacyTokenResolvesUserRow(t *testing.T) {
	srv, p := newTestProvider(t)
	clk := &fakeClock{t: time.Now()}
	p.setClock(clk.now)
	m := p.store.(*memoryStore)
	ctx := context.Background()

	// State a pre-upgrade consent left: the per-user row and a refresh token
	// without a credential id.
	blob, err := p.crypto.Encrypt("user-123", "hlk_legacy")
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.userCreds["user-123"] = blob
	m.mu.Unlock()
	if err := m.SaveRefreshToken(ctx, &RefreshToken{
		TokenHash: hashToken("rtok-legacy"), UserID: "user-123", ClientID: "c-old", Scope: "mcp",
		Resource: "https://mcp.test", ExpiresAt: clk.now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	legacyAccess, err := p.signer.Sign("user-123", "https://mcp.test", "c-old", "mcp", "", clk.now())
	if err != nil {
		t.Fatal(err)
	}
	if got := mcpKey(t, srv, legacyAccess); got != "hlk_legacy" {
		t.Fatalf("legacy token sees %q, want hlk_legacy", got)
	}

	// A new consent must leave the per-user row alone.
	_, _ = connect(t, srv, "hlk_k0")
	clk.advance(credCacheTTL + time.Second)
	if got := mcpKey(t, srv, legacyAccess); got != "hlk_legacy" {
		t.Fatalf("legacy token sees %q after a new consent, want hlk_legacy", got)
	}
	refreshed, _ := refreshGrant(t, srv, "rtok-legacy")
	if c, err := p.signer.Verify(refreshed, "https://mcp.test"); err != nil || c.CredentialID != "" {
		t.Fatalf("refreshing a legacy grant must keep it on the per-user row: %+v %v", c, err)
	}
	if got := mcpKey(t, srv, refreshed); got != "hlk_legacy" {
		t.Fatalf("refreshed legacy token sees %q, want hlk_legacy", got)
	}
}

func TestGrantKey_RefreshKeepsCredentialID(t *testing.T) {
	srv, p := newTestProvider(t)
	access, refresh := connect(t, srv, "hlk_k1")
	first, err := p.signer.Verify(access, "https://mcp.test")
	if err != nil || first.CredentialID == "" {
		t.Fatalf("code exchange must issue a credential id: %+v %v", first, err)
	}
	for range 2 {
		access, refresh = refreshGrant(t, srv, refresh)
		c, err := p.signer.Verify(access, "https://mcp.test")
		if err != nil || c.CredentialID != first.CredentialID {
			t.Fatalf("refresh changed the credential id: %q -> %+v %v", first.CredentialID, c, err)
		}
		rt, err := p.store.GetRefreshToken(context.Background(), hashToken(refresh))
		if err != nil || rt.CredentialID != first.CredentialID {
			t.Fatalf("rotated refresh token lost the credential id: %+v %v", rt, err)
		}
	}
}

// A credential id is only honoured for the user it was issued to.
func TestGrantKey_CredentialBoundToSubject(t *testing.T) {
	srv, p := newTestProvider(t)
	access, _ := connect(t, srv, "hlk_k1")
	c, err := p.signer.Verify(access, "https://mcp.test")
	if err != nil {
		t.Fatal(err)
	}
	forged, _ := p.signer.Sign("user-other", "https://mcp.test", c.ClientID, "mcp", c.CredentialID, time.Now())
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+forged)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("another subject with this credential id got %d, want 401", res.StatusCode)
	}
}

func TestMemoryStore_PendingKeyAndCredentialCleanup(t *testing.T) {
	m := newMemoryStore()
	ctx := context.Background()
	clk := time.Now()
	m.now = func() time.Time { return clk }

	if err := m.SaveAuthCode(ctx, &AuthCode{CodeHash: "h", UserID: "u", EncryptedHLK: []byte("pending"), ExpiresAt: clk.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	ac, err := m.ConsumeAuthCode(ctx, "h")
	if err != nil || string(ac.EncryptedHLK) != "pending" {
		t.Fatalf("consume must return the pending key: %+v %v", ac, err)
	}
	if m.codes["h"].ac.EncryptedHLK != nil {
		t.Fatal("a used code must not keep the key")
	}

	_ = m.SaveCredential(ctx, "live", "u", []byte("a"))
	_ = m.SaveCredential(ctx, "orphan", "u", []byte("b"))
	_ = m.SaveRefreshToken(ctx, &RefreshToken{TokenHash: "rt", UserID: "u", CredentialID: "live", ExpiresAt: clk.Add(48 * time.Hour)})
	if _, err := m.GetCredential(ctx, "someone-else", "live"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another user must not read the credential, got %v", err)
	}
	_ = m.Cleanup(ctx)
	if _, err := m.GetCredential(ctx, "u", "orphan"); err != nil {
		t.Fatalf("a fresh credential must survive cleanup: %v", err)
	}
	clk = clk.Add(2 * time.Hour)
	_ = m.Cleanup(ctx)
	if _, err := m.GetCredential(ctx, "u", "orphan"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unreferenced credential must be purged, got %v", err)
	}
	if _, err := m.GetCredential(ctx, "u", "live"); err != nil {
		t.Fatalf("a credential with a refresh token must stay: %v", err)
	}
}
