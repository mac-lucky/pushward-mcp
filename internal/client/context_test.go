package client

import (
	"context"
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

func TestRelayClient_RejectsBadProvider(t *testing.T) {
	c := NewRelayClient("https://relay.example", "t")
	for _, bad := range []string{"../etc/passwd", "a/b", "Bad Provider", "", "evil.com/path"} {
		if _, err := c.PostWebhook(context.Background(), bad, nil); err == nil {
			t.Fatalf("expected rejection for provider %q", bad)
		}
	}
}
