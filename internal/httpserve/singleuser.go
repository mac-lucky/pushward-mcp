package httpserve

import (
	"net/http"

	"github.com/mac-lucky/pushward-mcp/internal/client"
)

// singleUserID tags tool-call log lines in single-user mode, where there is no
// OAuth subject.
const singleUserID = "single-user"

// SingleUser is the Authenticator for PUSHWARD_MCP_HTTP_AUTH=none. It mounts no
// OAuth routes and runs every MCP request as the one configured PushWard key.
// Callers do not authenticate at all, so the listener must only be reachable by
// trusted clients (a network policy, loopback).
type SingleUser struct {
	// Token is the PushWard integration key every request acts as.
	Token string
}

// RegisterRoutes mounts nothing: there is no login flow in single-user mode.
func (SingleUser) RegisterRoutes(*http.ServeMux) {}

// WrapMCP injects the configured key into the request context. Any inbound
// Authorization header is dropped so a caller can never choose the identity.
//
// A request carrying an Origin header comes from a browser, and a headless
// agent never sends one. Rejecting it keeps a web page from driving the key
// through the permissive CORS on /mcp when the server runs on a laptop.
func (s SingleUser) WrapMCP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" {
			http.Error(w, "browser requests are not accepted in single-user mode", http.StatusForbidden)
			return
		}
		ctx := client.ContextWithToken(r.Context(), s.Token)
		ctx = client.ContextWithUserID(ctx, singleUserID)
		r = r.Clone(ctx)
		r.Header.Del("Authorization")
		next.ServeHTTP(w, r)
	})
}
