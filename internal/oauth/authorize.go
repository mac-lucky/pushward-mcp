package oauth

import (
	"net/http"
	"net/url"
	"strings"
)

const (
	scopeMCP     = "mcp"
	csrfCookie   = "pw_mcp_csrf"
	csrfFormName = "csrf"

	// OAuth protocol token values, single-sourced so the request handlers and the
	// advertised discovery metadata (metadata.go) cannot drift.
	responseTypeCode           = "code"
	pkceMethodS256             = "S256"
	grantTypeAuthorizationCode = "authorization_code"
	grantTypeRefreshToken      = "refresh_token"
	authMethodNone             = "none"

	// maxFormBytes caps the urlencoded body on the authorize/token POSTs.
	maxFormBytes = 16 << 10
)

// authzParams holds the OAuth authorization request parameters shared by the
// GET (consent render) and POST (consent submit) handlers.
type authzParams struct {
	ResponseType        string
	ClientID            string
	RedirectURI         string
	State               string
	Scope               string
	CodeChallenge       string
	CodeChallengeMethod string
	Resource            string
}

func parseAuthzParams(v url.Values) authzParams {
	scope := strings.TrimSpace(v.Get("scope"))
	if scope == "" {
		scope = scopeMCP
	}
	return authzParams{
		ResponseType:        v.Get("response_type"),
		ClientID:            v.Get("client_id"),
		RedirectURI:         v.Get("redirect_uri"),
		State:               v.Get("state"),
		Scope:               scope,
		CodeChallenge:       v.Get("code_challenge"),
		CodeChallengeMethod: v.Get("code_challenge_method"),
		Resource:            v.Get("resource"),
	}
}

// resourceOK validates the RFC 8707 resource indicator. Absent is tolerated
// (bound to the configured resource); when present it must identify this server.
func (p *Provider) resourceOK(resource string) bool {
	if resource == "" {
		return true
	}
	r := strings.TrimRight(resource, "/")
	return r == p.cfg.Resource || r == p.cfg.Issuer || r == p.cfg.Issuer+"/mcp"
}

