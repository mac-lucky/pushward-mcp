package oauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"html"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/mac-lucky/pushward-mcp/internal/client"
)

func testKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
}

func testEncKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func TestCrypto_RoundTripAndUserBinding(t *testing.T) {
	c, err := newHLKCipher(testEncKey(t))
	if err != nil {
		t.Fatal(err)
	}
	blob, err := c.Encrypt("user-1", "hlk_secret")
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Decrypt("user-1", blob)
	if err != nil || got != "hlk_secret" {
		t.Fatalf("round trip failed: %q %v", got, err)
	}
	if _, err := c.Decrypt("user-2", blob); err == nil {
		t.Fatal("decrypting with the wrong user id must fail (AAD/key binding)")
	}
}

func TestCSRFTokenizer(t *testing.T) {
	base := time.Now()
	clk := base
	tok := newCSRFTokenizer([]byte("0123456789abcdef0123456789abcdef"), 10*time.Minute, func() time.Time { return clk })

	s := tok.issue("client-A")
	if !tok.verify(s, "client-A") {
		t.Fatal("a fresh token must verify for its client_id")
	}
	if tok.verify(s, "client-B") {
		t.Fatal("token must be bound to client_id (must not verify for another client)")
	}
	for _, bad := range []string{"", "no-dot", s + "x", "@@@.@@@"} {
		if tok.verify(bad, "client-A") {
			t.Fatalf("malformed/tampered token %q must not verify", bad)
		}
	}
	// A token issued by a tokenizer with a different key must not verify.
	other := newCSRFTokenizer([]byte("ffffffffffffffffffffffffffffffff"), 10*time.Minute, func() time.Time { return clk })
	if tok.verify(other.issue("client-A"), "client-A") {
		t.Fatal("token signed with a different key must not verify")
	}
	// Expiry.
	clk = base.Add(11 * time.Minute)
	if tok.verify(s, "client-A") {
		t.Fatal("an expired token must not verify")
	}
}

func TestPKCE_S256(t *testing.T) {
	verifier := "verifier-abc-123-xyz-long-enough-string"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	if !verifyPKCES256(verifier, challenge) {
		t.Fatal("valid PKCE pair should verify")
	}
	if verifyPKCES256("wrong", challenge) {
		t.Fatal("wrong verifier must not verify")
	}
}

func TestSigner_SignVerifyAndAudience(t *testing.T) {
	sg, err := newSigner(testKeyPEM(t), "https://mcp.test")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := sg.Sign("user-9", "https://mcp.test", "client-1", "mcp", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	claims, err := sg.Verify(tok, "https://mcp.test")
	if err != nil || claims.Subject != "user-9" {
		t.Fatalf("verify failed: %v sub=%v", err, claims)
	}
	if _, err := sg.Verify(tok, "https://other.test"); err == nil {
		t.Fatal("verification must reject a wrong audience")
	}
	var jwks map[string]any
	if err := json.Unmarshal(sg.JWKS(), &jwks); err != nil {
		t.Fatalf("jwks not valid json: %v", err)
	}
}

func TestRedirectURIAllowed_Loopback(t *testing.T) {
	reg := []string{"https://claude.ai/api/mcp/auth_callback", "http://localhost/callback"}
	if !redirectURIAllowed(reg, "https://claude.ai/api/mcp/auth_callback") {
		t.Fatal("exact https match should pass")
	}
	if !redirectURIAllowed(reg, "http://localhost:54321/callback") {
		t.Fatal("loopback port should be ignored")
	}
	if redirectURIAllowed(reg, "https://evil.example/callback") {
		t.Fatal("unregistered uri must be rejected")
	}
	if redirectURIAllowed(reg, "http://localhost:1/other") {
		t.Fatal("loopback path must still match exactly")
	}
}

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{"127.0.0.1", "10.0.0.1", "192.168.1.1", "172.16.5.5", "169.254.169.254", "::1", "fc00::1", "0.0.0.0"}
	for _, s := range blocked {
		if !isBlockedIP(net.ParseIP(s)) {
			t.Fatalf("%s should be blocked (SSRF)", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "93.184.216.34"} {
		if isBlockedIP(net.ParseIP(s)) {
			t.Fatalf("%s should be allowed", s)
		}
	}
}

// fullFlowDeps builds a Provider wired to a fake upstream /auth/me and returns
// an httptest server mounting the OAuth routes plus a /mcp endpoint guarded by
// WrapMCP whose inner handler echoes the per-request token + user id it sees.
func newTestProvider(t *testing.T) (*httptest.Server, *Provider) {
	t.Helper()
	client.SetRemoteMode(true)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer hlk_good" {
			_, _ = w.Write([]byte(`{"id":"user-123"}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"unauthorized"}`))
	}))
	t.Cleanup(upstream.Close)

	cfg := &Config{
		Issuer:        "https://mcp.test",
		Resource:      "https://mcp.test",
		APIBaseURL:    upstream.URL,
		SigningKeyPEM: testKeyPEM(t),
		HLKEncKey:     testEncKey(t),
	}
	p, err := New(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close) // stop the janitor goroutine

	mux := http.NewServeMux()
	p.RegisterRoutes(mux)
	mux.Handle("/mcp", p.WrapMCP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token": client.TokenFromContext(r.Context()),
			"user":  client.UserIDFromContext(r.Context()),
		})
	})))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, p
}

