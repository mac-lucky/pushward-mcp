package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
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
	refreshed, legacyRefresh := refreshGrant(t, srv, "rtok-legacy")
	if c, err := p.signer.Verify(refreshed, "https://mcp.test"); err != nil || c.CredentialID != "" {
		t.Fatalf("refreshing a legacy grant must keep it on the per-user row: %+v %v", c, err)
	}
	if got := mcpKey(t, srv, refreshed); got != "hlk_legacy" {
		t.Fatalf("refreshed legacy token sees %q, want hlk_legacy", got)
	}
	// An older release must still find the rotated token of a legacy grant,
	// and a legacy grant gets no family prefix.
	if strings.Contains(legacyRefresh, ".") {
		t.Fatalf("legacy refresh token %q must stay unprefixed", legacyRefresh)
	}
	if rt, err := m.GetRefreshToken(ctx, hashToken(legacyRefresh)); err != nil || rt.CredentialID != "" {
		t.Fatalf("older release cannot rotate a legacy grant: %+v %v", rt, err)
	}
}

// Releases before per-grant credentials look codes and refresh tokens up by
// hashToken. A pod on one, during a rolling update or after a rollback, must
// not find those of a key-bound grant: it would issue tokens without the
// credential id, and the grant would read the per-user key from then on.
func TestGrantKey_OlderReleaseCannotRedeemKeyBoundGrant(t *testing.T) {
	srv, p := newTestProvider(t)
	ctx := context.Background()
	m := p.store.(*memoryStore)

	clientID, verifier, redirect, code := obtainCode(t, srv, "hlk_k1")
	if _, err := m.ConsumeAuthCode(ctx, hashToken(code), ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("older release redeemed a code with a pending key: %v", err)
	}
	access, refresh := exchangeForTokens(t, srv, code, clientID, verifier, redirect)
	if got := mcpKey(t, srv, access); got != "hlk_k1" {
		t.Fatalf("client sees %q, want hlk_k1", got)
	}
	for range 2 {
		if _, err := m.GetRefreshToken(ctx, hashToken(refresh)); !errors.Is(err, ErrNotFound) {
			t.Fatalf("older release can rotate a key-bound grant: %v", err)
		}
		_, refresh = refreshGrant(t, srv, refresh)
	}

	// A code under the older hash (minted by an older pod) is not redeemed here.
	if err := m.SaveAuthCode(ctx, &AuthCode{
		CodeHash: hashToken("old-pod-code"), ClientID: clientID, UserID: "user-123", Scope: "mcp",
		RedirectURI: redirect, CodeChallenge: "x", Resource: "https://mcp.test", ExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	status, body := tokenPost(t, srv, url.Values{
		"grant_type": {"authorization_code"}, "code": {"old-pod-code"}, "redirect_uri": {redirect},
		"client_id": {clientID}, "code_verifier": {verifier},
	})
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("code without a pending key: %d %v, want invalid_grant", status, body)
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
		rt, err := p.store.GetRefreshToken(context.Background(), grantTokenHash(refresh))
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
	ac, err := m.ConsumeAuthCode(ctx, "h", "")
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

// keyUpstream is a fake /auth/me whose keys can be rolled and which can be
// made to fail as a whole, for the refresh-time key check.
type keyUpstream struct {
	dead   sync.Map     // "Bearer <key>" -> struct{}
	status atomic.Int32 // non-zero: every call answers this status
}

func (u *keyUpstream) serve(w http.ResponseWriter, r *http.Request) {
	if s := u.status.Load(); s != 0 {
		w.WriteHeader(int(s))
		return
	}
	if _, ok := u.dead.Load(r.Header.Get("Authorization")); ok {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"integration_key.expired"}`))
		return
	}
	fakeAuthMe(w, r)
}

func TestRefresh_DeadKeyEndsOnlyItsGrant(t *testing.T) {
	up := &keyUpstream{}
	srv, _ := newTestProviderUpstream(t, up.serve)
	_, refreshA := connect(t, srv, "hlk_k1")
	_, refreshB := connect(t, srv, "hlk_k0")
	_, refreshA = refreshGrant(t, srv, refreshA)

	up.dead.Store("Bearer hlk_k1", struct{}{})
	status, m := tokenPost(t, srv, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshA}})
	if status != http.StatusBadRequest || m["error"] != "invalid_grant" {
		t.Fatalf("refresh with a rolled key: %d %v, want invalid_grant", status, m)
	}
	// The grant is over, not just this one token.
	if status, _ := tokenPost(t, srv, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshA}}); status == http.StatusOK {
		t.Fatal("a grant ended for a dead key must stay ended")
	}
	// The user's other connection, with its own key, carries on.
	if access, _ := refreshGrant(t, srv, refreshB); mcpKey(t, srv, access) != "hlk_k0" {
		t.Fatal("ending one grant must not touch another grant of the user")
	}
}

func TestRefresh_UpstreamFailureKeepsGrant(t *testing.T) {
	up := &keyUpstream{}
	srv, _ := newTestProviderUpstream(t, up.serve)
	_, refresh := connect(t, srv, "hlk_k1")
	for _, s := range []int32{http.StatusServiceUnavailable, http.StatusTooManyRequests, http.StatusInternalServerError} {
		up.status.Store(s)
		_, refresh = refreshGrant(t, srv, refresh) // fails the test unless 200
	}
	up.status.Store(0)
	_, _ = refreshGrant(t, srv, refresh)
}

func TestRefresh_UnusableCredentialEndsGrant(t *testing.T) {
	for _, tc := range []struct {
		name    string
		breakIt func(m *memoryStore, credID string)
	}{
		{"missing", func(m *memoryStore, credID string) { delete(m.creds, credID) }},
		{"undecryptable", func(m *memoryStore, credID string) {
			c := m.creds[credID]
			c.blob = append([]byte(nil), c.blob...)
			c.blob[len(c.blob)-1] ^= 0xff
			m.creds[credID] = c
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, p := newTestProvider(t)
			access, refresh := connect(t, srv, "hlk_k1")
			c, err := p.signer.Verify(access, "https://mcp.test")
			if err != nil {
				t.Fatal(err)
			}
			m := p.store.(*memoryStore)
			m.mu.Lock()
			tc.breakIt(m, c.CredentialID)
			m.mu.Unlock()
			status, body := tokenPost(t, srv, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
			if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
				t.Fatalf("refresh with a %s credential: %d %v, want invalid_grant", tc.name, status, body)
			}
		})
	}
}

// A legacy chain shares the per-user row with the user's other legacy chains,
// so a dead key ends only the presented chain.
func TestRefresh_DeadLegacyKeyEndsPresentedTokenOnly(t *testing.T) {
	up := &keyUpstream{}
	srv, p := newTestProviderUpstream(t, up.serve)
	m := p.store.(*memoryStore)
	ctx := context.Background()
	blob, err := p.crypto.Encrypt("user-123", "hlk_legacy")
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.userCreds["user-123"] = blob
	m.mu.Unlock()
	for _, tok := range []string{"rtok-a", "rtok-b"} {
		if err := m.SaveRefreshToken(ctx, &RefreshToken{
			TokenHash: hashToken(tok), UserID: "user-123", ClientID: "c-old", Scope: "mcp",
			Resource: "https://mcp.test", ExpiresAt: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	up.dead.Store("Bearer hlk_legacy", struct{}{})
	if status, _ := tokenPost(t, srv, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"rtok-a"}}); status != http.StatusBadRequest {
		t.Fatalf("legacy refresh with a dead key: %d, want 400", status)
	}
	if rt, err := m.GetRefreshToken(ctx, hashToken("rtok-b")); err != nil || rt.RevokedAt != nil {
		t.Fatalf("another legacy chain must stay live: %+v %v", rt, err)
	}
	if _, err := m.GetCredential(ctx, "user-123", ""); err != nil {
		t.Fatalf("the per-user row must stay: %v", err)
	}
}
