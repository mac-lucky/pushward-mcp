package oauth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// flakyStore fails every read the token and MCP paths make, as a database
// outage or a pooler restart would.
type flakyStore struct {
	Store
	err error
}

func (f *flakyStore) GetCredential(context.Context, string, string) ([]byte, error) {
	return nil, f.err
}

func (f *flakyStore) GetRefreshToken(context.Context, string) (*RefreshToken, error) {
	return nil, f.err
}

func (f *flakyStore) ConsumeAuthCode(context.Context, string, string) (*AuthCode, error) {
	return nil, f.err
}

var errStoreDown = errors.New("connection refused")

func TestWrapMCP_StoreErrorIsRetryable(t *testing.T) {
	srv, p := newTestProvider(t)
	access, _ := connect(t, srv, "hlk_k1")
	p.store = &flakyStore{Store: p.store, err: errStoreDown}

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable || res.Header.Get("Retry-After") == "" {
		t.Fatalf("store outage on /mcp: %d Retry-After=%q, want 503 with Retry-After", res.StatusCode, res.Header.Get("Retry-After"))
	}
	if res.Header.Get("WWW-Authenticate") != "" {
		t.Fatal("a store outage must not challenge the client to re-authorize")
	}
}

func TestToken_StoreErrorIsServerError(t *testing.T) {
	srv, p := newTestProvider(t)
	clientID, verifier, redirect, code := obtainCode(t, srv, "hlk_k1")
	_, refresh := connect(t, srv, "hlk_k1")
	p.store = &flakyStore{Store: p.store, err: errStoreDown}

	status, m := tokenPost(t, srv, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
	if status != http.StatusInternalServerError || m["error"] != "server_error" {
		t.Fatalf("store outage on refresh: %d %v, want 500 server_error", status, m)
	}
	status, m = tokenPost(t, srv, url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect},
		"client_id": {clientID}, "code_verifier": {verifier},
	})
	if status != http.StatusInternalServerError || m["error"] != "server_error" {
		t.Fatalf("store outage on code exchange: %d %v, want 500 server_error", status, m)
	}
}

// credStoreDown fails only the credential read, so a refresh gets as far as
// the key check.
type credStoreDown struct{ Store }

func (credStoreDown) GetCredential(context.Context, string, string) ([]byte, error) {
	return nil, errStoreDown
}

func TestRefresh_CredentialStoreErrorKeepsGrant(t *testing.T) {
	srv, p := newTestProvider(t)
	_, refresh := connect(t, srv, "hlk_k1")
	store := p.store
	p.store = credStoreDown{store}
	status, m := tokenPost(t, srv, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
	if status != http.StatusInternalServerError || m["error"] != "server_error" {
		t.Fatalf("credential store outage on refresh: %d %v, want 500 server_error", status, m)
	}
	p.store = store
	_, _ = refreshGrant(t, srv, refresh) // fails the test unless the grant is still live
}

func TestRotateRefreshToken_MemoryWinsOnce(t *testing.T) {
	m := newMemoryStore()
	ctx := context.Background()
	_ = m.SaveRefreshToken(ctx, &RefreshToken{TokenHash: "old", UserID: "u", ExpiresAt: time.Now().Add(time.Hour)})
	won, err := m.RotateRefreshToken(ctx, "old", &RefreshToken{TokenHash: "a", UserID: "u", PrevHash: "old"})
	if err != nil || !won {
		t.Fatalf("first rotation must win: %v %v", won, err)
	}
	won, err = m.RotateRefreshToken(ctx, "old", &RefreshToken{TokenHash: "b", UserID: "u", PrevHash: "old"})
	if err != nil || won {
		t.Fatalf("second rotation must lose: %v %v", won, err)
	}
	if _, err := m.GetRefreshToken(ctx, "b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a lost rotation must save nothing, got %v", err)
	}
	if rt, err := m.GetRefreshToken(ctx, "a"); err != nil || rt.RevokedAt != nil {
		t.Fatalf("the winner's token must be live: %+v %v", rt, err)
	}
}
