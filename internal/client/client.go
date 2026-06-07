package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxRespBytes caps how much of a response body DoJSON will read into memory.
const maxRespBytes = 1 << 20 // 1MB

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

	// Read one byte past the cap so we can distinguish "exactly at the cap" from
	// "truncated". A truncated success body is no longer valid JSON.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes+1))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("reading response: %w", err)
	}
	overLimit := len(respBody) > maxRespBytes
	if overLimit {
		respBody = respBody[:maxRespBytes]
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := extractErrorMessage(respBody)
		return nil, resp.StatusCode, fmt.Errorf("%s %s returned %d: %s", method, path, resp.StatusCode, msg)
	}

	// A success body that overflowed the cap was truncated mid-JSON; return a
	// clear error instead of handing back corrupt data that fails to parse.
	if overLimit {
		return nil, resp.StatusCode, fmt.Errorf("%s %s response exceeds the %d-byte cap", method, path, maxRespBytes)
	}

	if len(respBody) == 0 {
		return json.RawMessage(`{}`), resp.StatusCode, nil
	}
	return json.RawMessage(respBody), resp.StatusCode, nil
}

// extractErrorMessage pulls a human-friendly message from an RFC 9457 Problem
// body, falling back to a truncated raw snippet if parsing fails.
func extractErrorMessage(body []byte) string {
	var p struct {
		Title        string `json:"title"`
		Detail       string `json:"detail"`
		Code         string `json:"code"`
		RetryAfterMs int64  `json:"retry_after_ms"`
	}
	if json.Unmarshal(body, &p) == nil && (p.Detail != "" || p.Title != "" || p.Code != "") {
		var parts []string
		if p.Code != "" {
			parts = append(parts, "["+p.Code+"]")
		}
		switch {
		case p.Detail != "":
			parts = append(parts, p.Detail)
		case p.Title != "":
			parts = append(parts, p.Title)
		}
		if p.RetryAfterMs > 0 {
			parts = append(parts, fmt.Sprintf("(retry_after_ms=%d)", p.RetryAfterMs))
		}
		return strings.Join(parts, " ")
	}
	snippet := string(body)
	if len(snippet) > 500 {
		snippet = snippet[:500] + "..."
	}
	return snippet
}
