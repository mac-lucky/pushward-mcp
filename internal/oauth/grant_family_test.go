package oauth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// refreshRefused reports whether refreshing with token is refused.
func refreshRefused(t *testing.T, srvURL, token string) bool {
	t.Helper()
	res, err := http.PostForm(srvURL+"/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {token}})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode != http.StatusOK
}

func TestRefreshReplay_RevokesOnlyThatGrant(t *testing.T) {
	srv, p := newTestProvider(t)
	clk := &fakeClock{t: time.Now()}
	p.setClock(clk.now)
	_, a1 := connect(t, srv, "hlk_k1")
	_, b1 := connect(t, srv, "hlk_k0")
	_, a2 := refreshGrant(t, srv, a1)

	clk.advance(refreshGrace + time.Second)
	if !refreshRefused(t, srv.URL, a1) {
		t.Fatal("a replayed refresh token must be refused")
	}
	if !refreshRefused(t, srv.URL, a2) {
		t.Fatal("a replay must revoke the rest of its grant")
	}
	if _, b2 := refreshGrant(t, srv, b1); b2 == "" {
		t.Fatal("another grant of the user must survive the replay")
	}
}

func TestRefreshReplay_AfterPurgeRevokesFamily(t *testing.T) {
	srv, p := newTestProvider(t)
	clk := &fakeClock{t: time.Now()}
	p.setClock(clk.now)
	m := p.store.(*memoryStore)
	ctx := context.Background()
	_, a1 := connect(t, srv, "hlk_k1")
	_, b1 := connect(t, srv, "hlk_k0")
	_, a2 := refreshGrant(t, srv, a1)

	clk.advance(2 * time.Hour)
	_ = m.Cleanup(ctx)
	if _, err := m.GetRefreshToken(ctx, grantTokenHash(a1)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the rotated token's row should be purged by now: %v", err)
	}
	if !refreshRefused(t, srv.URL, a1) {
		t.Fatal("a purged refresh token must be refused")
	}
	if !refreshRefused(t, srv.URL, a2) {
		t.Fatal("a replay past the purge must still revoke its grant")
	}
	if _, b2 := refreshGrant(t, srv, b1); b2 == "" {
		t.Fatal("another grant of the user must survive the replay")
	}
}

// A legacy chain has no grant id, so its replay ends every legacy chain of the
// user, and none of the user's key-bound grants, whose keys it never held.
func TestRefreshReplay_LegacyEndsLegacyChainsOnly(t *testing.T) {
	srv, p := newTestProvider(t)
	clk := &fakeClock{t: time.Now()}
	p.setClock(clk.now)
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
			Resource: "https://mcp.test", ExpiresAt: clk.now().Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	_, bound := connect(t, srv, "hlk_k1")
	_, a2 := refreshGrant(t, srv, "rtok-a")

	clk.advance(refreshGrace + time.Second)
	if !refreshRefused(t, srv.URL, "rtok-a") {
		t.Fatal("a replayed legacy refresh token must be refused")
	}
	if !refreshRefused(t, srv.URL, a2) {
		t.Fatal("a legacy replay must revoke the rest of its chain")
	}
	if !refreshRefused(t, srv.URL, "rtok-b") {
		t.Fatal("a legacy replay must revoke the user's other legacy chains")
	}
	if _, next := refreshGrant(t, srv, bound); next == "" {
		t.Fatal("a legacy replay must not end a key-bound grant of the user")
	}
}

func TestCodeReplay_RevokesOnlyThatGrant(t *testing.T) {
	srv, _ := newTestProvider(t)
	clientID, verifier, redirect, code := obtainCode(t, srv, "hlk_k1")
	_, a1 := exchangeForTokens(t, srv, code, clientID, verifier, redirect)
	_, b1 := connect(t, srv, "hlk_k0")

	status, _ := tokenPost(t, srv, url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect},
		"client_id": {clientID}, "code_verifier": {verifier},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("code replay: %d, want 400", status)
	}
	if !refreshRefused(t, srv.URL, a1) {
		t.Fatal("a code replay must revoke the grant minted from it")
	}
	if _, b2 := refreshGrant(t, srv, b1); b2 == "" {
		t.Fatal("another grant of the user must survive a code replay")
	}
}

// The family prefix is not the credential id an access token shows, so an
// access token alone cannot be turned into a grant-killing refresh token.
func TestRefreshFamily_NotDerivableFromAccessToken(t *testing.T) {
	srv, p := newTestProvider(t)
	access, refresh := connect(t, srv, "hlk_k1")
	claims, err := p.signer.Verify(access, "https://mcp.test")
	if err != nil {
		t.Fatal(err)
	}
	family, _, ok := strings.Cut(refresh, ".")
	if !ok || family == "" || family == claims.CredentialID {
		t.Fatalf("refresh token %q must carry a family prefix other than the credential id", refresh)
	}
	if !refreshRefused(t, srv.URL, claims.CredentialID+".forged") {
		t.Fatal("a forged refresh token must be refused")
	}
	if _, next := refreshGrant(t, srv, refresh); next == "" {
		t.Fatal("a token forged from the credential id must not end the grant")
	}
}

// A key-bound grant issued before families carries a bare token; its next
// rotation moves it onto a family.
func TestRefreshFamily_AssignedOnNextRotation(t *testing.T) {
	srv, p := newTestProvider(t)
	m := p.store.(*memoryStore)
	ctx := context.Background()
	blob, err := p.crypto.Encrypt("user-123", "hlk_k1")
	if err != nil {
		t.Fatal(err)
	}
	_ = m.SaveCredential(ctx, "cred-old", "user-123", blob)
	_ = m.SaveRefreshToken(ctx, &RefreshToken{
		TokenHash: grantTokenHash("rtok-nofamily"), UserID: "user-123", ClientID: "c", Scope: "mcp",
		Resource: "https://mcp.test", CredentialID: "cred-old", ExpiresAt: time.Now().Add(time.Hour),
	})
	_, next := refreshGrant(t, srv, "rtok-nofamily")
	rt, err := m.GetRefreshToken(ctx, grantTokenHash(next))
	if err != nil || rt.Family == "" || !strings.HasPrefix(next, rt.Family+".") {
		t.Fatalf("rotated token %q must carry its row's family: %+v %v", next, rt, err)
	}
	if _, again := refreshGrant(t, srv, next); !strings.HasPrefix(again, rt.Family+".") {
		t.Fatalf("the family must stay the same across rotations: %q", again)
	}
}
