package httpserve

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"
)

func TestSecurityHeaders(t *testing.T) {
	h := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/mcp", nil))

	if got := rr.Header().Get("Strict-Transport-Security"); !strings.Contains(got, "max-age=") {
		t.Errorf("missing HSTS header, got %q", got)
	}
	if got := rr.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

func TestCORSMCPPreflightShortCircuits(t *testing.T) {
	called := false
	h := corsMCP(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodOptions, "/mcp", nil))

	if called {
		t.Error("OPTIONS preflight must not reach the wrapped handler")
	}
	if rr.Code != http.StatusNoContent {
		t.Errorf("preflight status = %d, want %d", rr.Code, http.StatusNoContent)
	}
	if rr.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("preflight response missing CORS origin header")
	}
}

func TestCORSMCPDisablesProxyBuffering(t *testing.T) {
	// Cloudflare and the Traefik gateway buffer streaming responses unless this
	// header is present; losing it presents as a hung server to remote clients.
	h := corsMCP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/mcp", nil))

	if got := rr.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", got)
	}
}

func TestLimitBodyCapsReads(t *testing.T) {
	var readErr error
	h := limitBody(8, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(strings.Repeat("x", 64))))

	if readErr == nil {
		t.Fatal("reading an oversized body should fail")
	}
	var mbe *http.MaxBytesError
	if !errors.As(readErr, &mbe) {
		t.Errorf("read error = %v, want *http.MaxBytesError", readErr)
	}
}

func TestHealthEndpoints(t *testing.T) {
	for name, fn := range map[string]http.HandlerFunc{"liveness": liveness, "readiness": readiness} {
		rr := httptest.NewRecorder()
		fn(rr, httptest.NewRequest(http.MethodGet, "/health", nil))
		if rr.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200", name, rr.Code)
		}
		if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s content type = %q, want application/json", name, ct)
		}
	}
}

func TestLimitListenerBoundsConcurrentConns(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := newLimitListener(inner, 1)
	defer ln.Close()

	accepted := make(chan net.Conn, 2)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()

	dial := func() net.Conn {
		c, err := net.Dial("tcp", inner.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c1 := dial()
	defer c1.Close()
	first := <-accepted

	// With the single slot held, a second dial connects at the TCP level but
	// must not be surfaced by Accept until the first conn releases its slot.
	c2 := dial()
	defer c2.Close()
	select {
	case <-accepted:
		t.Fatal("second conn accepted while the only slot was held")
	case <-time.After(100 * time.Millisecond):
	}

	first.Close()
	select {
	case c := <-accepted:
		c.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("second conn never accepted after the slot was released")
	}
}

func TestStreamableRefusesStandaloneGET(t *testing.T) {
	h := newStreamable(mcpserver.NewMCPServer("test", "1.0.0"))

	// Without the fix this GET opens a stream; the deadline keeps a regression
	// from hanging the test.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r := httptest.NewRequest(http.MethodGet, "/mcp", nil).WithContext(ctx)
	r.Header.Set("Accept", "text/event-stream")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("standalone GET /mcp = %d, want 405", rr.Code)
	}

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
	r = httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST initialize = %d %s, want 200", rr.Code, rr.Body.String())
	}
}
