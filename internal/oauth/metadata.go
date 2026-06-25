package oauth

import (
	"net/http"

	mcpserver "github.com/mark3labs/mcp-go/server"
)

// prmHandler serves RFC 9728 Protected Resource Metadata (with permissive CORS,
// provided by mcp-go's handler).
func (p *Provider) prmHandler() http.Handler {
	return mcpserver.NewProtectedResourceMetadataHandler(mcpserver.ProtectedResourceMetadataConfig{
		Resource:               p.cfg.Resource,
		AuthorizationServers:   []string{p.cfg.Issuer},
		BearerMethodsSupported: []string{"header"},
		ScopesSupported:        []string{scopeMCP},
		JWKSURI:                p.cfg.jwksURL(),
		ResourceName:           "PushWard MCP",
		ResourceDocumentation:  "https://pushward.app/docs/mcp",
	})
}

// asMetadataHandler serves RFC 8414 Authorization Server Metadata. The
// advertised capabilities make Claude select the CIMD public-client path
// (token_endpoint_auth_methods_supported: ["none"] +
// client_id_metadata_document_supported: true), while still exposing a DCR
// registration_endpoint as a fallback.
func (p *Provider) asMetadataHandler() http.Handler {
	doc := map[string]any{
		"issuer":                                         p.cfg.Issuer,
		"authorization_endpoint":                         p.cfg.authorizeURL(),
		"token_endpoint":                                 p.cfg.tokenURL(),
		"registration_endpoint":                          p.cfg.registerURL(),
		"jwks_uri":                                       p.cfg.jwksURL(),
		"response_types_supported":                       []string{responseTypeCode},
		"grant_types_supported":                          []string{grantTypeAuthorizationCode, grantTypeRefreshToken},
		"token_endpoint_auth_methods_supported":          []string{authMethodNone},
		"code_challenge_methods_supported":               []string{pkceMethodS256},
		"scopes_supported":                               []string{scopeMCP},
		"client_id_metadata_document_supported":          true,
		"authorization_response_iss_parameter_supported": true,
	}
	return corsJSONHandler(doc)
}

func (p *Provider) jwksHandler() http.Handler {
	body := p.signer.JWKS()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setCORS(w)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write(body)
	})
}

func corsJSONHandler(doc map[string]any) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setCORS(w)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(w, http.StatusOK, doc)
	})
}

func setCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
}

// corsPOST wraps the OAuth POST endpoints (token, register) with permissive CORS
// and OPTIONS preflight handling so browser-based connector flows (Claude.ai,
// ChatGPT) can complete the token exchange / dynamic registration. These are
// public-client (PKCE) endpoints carrying no cookie or client secret, so
// Allow-Origin:* is safe - the auth code + PKCE verifier in the request body are
// the only credentials, and they are useless to a cross-origin attacker.
func corsPOST(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	})
}
