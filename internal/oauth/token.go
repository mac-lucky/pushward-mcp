package oauth

import (
	"context"
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
	// Hosted connectors (Claude.ai, ChatGPT) refresh every user's grant from a
	// few shared egress addresses, so only failed requests cost a token: valid
	// refreshes never drain the bucket, guessed or replayed tokens still do.
	key := rateKey(p.clientIP(r))
	if !p.tokenLimiter.Peek(key) {
		oauthError(w, http.StatusTooManyRequests, "temporarily_unavailable", "rate limited")
		return
	}
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	defer func() {
		if sw.status >= 400 && sw.status < 500 {
			p.tokenLimiter.Charge(key)
		}
	}()
	w = sw
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

// statusWriter records the status a handler answered with.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
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

	// The consent's key becomes live only here, under an id of its own, so no
	// other grant of this user sees it. The code records the id, so a replay of
	// it can end exactly this grant.
	credID, err := randomToken(16)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not generate credential id")
		return
	}
	family, err := randomToken(16)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not generate grant id")
		return
	}

	// Only codes that carry a pending key are stored under grantTokenHash. One a
	// pod on an older release minted is refused, and its consent is redone.
	ac, err := p.store.ConsumeAuthCode(r.Context(), grantTokenHash(code), credID)
	if errors.Is(err, ErrCodeAlreadyUsed) {
		// Replay of a consumed code is treated as compromise of that grant: revoke
		// the tokens minted from the first use (RFC 6749 section 4.1.2 / OAuth
		// 2.1). ConsumeAuthCode surfaces the user and credential on the reuse path
		// so this is actionable.
		if ac != nil && ac.UserID != "" {
			p.revokeGrant(r.Context(), "authorization code replay", ac.UserID, ac.CredentialID)
		}
		oauthError(w, http.StatusBadRequest, "invalid_grant", "authorization code already used")
		return
	}
	if errors.Is(err, ErrNotFound) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "invalid or expired authorization code")
		return
	}
	if err != nil {
		// A store failure is not a bad code: invalid_grant would make the client
		// drop it and restart consent, a 500 lets it retry.
		oauthError(w, http.StatusInternalServerError, "server_error", "could not read authorization code")
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

	if err := p.store.SaveCredential(r.Context(), credID, ac.UserID, ac.EncryptedHLK); err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not persist credential")
		return
	}
	p.issueTokens(w, r, RefreshToken{
		UserID: ac.UserID, ClientID: ac.ClientID, Scope: ac.Scope, CredentialID: credID, Family: family,
	}, "")
}

func (p *Provider) grantRefreshToken(w http.ResponseWriter, r *http.Request) {
	raw := r.PostFormValue("refresh_token")
	if raw == "" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "refresh_token is required")
		return
	}
	rt, err := p.store.GetRefreshToken(r.Context(), grantTokenHash(raw))
	if errors.Is(err, ErrNotFound) {
		// A grant from before per-grant credentials (see issueTokens).
		rt, err = p.store.GetRefreshToken(r.Context(), hashToken(raw))
	}
	if errors.Is(err, ErrNotFound) {
		p.revokePurgedFamily(r.Context(), raw)
		oauthError(w, http.StatusBadRequest, "invalid_grant", "invalid refresh token")
		return
	}
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not read refresh token")
		return
	}
	if rt.RevokedAt != nil {
		// A revoked token presented within the grace window is a benign
		// double-submit; beyond it, it indicates token theft (the legitimate
		// client already rotated), so revoke the whole family.
		if p.now().Sub(*rt.RevokedAt) > refreshGrace {
			p.revokeGrant(r.Context(), "refresh token replay", rt.UserID, rt.CredentialID)
		}
		oauthError(w, http.StatusBadRequest, "invalid_grant", "refresh token no longer valid")
		return
	}
	if rt.ExpiresAt.Before(p.now()) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "refresh token expired")
		return
	}
	if !p.grantKeyUsable(w, r, rt) {
		return
	}

	g := RefreshToken{UserID: rt.UserID, ClientID: rt.ClientID, Scope: rt.Scope, CredentialID: rt.CredentialID, Family: rt.Family}
	if g.CredentialID != "" && g.Family == "" {
		// A key-bound grant from before families, or rotated by an older pod.
		if g.Family, err = randomToken(16); err != nil {
			oauthError(w, http.StatusInternalServerError, "server_error", "could not generate grant id")
			return
		}
	}
	// Rotate atomically: only the request that wins the conditional revoke may
	// mint a new pair (issueTokens). A lost race (concurrent reuse of one
	// token) is rejected, closing the double-spend window that would otherwise
	// let a stolen token and the legitimate client both rotate undetected.
	p.issueTokens(w, r, g, rt.TokenHash)
}

