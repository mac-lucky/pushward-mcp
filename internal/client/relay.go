package client

import (
	"context"
	"encoding/json"
	"net/http"
)

// RelayClient wraps the PushWard Relay (relay.pushward.app).
type RelayClient struct{ *Base }

// NewRelayClient creates a new PushWard Relay client.
func NewRelayClient(baseURL, token string) *RelayClient {
	return &RelayClient{NewBase(baseURL, token)}
}

// PostWebhook sends a webhook payload to a relay provider endpoint.
func (c *RelayClient) PostWebhook(ctx context.Context, provider string, body any) (json.RawMessage, error) {
	raw, _, err := c.DoJSON(ctx, http.MethodPost, "/"+provider, body)
	return raw, err
}