func TestOAuthFullFlow(t *testing.T) {
	srv, _ := newTestProvider(t)
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	// AS metadata advertises the CIMD-selecting capabilities.
	res, _ := http.Get(srv.URL + "/.well-known/oauth-authorization-server")
	var meta map[string]any
	_ = json.NewDecoder(res.Body).Decode(&meta)
	res.Body.Close()
	if meta["client_id_metadata_document_supported"] != true {
		t.Fatal("AS metadata must advertise CIMD support")
	}

	// DCR.
	body, _ := json.Marshal(map[string]any{"redirect_uris": []string{"https://client.test/cb"}, "client_name": "Test"})
	res, _ = http.Post(srv.URL+"/oauth/register", "application/json", strings.NewReader(string(body)))
	var reg map[string]any
	_ = json.NewDecoder(res.Body).Decode(&reg)
	res.Body.Close()
	clientID, _ := reg["client_id"].(string)
	if clientID == "" {
		t.Fatal("DCR did not return a client_id")
	}

	// Authorize (GET) → consent page with a stateless CSRF token.
	verifier := "this-is-a-sufficiently-long-pkce-code-verifier-value"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {"https://client.test/cb"},
		"state":                 {"st-1"},
		"scope":                 {"mcp"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"resource":              {"https://mcp.test"},
	}
	csrf := authzGetCSRF(t, srv, q.Encode())

	// Authorize (POST) with the key → redirect with code.
	form := url.Values{}
	for k, v := range q {
		form.Set(k, v[0])
	}
	form.Set("api_key", "hlk_good")
	form.Set(csrfFormName, csrf)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound {
		t.Fatalf("authorize POST status = %d, want 302", res.StatusCode)
	}
	loc, _ := url.Parse(res.Header.Get("Location"))
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("no code in redirect: %s", res.Header.Get("Location"))
	}
	if loc.Query().Get("iss") != "https://mcp.test" {
		t.Fatal("redirect must include iss")
	}

	// Token exchange.
	tokForm := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {"https://client.test/cb"},
		"client_id":     {clientID},
		"code_verifier": {verifier},
		"resource":      {"https://mcp.test"},
	}
	res, _ = http.PostForm(srv.URL+"/oauth/token", tokForm)
	var tok map[string]any
	_ = json.NewDecoder(res.Body).Decode(&tok)
	res.Body.Close()
	access, _ := tok["access_token"].(string)
	refresh, _ := tok["refresh_token"].(string)
	if access == "" || refresh == "" {
		t.Fatalf("token response missing tokens: %v", tok)
	}

	// Code reuse must fail.
	res, _ = http.PostForm(srv.URL+"/oauth/token", tokForm)
	if res.StatusCode == http.StatusOK {
		t.Fatal("authorization code reuse must be rejected")
	}
	res.Body.Close()

	// Call /mcp with the access token → inner handler sees the decrypted hlk.
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	res, _ = http.DefaultClient.Do(req)
	var seen map[string]string
	_ = json.NewDecoder(res.Body).Decode(&seen)
	res.Body.Close()
	if seen["token"] != "hlk_good" {
		t.Fatalf("WrapMCP did not inject the stored hlk_: %v", seen)
	}
	if seen["user"] != "user-123" {
		t.Fatalf("WrapMCP did not inject the user id: %v", seen)
	}

	// /mcp without a token → 401 + WWW-Authenticate.
	res, _ = http.Get(srv.URL + "/mcp")
	if res.StatusCode != http.StatusUnauthorized || res.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("unauthenticated /mcp must 401 with challenge, got %d %q", res.StatusCode, res.Header.Get("WWW-Authenticate"))
	}
	res.Body.Close()

	// Refresh rotation: old refresh works once, then is revoked.
	rf := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}}
	res, _ = http.PostForm(srv.URL+"/oauth/token", rf)
	var tok2 map[string]any
	_ = json.NewDecoder(res.Body).Decode(&tok2)
	res.Body.Close()
	if tok2["access_token"] == nil {
		t.Fatalf("refresh did not return a new access token: %v", tok2)
	}
	// Reusing the now-rotated refresh token must fail.
	res, _ = http.PostForm(srv.URL+"/oauth/token", rf)
	if res.StatusCode == http.StatusOK {
		t.Fatal("reuse of a rotated refresh token must be rejected")
	}
	res.Body.Close()
}

