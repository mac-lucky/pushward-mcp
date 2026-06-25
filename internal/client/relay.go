package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
)

// relayProviderPattern bounds the provider path segment to a safe shape. The
// provider reaches this client as a tool-call argument; even though tool
// schemas restrict it via an enum, validating the format here is
// defense-in-depth - it prevents a path-traversal ("../"), an absolute path, or
// an unexpected-host segment from ever being concatenated into the request URL,
// without coupling to (and drifting from) the relay's generated provider set.
var relayProviderPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

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