// revokeGrant is the theft response to a replayed code or refresh token: it
// revokes the refresh tokens of the one grant the replay belongs to. A legacy
// grant has no id of its own, so all of the user's tokens go, as they always
// did.
func (p *Provider) revokeGrant(ctx context.Context, what, userID, credID string) {
	var err error
	if credID != "" {
		err = p.store.RevokeGrantRefreshTokens(ctx, credID)
	} else {
		err = p.store.RevokeUserRefreshTokens(ctx, userID)
	}
	if err != nil {
		p.log.Warn("failed to revoke refresh tokens after "+what, "user", userID, "err", err)
		return
	}
	p.log.Warn(what+" detected; revoked refresh tokens", "user", userID, "grant_scoped", credID != "")
}

// revokePurgedFamily handles a refresh token with no row. Rows of rotated
// tokens are purged an hour after rotation, so a stolen token replayed later,
// or the legitimate client's old one after a thief rotated it, lands here; its
// "<family>." prefix still names the grant, which is revoked if it is live.
// A token without the prefix (legacy, or minted before families) cannot be
// traced and is only refused.
func (p *Provider) revokePurgedFamily(ctx context.Context, raw string) {
	family, _, ok := strings.Cut(raw, ".")
	if !ok || family == "" {
		return
	}
	revoked, err := p.store.RevokeRefreshFamily(ctx, family)
	if err != nil {
		p.log.Warn("failed to revoke refresh token family after a replay", "err", err)
		return
	}
	if revoked {
		p.log.Warn("refresh token replay detected after its row was purged; revoked the grant")
	}
}

// grantKeyUsable checks the grant's stored key before a refresh. A key that is
// gone, no longer decrypts or that PushWard refuses (rolled, revoked, expired)
// ends the grant with invalid_grant, so the client re-runs consent instead of
// refreshing into a key that fails every call. A PushWard outage, rate limit or
// timeout is not proof of a dead key, so the refresh goes on. It writes the
// response itself when it returns false.
func (p *Provider) grantKeyUsable(w http.ResponseWriter, r *http.Request, rt *RefreshToken) bool {
	hlk, err := p.loadHLK(r.Context(), rt.UserID, rt.CredentialID)
	if err != nil && !credentialGone(err) {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not read credential")
		return false
	}
	if err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), keyCheckTimeout)
		_, status, verr := validateHLK(ctx, p.cfg.APIBaseURL, hlk)
		cancel()
		if verr == nil || status != http.StatusUnauthorized {
			return true
		}
	}
	// Credential rows stay: Cleanup drops a grant key once its tokens are gone,
	// and a legacy per-user row may still serve the user's other chains.
	if rt.CredentialID != "" {
		err = p.store.RevokeGrantRefreshTokens(r.Context(), rt.CredentialID)
	} else {
		_, err = p.store.RevokeRefreshToken(r.Context(), rt.TokenHash)
	}
	if err != nil {
		p.log.Warn("failed to end grant with an unusable key", "user", rt.UserID, "err", err)
	}
	p.log.Info("refresh refused: the grant's PushWard key is no longer usable", "user", rt.UserID, "client_id", rt.ClientID)
	oauthError(w, http.StatusBadRequest, "invalid_grant", "the stored PushWard API key is no longer valid; authorize again")
	return false
}

// issueTokens signs an access token for grant g and mints its next refresh
// token. With prevHash set it rotates that token into the new one, and answers
// invalid_grant if another request rotated it first.
func (p *Provider) issueTokens(w http.ResponseWriter, r *http.Request, g RefreshToken, prevHash string) {
	now := p.now()
	access, err := p.signer.Sign(g.UserID, p.cfg.Resource, g.ClientID, g.Scope, g.CredentialID, now)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not sign token")
		return
	}
	refresh, err := randomToken(32)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not generate refresh token")
		return
	}
	if g.Family != "" {
		// "." is outside the base64url alphabet, so the prefix parses back out.
		refresh = g.Family + "." + refresh
	}
	// A grant without a credential id keeps the hash older releases read, so a
	// pod on one can still rotate it during a rollout or after a rollback.
	tokenHash := hashToken(refresh)
	if g.CredentialID != "" {
		tokenHash = grantTokenHash(refresh)
	}
	next := &g
	next.TokenHash = tokenHash
	next.Resource = p.cfg.Resource
	next.PrevHash = prevHash
	next.ExpiresAt = now.Add(refreshTokenTTL)
	next.RevokedAt = nil
	if prevHash == "" {
		err = p.store.SaveRefreshToken(r.Context(), next)
	} else {
		var won bool
		won, err = p.store.RotateRefreshToken(r.Context(), prevHash, next)
		if err == nil && !won {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "refresh token no longer valid")
			return
		}
	}
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not persist refresh token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(accessTokenTTL.Seconds()),
		"refresh_token": refresh,
		"scope":         strings.TrimSpace(g.Scope),
	})
}