func TestAuthorize_RejectsBadKey(t *testing.T) {
	srv, _ := newTestProvider(t)
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	body, _ := json.Marshal(map[string]any{"redirect_uris": []string{"https://client.test/cb"}})
	res, _ := http.Post(srv.URL+"/oauth/register", "application/json", strings.NewReader(string(body)))
	var reg map[string]any
	_ = json.NewDecoder(res.Body).Decode(&reg)
	res.Body.Close()
	clientID := reg["client_id"].(string)

	verifier := "another-sufficiently-long-pkce-code-verifier-value"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	q := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {"https://client.test/cb"},
		"scope": {"mcp"}, "code_challenge": {challenge}, "code_challenge_method": {"S256"},
		"resource": {"https://mcp.test"},
	}
	csrf := authzGetCSRF(t, srv, q.Encode())
	form := url.Values{}
	for k, v := range q {
		form.Set(k, v[0])
	}
	form.Set("api_key", "hlk_bad")
	form.Set(csrfFormName, csrf)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusFound {
		t.Fatal("a bad API key must not produce an authorization code")
	}
}

func TestAuthorize_MissingPKCERejected(t *testing.T) {
	srv, _ := newTestProvider(t)
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	body, _ := json.Marshal(map[string]any{"redirect_uris": []string{"https://client.test/cb"}})
	res, _ := http.Post(srv.URL+"/oauth/register", "application/json", strings.NewReader(string(body)))
	var reg map[string]any
	_ = json.NewDecoder(res.Body).Decode(&reg)
	res.Body.Close()
	clientID := reg["client_id"].(string)

	q := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {"https://client.test/cb"},
		"scope": {"mcp"}, "resource": {"https://mcp.test"}, // no code_challenge
	}
	res, err := noRedirect.Get(srv.URL + "/oauth/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusFound {
		t.Fatalf("expected redirect-with-error, got %d", res.StatusCode)
	}
	loc, _ := url.Parse(res.Header.Get("Location"))
	if loc.Query().Get("error") == "" {
		t.Fatal("missing PKCE should redirect back with an error")
	}
}

// hiddenInputRE extracts the name/value of each hidden field the consent
// template renders, so a test can submit exactly what a browser would.
var hiddenInputRE = regexp.MustCompile(`<input type="hidden" name="([^"]*)" value="([^"]*)">`)

