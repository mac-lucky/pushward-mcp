package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
)

// relayProviderPattern bounds the provider path segment to a safe shape. The
// provider reaches this client as a tool-call argument; even though tool
// schemas restrict it via an enum, validating the format here is
// defense-in-depth - it prevents a path-traversal ("../"), an absolute path, or
// an unexpected-host segment from ever being concatenated into the request URL,
// without coupling to (and drifting from) the relay's generated provider set.
var relayProviderPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// relaySourcePattern is the relay's own rule for ?source=. The relay answers a
// bad one with a 422 whose detail is only "validation failed", so the rule is
// checked here, where the error can say what it is.
var relaySourcePattern = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// RelayClient wraps the PushWard Relay (relay.pushward.app).
type RelayClient struct{ *Base }

// NewRelayClient creates a new PushWard Relay client. The relay client uses a
// fixed server-side credential (never a per-user context token).
func NewRelayClient(baseURL, token string) *RelayClient {
	return &RelayClient{NewBase(baseURL, token)}
}

// PostWebhook sends a webhook payload to a relay provider endpoint.
func (c *RelayClient) PostWebhook(ctx context.Context, provider string, body any) (json.RawMessage, error) {
	if !relayProviderPattern.MatchString(provider) {
		return nil, fmt.Errorf("invalid relay provider %q", provider)
	}
	raw, _, err := c.DoJSON(ctx, http.MethodPost, "/"+provider, body)
	return raw, err
}

// PostRoot sends a payload to the relay root, POST /, the one URL any service
// can be pointed at: a payload it recognises as a provider's goes to that
// provider's route, anything else to the universal webhook. q becomes the
// query string, where a source has to follow the relay's rule.
func (c *RelayClient) PostRoot(ctx context.Context, body any, q url.Values) (json.RawMessage, error) {
	if q.Has("source") && !relaySourcePattern.MatchString(q.Get("source")) {
		return nil, fmt.Errorf("invalid source %q: lowercase letters, digits and -, up to 32 characters", q.Get("source"))
	}
	raw, _, err := c.DoJSON(ctx, http.MethodPost, withQuery("/", q), body)
	return raw, err
}
