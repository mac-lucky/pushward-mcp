package httpserve

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mac-lucky/pushward-mcp/internal/client"
	"github.com/mac-lucky/pushward-mcp/internal/tools"
)

func TestSingleUserInjectsConfiguredToken(t *testing.T) {
	var got, user string
	h := SingleUser{Token: "hlk_owner"}.WrapMCP(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = client.TokenFromContext(r.Context())
		user = client.UserIDFromContext(r.Context())
		if r.Header.Get("Authorization") != "" {
			t.Error("inbound Authorization header must not reach the MCP handler")
		}
	}))
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	r.Header.Set("Authorization", "Bearer hlk_intruder")
	h.ServeHTTP(httptest.NewRecorder(), r)

	if got != "hlk_owner" {
		t.Errorf("context token = %q, want hlk_owner", got)
	}
	if user != singleUserID {
		t.Errorf("context user = %q, want %q", user, singleUserID)
	}
	if r.Header.Get("Authorization") == "" {
		t.Error("WrapMCP must not modify the caller's request")
	}
}

func TestSingleUserRejectsBrowserRequests(t *testing.T) {
	called := false
	h := SingleUser{Token: "hlk_owner"}.WrapMCP(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	r.Header.Set("Origin", "https://evil.example")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)

	if called {
		t.Error("a request with an Origin header must not reach the MCP handler")
	}
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rr.Code)
	}
}

// A tool call through the single-user endpoint reaches the upstream API as the
// configured key, whatever bearer the caller sends and with none at all.
func TestSingleUserToolCallUsesConfiguredToken(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":1}`)
	}))
	defer upstream.Close()

	mcp := mcpserver.NewMCPServer("pushward-test", "0.0.0")
	// No client token, as in main.go: the key can only arrive through WrapMCP.
	tools.RegisterAll(mcp, client.NewAPIClient(upstream.URL, ""), nil)
	streamable := mcpserver.NewStreamableHTTPServer(mcp, mcpserver.WithStateLess(true))
	srv := httptest.NewServer(SingleUser{Token: "hlk_owner"}.WrapMCP(streamable))
	defer srv.Close()

	for _, inbound := range []string{"", "Bearer hlk_intruder"} {
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/mcp",
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_me","arguments":{}}}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if inbound != "" {
			req.Header.Set("Authorization", inbound)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || strings.Contains(string(body), `"isError":true`) {
			t.Fatalf("inbound %q: status %d, body %s", inbound, resp.StatusCode, body)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("upstream saw %d requests, want 2", len(seen))
	}
	for i, h := range seen {
		if h != "Bearer hlk_owner" {
			t.Errorf("upstream request %d Authorization = %q, want the configured key", i, h)
		}
	}
}