// authzGetCSRF runs the authorize GET and returns the stateless CSRF token the
// consent page embedded in its hidden field. There is no cookie — the token is
// self-contained — so this is the only thing a submitter needs to carry back.
func authzGetCSRF(t *testing.T, srv *httptest.Server, rawQuery string) string {
	t.Helper()
	res, err := http.Get(srv.URL + "/oauth/authorize?" + rawQuery)
	if err != nil {
		t.Fatal(err)
	}
	bodyBytes, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("authorize GET status = %d", res.StatusCode)
	}
	for _, m := range hiddenInputRE.FindAllStringSubmatch(string(bodyBytes), -1) {
		if m[1] == csrfFormName {
			return html.UnescapeString(m[2])
		}
	}
	t.Fatal("consent page has no csrf hidden field")
	return ""
}

// TestConsentFormRoundTripsOAuthParams renders the consent page (GET) and then
// POSTs back ONLY the hidden fields the rendered HTML actually contains — exactly
// as a browser does. The other authorize tests rebuild the POST body from the
// original query, so they silently re-add any field the form drops; this one
// would not. A missing hidden field (e.g. response_type) makes the POST fail
// validation and redirect back with an OAuth error even though the GET rendered
// fine. Regression test for the dropped-response_type bug.
func TestConsentFormRoundTripsOAuthParams(t *testing.T) {
	srv, _ := newTestProvider(t)
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	body, _ := json.Marshal(map[string]any{"redirect_uris": []string{"https://client.test/cb"}, "client_name": "Test"})
	res, _ := http.Post(srv.URL+"/oauth/register", "application/json", strings.NewReader(string(body)))
	var reg map[string]any
	_ = json.NewDecoder(res.Body).Decode(&reg)
	res.Body.Close()
	clientID := reg["client_id"].(string)

	verifier := "verifier-" + strings.Repeat("x", 40)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	q := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {"https://client.test/cb"},
		"state": {"st"}, "scope": {"mcp"}, "code_challenge": {challenge},
		"code_challenge_method": {"S256"}, "resource": {"https://mcp.test"},
	}
	res, _ = http.Get(srv.URL + "/oauth/authorize?" + q.Encode())
	htmlBody, _ := io.ReadAll(res.Body)
	res.Body.Close()

	// Reconstruct the POST body from the rendered form's hidden inputs ONLY — no
	// cookie. The CSRF token now lives entirely in a hidden field, so submitting
	// exactly what the form contains (plus the key) must succeed.
	form := url.Values{}
	for _, m := range hiddenInputRE.FindAllStringSubmatch(string(htmlBody), -1) {
		form.Set(m[1], html.UnescapeString(m[2]))
	}
	if form.Get("response_type") != "code" {
		t.Fatalf("consent form must carry response_type=code as a hidden field; got %q", form.Get("response_type"))
	}
	if form.Get(csrfFormName) == "" {
		t.Fatal("consent form must carry a csrf token as a hidden field")
	}
	form.Set("api_key", "hlk_good")

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusFound {
		t.Fatalf("submitting the rendered consent form should redirect with a code, got %d", res.StatusCode)
	}
	loc, _ := url.Parse(res.Header.Get("Location"))
	if loc.Query().Get("error") != "" || loc.Query().Get("code") == "" {
		t.Fatalf("expected an authorization code, got error=%q", loc.Query().Get("error"))
	}
}

