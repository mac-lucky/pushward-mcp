package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
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
// set once at startup (OAuth http mode) before any request is served, so it is
// safe to read without synchronization.
var remoteMode bool

// SetRemoteMode toggles caller-facing error redaction. Call once at startup.
func SetRemoteMode(v bool) { remoteMode = v }

// logger receives what remote mode withholds from callers - the transport
// error, or the upstream Problem detail behind a redacted message - so an
// operator can still tell a refused dial from a rejected key. Like remoteMode
// it is set once at startup. Nothing logged through it carries the
// Authorization header or a request body.
var logger = slog.Default()

// SetLogger sets where remote mode logs withheld upstream errors. Call once at
// startup.
func SetLogger(l *slog.Logger) { logger = l }

// logWithheld logs the cause behind a redacted caller-facing error.
func logWithheld(ctx context.Context, level slog.Level, msg, method, path string, attrs ...slog.Attr) {
	logger.LogAttrs(ctx, level, msg, append([]slog.Attr{
		slog.String("method", method),
		slog.String("path", path),
		slog.String("user", UserIDFromContext(ctx)),
	}, attrs...)...)
}

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
// (and thus a single connection pool) is reused by every Base - the long-lived API
// and relay clients AND the short-lived Base that validateHLK spins up per consent -
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
		if remoteMode {
			logWithheld(ctx, slog.LevelWarn, "upstream request failed", method, path, slog.String("err", err.Error()))
			return nil, 0, transportError(method, path, err)
		}
		return nil, 0, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	// Read one byte past the cap so we can distinguish "exactly at the cap" from
	// "truncated". A truncated success body is no longer valid JSON.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes+1))
	if err != nil {
		if remoteMode {
			// The read error names both ends of the connection, like a dial error.
			logWithheld(ctx, slog.LevelWarn, "upstream response cut off", method, path,
				slog.Int("status", resp.StatusCode), slog.String("err", err.Error()))
			return nil, resp.StatusCode, fmt.Errorf("%s %s: upstream response cut off", method, path)
		}
		return nil, resp.StatusCode, fmt.Errorf("reading response: %w", err)
	}
	overLimit := len(respBody) > maxRespBytes
	if overLimit {
		respBody = respBody[:maxRespBytes]
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := extractErrorMessage(respBody)
		if remoteMode {
			// A 4xx is mostly the caller's own mistake; a 5xx is ours.
			level := slog.LevelInfo
			if resp.StatusCode >= 500 {
				level = slog.LevelWarn
			}
			logWithheld(ctx, level, "upstream error response", method, path,
				slog.Int("status", resp.StatusCode), slog.String("detail", fullErrorMessage(respBody)))
		}
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

// transportError is the caller-facing form of a failed round trip in remote
// mode. The transport's own message quotes the upstream URL and the address it
// dialed, which can be an in-cluster Service that callers of a public endpoint
// have no business seeing, so only the operation and whether it timed out stay
// (DoJSON logs the raw error).
func transportError(method, path string, err error) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return fmt.Errorf("%s %s: upstream timed out", method, path)
	}
	return fmt.Errorf("%s %s: upstream unreachable", method, path)
}

// extractErrorMessage pulls a human-friendly message from an RFC 9457 Problem
// body, falling back to a truncated raw snippet if parsing fails.
//
// In remote mode the upstream's free-text title/detail and any raw body are
// withheld from the caller-facing error (they can leak internal field names,
// DB messages, or PII) - only the machine-readable Problem `code` and any
// retry hint are surfaced. DoJSON logs the full message (fullErrorMessage)
// for the operator.
func extractErrorMessage(body []byte) string {
	if !remoteMode {
		return fullErrorMessage(body)
	}
	p, parsed := parseProblem(body)
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

type problem struct {
	Title        string `json:"title"`
	Detail       string `json:"detail"`
	Code         string `json:"code"`
	RetryAfterMs int64  `json:"retry_after_ms"`
}

// parseProblem decodes an RFC 9457 Problem body; ok is false when the body is
// not one or carries none of code, title and detail.
func parseProblem(body []byte) (p problem, ok bool) {
	ok = json.Unmarshal(body, &p) == nil && (p.Detail != "" || p.Title != "" || p.Code != "")
	return p, ok
}

// fullErrorMessage is the unredacted message for an upstream error body: the
// Problem code, detail (or title) and retry hint, or a truncated raw snippet.
func fullErrorMessage(body []byte) string {
	if p, parsed := parseProblem(body); parsed {
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
