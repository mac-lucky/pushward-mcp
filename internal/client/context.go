package client

import "context"

// tokenContextKey is the unexported context key under which a per-request
// PushWard API token is carried. Using a private struct type avoids collisions
// with any other package's context values.
type tokenContextKey struct{}

// userIDContextKey carries the authenticated user's stable id (the OAuth
// subject) for audit logging. It is never the credential itself.
type userIDContextKey struct{}

// ContextWithUserID returns a child context carrying the authenticated user id
// for audit logging. An empty id is a no-op.
func ContextWithUserID(ctx context.Context, userID string) context.Context {
	if userID == "" {
		return ctx
	}
	return context.WithValue(ctx, userIDContextKey{}, userID)
}

// UserIDFromContext returns the authenticated user id carried by ctx, or "".
func UserIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(userIDContextKey{}).(string); ok {
		return v
	}
	return ""
}

// ContextWithToken returns a child context carrying the given per-request
// PushWard token. In HTTP (remote) mode the transport extracts each client's
// token from the inbound Authorization header and stashes it here so that
// outbound upstream calls are made with that user's credential rather than a
// single process-wide token. An empty token is a no-op (the context is returned
// unchanged) so callers need not special-case the stdio path.
func ContextWithToken(ctx context.Context, token string) context.Context {
	if token == "" {
		return ctx
	}
	return context.WithValue(ctx, tokenContextKey{}, token)
}

// TokenFromContext returns the per-request token carried by ctx, or "" if none
// is present. Read by Base.DoJSON when the client is configured to prefer the
// context token (see Base.useContextToken).
func TokenFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(tokenContextKey{}).(string); ok {
		return v
	}
	return ""
}