// TestAuthorize_StaleRenderStillValid reproduces the exact production bug: a
// second authorize render (another tab, a reload, or the OAuth client prefetching
// /oauth/authorize while also opening it) must NOT invalidate an earlier render's
// token. The old single fixed-name cookie was clobbered by render 2, so render 1's
// form no longer matched. With a stateless token both renders verify
// independently, so submitting the FIRST render's token still mints a code.
func TestAuthorize_StaleRenderStillValid(t *testing.T) {
	srv, _ := newTestProvider(t)
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	body, _ := json.Marshal(map[string]any{"redirect_uris": []string{"https://client.test/cb"}})
	res, _ := http.Post(srv.URL+"/oauth/register", "application/json", strings.NewReader(string(body)))
	var reg map[string]any
	_ = json.NewDecoder(res.Body).Decode(&reg)
	res.Body.Close()
	clientID := reg["client_id"].(string)
	sum := sha256.Sum256([]byte("verifier-" + strings.Repeat("z", 40)))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	q := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {"https://client.test/cb"},
		"state": {"st"}, "scope": {"mcp"}, "code_challenge": {challenge},
		"code_challenge_method": {"S256"}, "resource": {"https://mcp.test"},
	}
	first := authzGetCSRF(t, srv, q.Encode()) // render 1
	_ = authzGetCSRF(t, srv, q.Encode())      // render 2 — would clobber a shared cookie

	form := url.Values{}
	for k, v := range q {
		form.Set(k, v[0])
	}
	form.Set("api_key", "hlk_good")
	form.Set(csrfFormName, first) // submit the FIRST render's token
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusFound {
		t.Fatalf("submitting an earlier render's token must still mint a code, got %d", res.StatusCode)
	}
	if loc, _ := url.Parse(res.Header.Get("Location")); loc.Query().Get("code") == "" {
		t.Fatalf("expected a code; got error=%q", loc.Query().Get("error"))
	}
}

// ---------------------------------------------------------------------------
// Negative-path coverage: the checks below each guard a security property that
// the happy-path flow would NOT catch if it were deleted.
// ---------------------------------------------------------------------------

// fakeClock is a mutex-guarded controllable time source for token-expiry and
// refresh-grace tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) advance(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.t = c.t.Add(d) }

// obtainCode runs register → authorize(GET) → authorize(POST) with apiKey and
// returns the issued code plus the bound client_id/verifier/redirect_uri.
func obtainCode(t *testing.T, srv *httptest.Server, apiKey string) (clientID, verifier, redirect, code string) {
	t.Helper()
	redirect = "https://client.test/cb"
	body, _ := json.Marshal(map[string]any{"redirect_uris": []string{redirect}, "client_name": "Test"})
	res, _ := http.Post(srv.URL+"/oauth/register", "application/json", strings.NewReader(string(body)))
	var reg map[string]any
	_ = json.NewDecoder(res.Body).Decode(&reg)
	res.Body.Close()
	clientID, _ = reg["client_id"].(string)

	verifier = "verifier-" + strings.Repeat("x", 40)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	q := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect},
		"state": {"st"}, "scope": {"mcp"}, "code_challenge": {challenge},
		"code_challenge_method": {"S256"}, "resource": {"https://mcp.test"},
	}
	csrf := authzGetCSRF(t, srv, q.Encode())
	form := url.Values{}
	for k, v := range q {
		form.Set(k, v[0])
	}
	form.Set("api_key", apiKey)
	form.Set(csrfFormName, csrf)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	loc, _ := url.Parse(res.Header.Get("Location"))
	res.Body.Close()
	code = loc.Query().Get("code")
	if code == "" {
		t.Fatalf("no code issued: %s", res.Header.Get("Location"))
	}
	return
}

func tokenPost(t *testing.T, srv *httptest.Server, form url.Values) (int, map[string]any) {
	t.Helper()
	res, err := http.PostForm(srv.URL+"/oauth/token", form)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(res.Body).Decode(&m)
	return res.StatusCode, m
}

func exchangeForTokens(t *testing.T, srv *httptest.Server, code, clientID, verifier, redirect string) (string, string) {
	t.Helper()
	status, m := tokenPost(t, srv, url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect},
		"client_id": {clientID}, "code_verifier": {verifier}, "resource": {"https://mcp.test"},
	})
	if status != http.StatusOK {
		t.Fatalf("token exchange failed: %d %v", status, m)
	}
	access, _ := m["access_token"].(string)
	refresh, _ := m["refresh_token"].(string)
	if access == "" || refresh == "" {
		t.Fatalf("missing tokens: %v", m)
	}
	return access, refresh
}

