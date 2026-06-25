package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// maxRespBytes caps how much of a response body DoJSON will read into memory.
const maxRespBytes = 1 << 20 // 1MB

// remoteMode, when true, makes extractErrorMessage redact upstream Problem
// detail/title/raw bodies from the caller-facing error so an internet-exposed
// MCP endpoint does not leak upstream implementation detail to clients. It is
// set once at startup (HTTP transport) before any request is served, so it is
// safe to read without synchronization.
var remoteMode bool

// SetRemoteMode toggles caller-facing error redaction. Call once at startup.
func SetRemoteMode(v bool) { remoteMode = v }

// Base is the shared HTTP client for both API and relay requests.
type Base struct {
	httpClient *http.Client
	baseURL    string
	token      string
	// useContextToken makes DoJSON prefer a per-request token carried in the
	// context (set by the HTTP transport from the inbound Authorization header)
	// over the struct's token. The struct token remains a fallback for stdio
	// mode. The API client opts in; the relay client does not (relay uses a
	// fixed server-side credential, never a per-user token).
	useContextToken bool
}

// NewBase creates a new Base HTTP client.
func NewBase(baseURL, token string) *Base {
	return &Base{
		httpClient: &http.Client{Timeout: 30 * time.Second, Transport: newTransport()},
		baseURL:    baseURL,
		token:      token,
	}
}

// newTransport returns the process-wide shared *http.Transport. A single transport
// (and thus a single connection pool) is reused by every Base — the long-lived API
// and relay clients AND the short-lived Base that validateHLK spins up per consent —
// so a throwaway client never leaves behind its own idle connection pool. The pool
// is tuned for the shared http-mode client that serves every user (the per-user
// token rides in the request context): the stdlib default keeps only 2 idle
// connections per host, forcing TLS re-handshakes to api.pushward.app under
// concurrent load, so the idle pool is raised for connection reuse.
func newTransport() *http.Transport {
	sharedTransportOnce.Do(func() {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.MaxIdleConns = 200
		t.MaxIdleConnsPerHost = 100
		t.MaxConnsPerHost = 200
		t.IdleConnTimeout = 90 * time.Second
		sharedTransport = t
	})
	return sharedTransport
}

var (
	sharedTransport     *http.Transport
	sharedTransportOnce sync.Once
)

// ParseBearer extracts the token from an "Authorization: Bearer <token>" header
// value, matching the scheme case-insensitively. It returns "" when the value is
// absent or malformed. Shared by the HTTP transport and the OAuth resource-server
// middleware so the two never diverge.
func ParseBearer(header string) string {
	const prefix = "bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
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
	// Prefer the per-request token (HTTP mode) when this client opts in, falling
	// back to the struct token (stdio mode). Only set the header when a token is
	// actually present so a tokenless request reaches upstream as anonymous and
	// gets a clean 401 rather than a malformed "Bearer " header.
	token := b.token
	if b.useContextToken {
		if t := TokenFromContext(ctx); t != "" {
			token = t
		}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

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
//
// In remote mode the upstream's free-text title/detail and any raw body are
// withheld from the caller-facing error (they can leak internal field names,
// DB messages, or PII) — only the machine-readable Problem `code` and any
// retry hint are surfaced. The full message is still available to the operator
// via server-side logs.
func extractErrorMessage(body []byte) string {
	var p struct {
		Title        string `json:"title"`
		Detail       string `json:"detail"`
		Code         string `json:"code"`
		RetryAfterMs int64  `json:"retry_after_ms"`
	}
	parsed := json.Unmarshal(body, &p) == nil && (p.Detail != "" || p.Title != "" || p.Code != "")

	if remoteMode {
		var parts []string
		if parsed && p.Code != "" {
			parts = append(parts, "["+p.Code+"]")
		}
		if parsed && p.RetryAfterMs > 0 {
			parts = append(parts, fmt.Sprintf("(retry_after_ms=%d)", p.RetryAfterMs))
		}
		if len(parts) == 0 {
			return "upstream error (detail withheld)"
		}
		return strings.Join(parts, " ")
	}

	if parsed {
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
