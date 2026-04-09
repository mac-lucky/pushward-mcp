package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Base is the shared HTTP client for both API and relay requests.
type Base struct {
	httpClient *http.Client
	baseURL    string
	token      string
}

// NewBase creates a new Base HTTP client.
func NewBase(baseURL, token string) *Base {
	return &Base{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    baseURL,
		token:      token,
	}
}

// DoJSON sends an HTTP request and returns the raw JSON response body.
// For non-2xx responses, it returns an error containing the status code and body snippet.
func (b *Base) DoJSON(ctx context.Context, method, path string, body any) (json.RawMessage, int, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("marshaling request body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, b.baseURL+path, bodyReader)
	if err != nil {
		return nil, 0, fmt.Errorf("creating request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+b.token)

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB cap
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := string(respBody)
		if len(snippet) > 500 {
			snippet = snippet[:500] + "..."
		}
		return nil, resp.StatusCode, fmt.Errorf("%s %s returned %d: %s", method, path, resp.StatusCode, snippet)
	}

	if len(respBody) == 0 {
		return json.RawMessage(`{}`), resp.StatusCode, nil
	}
	return json.RawMessage(respBody), resp.StatusCode, nil
}