func TestSigner_RejectsAlgConfusionTamperAndExpiry(t *testing.T) {
	sg, err := newSigner(testKeyPEM(t), "https://mcp.test")
	if err != nil {
		t.Fatal(err)
	}
	// Algorithm confusion: an HS256 token MAC'd with the server's public key must
	// be rejected by the ES256 method pin (WithValidMethods + type assertion).
	pubDER, err := x509.MarshalPKIXPublicKey(&sg.priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	forged := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Issuer:    "https://mcp.test",
		Subject:   "attacker",
		Audience:  jwt.ClaimStrings{"https://mcp.test"},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	})
	forgedStr, err := forged.SignedString(pubDER)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sg.Verify(forgedStr, "https://mcp.test"); err == nil {
		t.Fatal("HS256 algorithm-confusion token must be rejected")
	}
	// Expired token.
	expired, _ := sg.Sign("u", "https://mcp.test", "c", "mcp", time.Now().Add(-2*time.Hour))
	if _, err := sg.Verify(expired, "https://mcp.test"); err == nil {
		t.Fatal("expired token must be rejected")
	}
	// Tampered signature.
	good, _ := sg.Sign("u", "https://mcp.test", "c", "mcp", time.Now())
	if _, err := sg.Verify(good[:len(good)-2]+"xy", "https://mcp.test"); err == nil {
		t.Fatal("tampered token must be rejected")
	}
	// Wrong issuer.
	other, _ := newSigner(testKeyPEM(t), "https://evil.test")
	wrongIss, _ := other.Sign("u", "https://mcp.test", "c", "mcp", time.Now())
	if _, err := sg.Verify(wrongIss, "https://mcp.test"); err == nil {
		t.Fatal("wrong-issuer token must be rejected")
	}
}

func TestToken_BindingRejections(t *testing.T) {
	srv, _ := newTestProvider(t)

	t.Run("wrong verifier", func(t *testing.T) {
		clientID, _, redirect, code := obtainCode(t, srv, "hlk_good")
		status, m := tokenPost(t, srv, url.Values{
			"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect},
			"client_id": {clientID}, "code_verifier": {"completely-wrong-verifier-value-zzz"}, "resource": {"https://mcp.test"},
		})
		if status != http.StatusBadRequest || m["error"] != "invalid_grant" {
			t.Fatalf("wrong PKCE verifier must be invalid_grant, got %d %v", status, m)
		}
	})
	t.Run("wrong client_id", func(t *testing.T) {
		clientID, verifier, redirect, code := obtainCode(t, srv, "hlk_good")
		_ = clientID
		status, m := tokenPost(t, srv, url.Values{
			"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect},
			"client_id": {"dcr_someone_else"}, "code_verifier": {verifier}, "resource": {"https://mcp.test"},
		})
		if status != http.StatusBadRequest || m["error"] != "invalid_grant" {
			t.Fatalf("mismatched client_id must be invalid_grant, got %d %v", status, m)
		}
	})
	t.Run("wrong redirect_uri", func(t *testing.T) {
		clientID, verifier, _, code := obtainCode(t, srv, "hlk_good")
		status, m := tokenPost(t, srv, url.Values{
			"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {"https://client.test/other"},
			"client_id": {clientID}, "code_verifier": {verifier}, "resource": {"https://mcp.test"},
		})
		if status != http.StatusBadRequest || m["error"] != "invalid_grant" {
			t.Fatalf("mismatched redirect_uri must be invalid_grant, got %d %v", status, m)
		}
	})
	t.Run("missing client_id", func(t *testing.T) {
		_, verifier, redirect, code := obtainCode(t, srv, "hlk_good")
		status, m := tokenPost(t, srv, url.Values{
			"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect},
			"code_verifier": {verifier}, "resource": {"https://mcp.test"},
		})
		if status != http.StatusBadRequest || m["error"] != "invalid_request" {
			t.Fatalf("missing client_id must be invalid_request, got %d %v", status, m)
		}
	})
}

