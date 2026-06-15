package oauth

import (
	"errors"
	"net/http"
	"strings"
)

// handleToken implements POST /oauth/token for the authorization_code and
// refresh_token grants. It is a public-client endpoint (no client secret); the
// authorization_code grant is bound to the client via PKCE.
func (p *Provider) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		oauthError(w, http.StatusMethodNotAllowed, "invalid_request", "POST required")
		return
	}
	if !p.tokenLimiter.Allow(clientIP(r, p.cfg.TrustProxy)) {
		oauthError(w, http.StatusTooManyRequests, "temporarily_unavailable", "rate limited")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "invalid form body")
		return
	}
	switch r.PostFormValue("grant_type") {
	case grantTypeAuthorizationCode:
		p.grantAuthorizationCode(w, r)
	case grantTypeRefreshToken:
		p.grantRefreshToken(w, r)
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "unsupported grant_type")
	}
}

func (p *Provider) grantAuthorizationCode(w http.ResponseWriter, r *http.Request) {
	code := r.PostFormValue("code")
	redirectURI := r.PostFormValue("redirect_uri")
	clientID := r.PostFormValue("client_id")
	verifier := r.PostFormValue("code_verifier")
	resource := r.PostFormValue("resource")

	if code == "" || verifier == "" || clientID == "" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "code, code_verifier and client_id are required")
		return
	}

	ac, err := p.store.ConsumeAuthCode(r.Context(), hashToken(code))
	if errors.Is(err, ErrCodeAlreadyUsed) {
		// Replay of a consumed code: treat as compromise of that grant.
		oauthError(w, http.StatusBadRequest, "invalid_grant", "authorization code already used")
		return
	}
	if err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "invalid or expired authorization code")
		return
	}
	if clientID != ac.ClientID {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "client mismatch")
		return
	}
	if redirectURI != ac.RedirectURI {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri mismatch")
		return
	}
	if !verifyPKCES256(verifier, ac.CodeChallenge) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
		return
	}
	if resource != "" && !p.resourceOK(resource) {
		oauthError(w, http.StatusBadRequest, "invalid_target", "resource mismatch")
		return
	}

	p.issueTokens(w, r, ac.UserID, ac.ClientID, ac.Scope)
}

func (p *Provider) grantRefreshToken(w http.ResponseWriter, r *http.Request) {
	raw := r.PostFormValue("refresh_token")
	if raw == "" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "refresh_token is required")
		return
	}
	rt, err := p.store.GetRefreshToken(r.Context(), hashToken(raw))
	if err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "invalid refresh token")
		return
	}
	if rt.RevokedAt != nil {
		// A revoked token presented within the grace window is a benign
		// double-submit; beyond it, it indicates token theft (the legitimate
		// client already rotated), so revoke the whole family.
		if p.now().Sub(*rt.RevokedAt) > refreshGrace {
			_ = p.store.RevokeUserRefreshTokens(r.Context(), rt.UserID)
		}
		oauthError(w, http.StatusBadRequest, "invalid_grant", "refresh token no longer valid")
		return
	}
	if rt.ExpiresAt.Before(p.now()) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "refresh token expired")
		return
	}

	// Rotate atomically: only the request that wins the conditional revoke may
	// mint a new pair. A lost race (concurrent reuse of one token) gets won=false
	// and is rejected, closing the double-spend window that would otherwise let a
	// stolen token and the legitimate client both rotate undetected.
	won, err := p.store.RevokeRefreshToken(r.Context(), rt.TokenHash)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not rotate refresh token")
		return
	}
	if !won {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "refresh token no longer valid")
		return
	}
	p.issueTokensWithPrev(w, r, rt.UserID, rt.ClientID, rt.Scope, rt.TokenHash)
}

func (p *Provider) issueTokens(w http.ResponseWriter, r *http.Request, userID, clientID, scope string) {
	p.issueTokensWithPrev(w, r, userID, clientID, scope, "")
}

func (p *Provider) issueTokensWithPrev(w http.ResponseWriter, r *http.Request, userID, clientID, scope, prevHash string) {
	now := p.now()
	access, err := p.signer.Sign(userID, p.cfg.Resource, clientID, scope, now)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not sign token")
		return
	}
	refresh, err := randomToken(32)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not generate refresh token")
		return
	}
	if err := p.store.SaveRefreshToken(r.Context(), &RefreshToken{
		TokenHash: hashToken(refresh),
		UserID:    userID,
		ClientID:  clientID,
		Scope:     scope,
		Resource:  p.cfg.Resource,
		PrevHash:  prevHash,
		ExpiresAt: now.Add(refreshTokenTTL),
	}); err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not persist refresh token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(accessTokenTTL.Seconds()),
		"refresh_token": refresh,
		"scope":         strings.TrimSpace(scope),
	})
}
