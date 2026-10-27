package client

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureAuth runs a request through the given client and returns the
// Authorization header the upstream saw.
func captureAuth(t *testing.T, do func(ctx context.Context, base string) error) string {
	t.Helper()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	if err := do(context.Background(), srv.URL); err != nil {
		t.Fatalf("request failed: %v", err)
	}
	return got
}

func TestAPIClient_PrefersContextToken(t *testing.T) {
	got := captureAuth(t, func(ctx context.Context, base string) error {
		c := NewAPIClient(base, "env-token")
		ctx = ContextWithToken(ctx, "per-request-token")
		_, err := c.GetMe(ctx)
		return err
	})
	if got != "Bearer per-request-token" {
		t.Fatalf("Authorization = %q, want per-request token to win", got)
	}
}

func TestAPIClient_FallsBackToEnvToken(t *testing.T) {
	got := captureAuth(t, func(ctx context.Context, base string) error {
		c := NewAPIClient(base, "env-token")
		_, err := c.GetMe(ctx) // no context token
		return err
	})
	if got != "Bearer env-token" {
		t.Fatalf("Authorization = %q, want env token fallback", got)
	}
}

func TestRelayClient_IgnoresContextToken(t *testing.T) {
	got := captureAuth(t, func(ctx context.Context, base string) error {
		c := NewRelayClient(base, "relay-token")
		ctx = ContextWithToken(ctx, "user-hlk-token")
		_, err := c.PostWebhook(ctx, "sonarr", map[string]any{"x": 1})
		return err
	})
	if got != "Bearer relay-token" {
		t.Fatalf("Authorization = %q, relay must never use a per-user context token", got)
	}
}

func TestDoJSON_NoTokenOmitsAuthHeader(t *testing.T) {
	got := captureAuth(t, func(ctx context.Context, base string) error {
		c := NewAPIClient(base, "") // no env token, no context token
		_, err := c.GetMe(ctx)
		return err
	})
	if got != "" {
		t.Fatalf("Authorization = %q, want empty when no token present", got)
	}
}

func TestExtractErrorMessage_RemoteModeRedactsDetail(t *testing.T) {
	SetRemoteMode(true)
	defer SetRemoteMode(false)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"slug_taken","title":"Conflict","detail":"activity slug internal-secret-xyz already exists in shard 7"}`))
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL, "t")
	_, err := c.CreateActivity(context.Background(), CreateActivityInput{Slug: "s", Name: "n"})
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if strings.Contains(msg, "internal-secret-xyz") || strings.Contains(msg, "shard 7") {
		t.Fatalf("remote-mode error leaked upstream detail: %q", msg)
	}
	if !strings.Contains(msg, "slug_taken") {
		t.Fatalf("remote-mode error should retain the machine code, got %q", msg)
	}
}

func TestExtractErrorMessage_LocalModeKeepsDetail(t *testing.T) {
	SetRemoteMode(false)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"slug_taken","detail":"helpful local detail"}`))
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL, "t")
	_, err := c.CreateActivity(context.Background(), CreateActivityInput{Slug: "s", Name: "n"})
	if err == nil || !strings.Contains(err.Error(), "helpful local detail") {
		t.Fatalf("local mode should surface detail, got %v", err)
	}
}

// remoteWithLog turns on remote mode with the withheld-error log captured,
// both undone when the test ends.
func remoteWithLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	SetRemoteMode(true)
	SetLogger(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() {
		SetRemoteMode(false)
		SetLogger(slog.Default())
	})
	return &buf
}

func TestDoJSON_TransportErrorHidesUpstreamAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := srv.URL
	srv.Close() // nothing listens there now, so the dial is refused
	host := strings.TrimPrefix(base, "http://")

	_, _, err := NewBase(base, "t").DoJSON(context.Background(), http.MethodGet, "/auth/me", nil)
	if err == nil || !strings.Contains(err.Error(), host) {
		t.Fatalf("local mode should keep the transport detail, got %v", err)
	}

	logged := remoteWithLog(t)
	_, _, err = NewBase(base, "hlk_never_logged").DoJSON(context.Background(), http.MethodGet, "/auth/me", nil)
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if msg := err.Error(); strings.Contains(msg, host) || msg != "GET /auth/me: upstream unreachable" {
		t.Fatalf("remote-mode transport error = %q, want the operation without the upstream address", msg)
	}
	line := logged.String()
	if !strings.Contains(line, `"level":"WARN"`) || !strings.Contains(line, `"path":"/auth/me"`) || !strings.Contains(line, host) {
		t.Fatalf("the raw transport error should be logged with method and path, got %s", line)
	}
	if strings.Contains(line, "hlk_never_logged") {
		t.Fatalf("credential leaked into the log: %s", line)
	}
}

// headersSeen closes seen once a response's headers are in, so the test server
// can wait for that before it resets the connection mid-body.
type headersSeen struct {
	rt   http.RoundTripper
	seen chan struct{}
}

func (h headersSeen) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := h.rt.RoundTrip(r)
	close(h.seen)
	return resp, err
}

func TestDoJSON_ResponseCutOffHidesUpstreamAddress(t *testing.T) {
	seen := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"activities":[`))
		w.(http.Flusher).Flush()
		<-seen
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.(*net.TCPConn).SetLinger(0) // close with a RST, not a FIN
		_ = conn.Close()
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	logged := remoteWithLog(t)
	b := NewBase(srv.URL, "hlk_never_logged")
	b.httpClient = &http.Client{Transport: headersSeen{rt: &http.Transport{}, seen: seen}}
	_, status, err := b.DoJSON(context.Background(), http.MethodGet, "/activities", nil)
	if err == nil {
		t.Fatal("expected a read error")
	}
	if msg := err.Error(); msg != "GET /activities: upstream response cut off" {
		t.Fatalf("remote-mode read error = %q, want the operation without the connection's addresses", msg)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want the 200 the headers carried", status)
	}
	line := logged.String()
	// The raw error is the reset read, which names both ends of the connection.
	if !strings.Contains(line, "upstream response cut off") || !strings.Contains(line, `"path":"/activities"`) || !strings.Contains(line, host) {
		t.Fatalf("the raw read error should be logged with method and path, got %s", line)
	}
	if strings.Contains(line, "hlk_never_logged") {
		t.Fatalf("credential leaked into the log: %s", line)
	}
}

func TestDoJSON_RemoteModeLogsWithheldDetail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"code":"upstream_down","detail":"apns pool drained"}`))
	}))
	defer srv.Close()

	logged := remoteWithLog(t)
	_, _, err := NewBase(srv.URL, "t").DoJSON(context.Background(), http.MethodGet, "/health", nil)
	if err == nil || strings.Contains(err.Error(), "apns pool drained") {
		t.Fatalf("remote-mode error should withhold the detail, got %v", err)
	}
	if line := logged.String(); !strings.Contains(line, "apns pool drained") || !strings.Contains(line, `"status":502`) {
		t.Fatalf("the withheld detail should be logged for the operator, got %s", line)
	}
}

func TestRelayClient_RejectsBadProvider(t *testing.T) {
	c := NewRelayClient("https://relay.example", "t")
	for _, bad := range []string{"../etc/passwd", "a/b", "Bad Provider", "", "evil.com/path"} {
		if _, err := c.PostWebhook(context.Background(), bad, nil); err == nil {
			t.Fatalf("expected rejection for provider %q", bad)
		}
	}
}