// handleAuthorize renders the consent screen (GET) and processes it (POST).
func (p *Provider) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	if !p.authorizeLimiter.Allow(clientIP(r, p.cfg.TrustProxy)) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	switch r.Method {
	case http.MethodGet:
		p.authorizeGet(w, r)
	case http.MethodPost:
		p.authorizePost(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// validateAuthz checks the client and redirect_uri (whose failures must NOT
// redirect, to avoid open redirects) and then the remaining parameters. It
// returns the resolved client and an ok flag; when ok is false it has already
// written the appropriate response (an error page for client/redirect failures,
// or a redirect-back with an OAuth error for the rest).
func (p *Provider) validateAuthz(w http.ResponseWriter, r *http.Request, pr authzParams) (*Client, bool) {
	if pr.ClientID == "" {
		http.Error(w, "missing client_id", http.StatusBadRequest)
		return nil, false
	}
	c, err := p.resolveClient(r.Context(), pr.ClientID)
	if err != nil {
		http.Error(w, "unknown or unresolvable client_id", http.StatusBadRequest)
		return nil, false
	}
	if pr.RedirectURI == "" || !redirectURIAllowed(c.RedirectURIs, pr.RedirectURI) {
		http.Error(w, "redirect_uri not registered for client", http.StatusBadRequest)
		return nil, false
	}
	// From here, errors redirect back to the (validated) redirect_uri.
	if pr.ResponseType != responseTypeCode {
		p.redirectError(w, r, pr, "unsupported_response_type", "only code is supported")
		return nil, false
	}
	if pr.CodeChallenge == "" || pr.CodeChallengeMethod != pkceMethodS256 {
		p.redirectError(w, r, pr, "invalid_request", "PKCE S256 is required")
		return nil, false
	}
	if !p.resourceOK(pr.Resource) {
		p.redirectError(w, r, pr, "invalid_target", "resource does not identify this server")
		return nil, false
	}
	return c, true
}

func (p *Provider) authorizeGet(w http.ResponseWriter, r *http.Request) {
	pr := parseAuthzParams(r.URL.Query())
	c, ok := p.validateAuthz(w, r, pr)
	if !ok {
		return
	}
	p.renderConsent(w, pr, c, "", 0)
}

// renderConsent issues a fresh CSRF cookie and renders the consent screen. A
// non-zero status writes that status first (the error re-render uses 401). The
// page handles a credential, so it forbids framing (clickjacking) and restricts
// where the form may post.
func (p *Provider) renderConsent(w http.ResponseWriter, pr authzParams, c *Client, errMsg string, status int) {
	csrf, err := randomToken(24)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookie,
		Value:    csrf,
		Path:     "/oauth/authorize",
		MaxAge:   600,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	host := pr.RedirectURI
	if u, err := url.Parse(pr.RedirectURI); err == nil {
		host = u.Host
	}
	name := c.Name
	if name == "" {
		name = "An application"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	if status != 0 {
		w.WriteHeader(status)
	}
	_ = consentTmpl.Execute(w, consentData{
		ClientName:          name,
		ClientID:            pr.ClientID,
		RedirectURI:         pr.RedirectURI,
		RedirectHost:        host,
		State:               pr.State,
		Scope:               pr.Scope,
		CodeChallenge:       pr.CodeChallenge,
		CodeChallengeMethod: pr.CodeChallengeMethod,
		Resource:            pr.Resource,
		CSRF:                csrf,
		Error:               errMsg,
	})
}

func (p *Provider) authorizePost(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	pr := parseAuthzParams(r.PostForm)
	c, ok := p.validateAuthz(w, r, pr)
	if !ok {
		return
	}
	// CSRF: double-submit cookie.
	cookie, err := r.Cookie(csrfCookie)
	if err != nil || cookie.Value == "" || cookie.Value != r.PostFormValue(csrfFormName) {
		http.Error(w, "invalid or missing CSRF token", http.StatusForbidden)
		return
	}

	apiKey := strings.TrimSpace(r.PostFormValue("api_key"))
	userID, err := validateHLK(r.Context(), p.cfg.APIBaseURL, apiKey)
	if err != nil {
		// Re-render the consent page with an error rather than leaking detail.
		p.renderConsent(w, pr, c, "That API key was not accepted. Check it and try again.", http.StatusUnauthorized)
		return
	}

	encHLK, err := p.crypto.Encrypt(userID, apiKey)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Persist the encrypted credential (the single source of truth used by /mcp)
	// BEFORE issuing a code, so a storage failure aborts the grant rather than
	// minting tokens that can never load a credential. Upsert makes a retry safe.
	if err := p.store.SaveUserCredential(r.Context(), userID, encHLK); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	code, err := randomToken(32)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	ac := &AuthCode{
		CodeHash:      hashToken(code),
		ClientID:      pr.ClientID,
		UserID:        userID,
		Scope:         pr.Scope,
		RedirectURI:   pr.RedirectURI,
		CodeChallenge: pr.CodeChallenge,
		Resource:      p.cfg.Resource,
		ExpiresAt:     p.now().Add(authCodeTTL),
	}
	if err := p.store.SaveAuthCode(r.Context(), ac); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Clear the CSRF cookie and redirect back with the code.
	http.SetCookie(w, &http.Cookie{
		Name: csrfCookie, Path: "/oauth/authorize", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	u, _ := url.Parse(pr.RedirectURI)
	q := u.Query()
	q.Set("code", code)
	if pr.State != "" {
		q.Set("state", pr.State)
	}
	q.Set("iss", p.cfg.Issuer)
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (p *Provider) redirectError(w http.ResponseWriter, r *http.Request, pr authzParams, code, desc string) {
	u, err := url.Parse(pr.RedirectURI)
	if err != nil {
		http.Error(w, code+": "+desc, http.StatusBadRequest)
		return
	}
	q := u.Query()
	q.Set("error", code)
	if desc != "" {
		q.Set("error_description", desc)
	}
	if pr.State != "" {
		q.Set("state", pr.State)
	}
	q.Set("iss", p.cfg.Issuer)
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}