func TestRefresh_ExpiredRejected(t *testing.T) {
	srv, p := newTestProvider(t)
	if err := p.store.SaveRefreshToken(context.Background(), &RefreshToken{
		TokenHash: hashToken("rtok-expired"), UserID: "u", ClientID: "c", Scope: "mcp",
		Resource: "https://mcp.test", ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	status, m := tokenPost(t, srv, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"rtok-expired"}})
	if status != http.StatusBadRequest || m["error"] != "invalid_grant" {
		t.Fatalf("expired refresh token must be invalid_grant, got %d %v", status, m)
	}
}

func TestRefresh_ReuseAfterGraceRevokesFamily(t *testing.T) {
	srv, p := newTestProvider(t)
	clk := &fakeClock{t: time.Now()}
	p.setClock(clk.now)

	clientID, verifier, redirect, code := obtainCode(t, srv, "hlk_good")
	_, refresh1 := exchangeForTokens(t, srv, code, clientID, verifier, redirect)

	// Rotate refresh1 → refresh2.
	status, m := tokenPost(t, srv, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh1}})
	if status != http.StatusOK {
		t.Fatalf("first rotation should succeed, got %d %v", status, m)
	}
	refresh2, _ := m["refresh_token"].(string)
	if refresh2 == "" {
		t.Fatal("rotation did not return a new refresh token")
	}

	// Beyond the grace window, replaying the rotated refresh1 is treated as theft
	// and must revoke the whole family (so refresh2 also dies).
	clk.advance(refreshGrace + time.Second)
	if status, _ := tokenPost(t, srv, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh1}}); status == http.StatusOK {
		t.Fatal("reuse of a rotated refresh token must be rejected")
	}
	if status, _ := tokenPost(t, srv, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh2}}); status == http.StatusOK {
		t.Fatal("the whole refresh-token family must be revoked after reuse-after-grace")
	}
}

// TestAuthorize_CSRFInvalidReRenders verifies that an invalid/expired CSRF token
// on POST does NOT mint an authorization code and does NOT dead-end: it re-renders
// the consent page (no redirect) carrying a fresh token so the user can recover in
// one click. The security property — no code without a valid token — is preserved
// (the response is the consent form, never a Location redirect carrying a code).
func TestAuthorize_CSRFInvalidReRenders(t *testing.T) {
	srv, _ := newTestProvider(t)
	body, _ := json.Marshal(map[string]any{"redirect_uris": []string{"https://client.test/cb"}})
	res, _ := http.Post(srv.URL+"/oauth/register", "application/json", strings.NewReader(string(body)))
	var reg map[string]any
	_ = json.NewDecoder(res.Body).Decode(&reg)
	res.Body.Close()
	clientID := reg["client_id"].(string)

	verifier := "verifier-" + strings.Repeat("y", 40)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	q := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {"https://client.test/cb"},
		"scope": {"mcp"}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}, "resource": {"https://mcp.test"},
	}
	// Render once (just to exercise the GET path), then POST a bogus CSRF token.
	_ = authzGetCSRF(t, srv, q.Encode())

	form := url.Values{}
	for k, v := range q {
		form.Set(k, v[0])
	}
	form.Set("api_key", "hlk_good")
	form.Set(csrfFormName, "not-a-valid-token") // forged / invalid
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	// Must NOT redirect (a redirect would mean a code was issued).
	if res.StatusCode == http.StatusFound {
		t.Fatalf("invalid CSRF must never mint a code; got redirect to %q", res.Header.Get("Location"))
	}
	// Must re-render the consent page (with a fresh, valid token) so the user can retry.
	page, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(page), `name="api_key"`) {
		t.Fatal("invalid CSRF must re-render the consent form, not an error page")
	}
	if csrf := csrfFromHTMLString(string(page)); csrf == "" {
		t.Fatal("re-render must embed a fresh csrf token")
	}
}

// csrfFromHTMLString returns the csrf hidden-field value in a rendered consent
// page, or "" if absent.
func csrfFromHTMLString(s string) string {
	for _, m := range hiddenInputRE.FindAllStringSubmatch(s, -1) {
		if m[1] == csrfFormName {
			return html.UnescapeString(m[2])
		}
	}
	return ""
}

func TestConsumeAuthCode_MemoryContract(t *testing.T) {
	m := newMemoryStore()
	ctx := context.Background()
	if err := m.SaveAuthCode(ctx, &AuthCode{CodeHash: "h1", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ConsumeAuthCode(ctx, "h1"); err != nil {
		t.Fatalf("first consume must succeed: %v", err)
	}
	if _, err := m.ConsumeAuthCode(ctx, "h1"); !errors.Is(err, ErrCodeAlreadyUsed) {
		t.Fatalf("replay must be ErrCodeAlreadyUsed, got %v", err)
	}
	if _, err := m.ConsumeAuthCode(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent code must be ErrNotFound, got %v", err)
	}
}

func TestRevokeRefreshToken_WinsOnce(t *testing.T) {
	m := newMemoryStore()
	ctx := context.Background()
	_ = m.SaveRefreshToken(ctx, &RefreshToken{TokenHash: "rh", UserID: "u", ExpiresAt: time.Now().Add(time.Hour)})
	won, err := m.RevokeRefreshToken(ctx, "rh")
	if err != nil || !won {
		t.Fatalf("first revoke must win: won=%v err=%v", won, err)
	}
	won, err = m.RevokeRefreshToken(ctx, "rh")
	if err != nil || won {
		t.Fatalf("second revoke must lose (already revoked): won=%v err=%v", won, err)
	}
}

func TestCrypto_RejectsTamperAndShort(t *testing.T) {
	c, err := newHLKCipher(testEncKey(t))
	if err != nil {
		t.Fatal(err)
	}
	blob, err := c.Encrypt("u", "hlk_secret")
	if err != nil {
		t.Fatal(err)
	}
	tampered := make([]byte, len(blob))
	copy(tampered, blob)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := c.Decrypt("u", tampered); err == nil {
		t.Fatal("a tampered ciphertext must fail GCM authentication")
	}
	if _, err := c.Decrypt("u", []byte{1, 2, 3}); err == nil {
		t.Fatal("a ciphertext shorter than the nonce must error, not panic")
	}
}

func TestRedirectURIAllowed_NearLoopbackRejected(t *testing.T) {
	reg := []string{"http://localhost/cb"}
	for _, bad := range []string{
		"http://localhost.evil.com/cb",
		"http://127.0.0.1.evil.com/cb",
		"http://127.0.0.1@evil.com/cb",
		"https://localhost/cb", // scheme must still match
		"http://localhost/other",
	} {
		if redirectURIAllowed(reg, bad) {
			t.Fatalf("%q must be rejected against %v", bad, reg)
		}
	}
}

func TestIsBlockedIP_CGNAT(t *testing.T) {
	for _, s := range []string{"100.64.0.1", "100.100.50.50", "100.127.255.255", "198.18.0.1", "192.0.2.5"} {
		if !isBlockedIP(net.ParseIP(s)) {
			t.Fatalf("%s should be blocked (SSRF)", s)
		}
	}
	// 100.128.0.0 is just outside the CGNAT /10 and must remain reachable.
	if isBlockedIP(net.ParseIP("100.128.0.1")) {
		t.Fatal("100.128.0.1 is a public address and must be allowed")
	}
}

// TestReadPasswordFile covers the file-sourced DB password path: the trailing
// newline that secret tooling appends is trimmed, and a missing mount errors
// (so newPostgresStore fails fast rather than connecting password-less).
func TestReadPasswordFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "password")
	if err := os.WriteFile(path, []byte("s3cr3t-pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readPasswordFile(path)
	if err != nil {
		t.Fatalf("readPasswordFile: %v", err)
	}
	if got != "s3cr3t-pw" {
		t.Fatalf("got %q, want trailing newline trimmed", got)
	}
	if _, err := readPasswordFile(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("expected error for a missing password file")
	}
}
