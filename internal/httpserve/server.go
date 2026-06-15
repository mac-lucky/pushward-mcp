// Package httpserve runs the MCP server over Streamable HTTP for remote
// (http transport) mode. It owns the listener, health/readiness endpoints,
// optional private metrics listener, CORS, and graceful shutdown; the
// authentication (OAuth 2.1 Resource Server + Authorization Server) is provided
// by an Authenticator so this package stays auth-agnostic and testable.
package httpserve

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mac-lucky/pushward-mcp/internal/client"
	"github.com/mac-lucky/pushward-mcp/internal/config"
)

// Authenticator mounts the OAuth discovery/authorize/token routes and guards
// the MCP endpoint. WrapMCP must, for each authorized request, inject the
// per-user upstream token (and user id) into the request context before
// calling next; for unauthorized requests it must write a 401 with an
// appropriate WWW-Authenticate challenge and not call next.
type Authenticator interface {
	RegisterRoutes(mux *http.ServeMux)
	WrapMCP(next http.Handler) http.Handler
}

// Run builds the HTTP server and blocks until ctx is cancelled, then drains
// in-flight requests within a bounded timeout. auth may be nil only in
// development/tests: without it the MCP endpoint extracts the raw inbound
// Bearer token and forwards it upstream unverified (never use in production).
func Run(ctx context.Context, cfg *config.Config, mcp *mcpserver.MCPServer, auth Authenticator, log *slog.Logger) error {
	streamable := mcpserver.NewStreamableHTTPServer(mcp,
		mcpserver.WithStateLess(true),
		mcpserver.WithEndpointPath("/mcp"),
	)

	mux := http.NewServeMux()

	var mcpHandler http.Handler = streamable
	if auth != nil {
		auth.RegisterRoutes(mux)
		mcpHandler = auth.WrapMCP(streamable)
	} else {
		mcpHandler = passthroughBearer(streamable)
	}
	mux.Handle("/mcp", corsMCP(mcpHandler))
	mux.HandleFunc("/health", liveness)
	mux.HandleFunc("/ready", readiness)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           securityHeaders(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0, // streaming responses must not be cut off
		IdleTimeout:       120 * time.Second,
	}

	// Optional pod-private metrics listener.
	var metricsSrv *http.Server
	if cfg.MetricsAddr != "" {
		mmux := http.NewServeMux()
		mmux.HandleFunc("/health", liveness)
		mmux.Handle("/metrics", metricsHandler())
		metricsSrv = &http.Server{Addr: cfg.MetricsAddr, Handler: mmux, ReadHeaderTimeout: 10 * time.Second}
	}

	errCh := make(chan error, 2)
	go func() {
		log.Info("mcp http server listening", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	if metricsSrv != nil {
		go func() {
			log.Info("metrics server listening", "addr", cfg.MetricsAddr)
			if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	}

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received, draining")
	case err := <-errCh:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if metricsSrv != nil {
		_ = metricsSrv.Shutdown(shutdownCtx)
	}
	_ = streamable.Shutdown(shutdownCtx)
	return srv.Shutdown(shutdownCtx)
}

func liveness(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func readiness(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ready"}`))
}

// metricsHandler is a placeholder until a Prometheus registry is wired; it
// keeps the private metrics listener mountable. Replace with promhttp.Handler().
func metricsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte("# metrics not yet wired\n"))
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

// corsMCP applies permissive CORS to the MCP endpoint so browser-originated
// clients can call it; the actual authorization is enforced by WrapMCP.
func corsMCP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, MCP-Protocol-Version, Mcp-Session-Id, Last-Event-ID")
		w.Header().Set("Access-Control-Expose-Headers", "Mcp-Session-Id, WWW-Authenticate")
		// Defeat proxy response buffering on the streaming path. Cloudflare and
		// the Traefik gateway both honor X-Accel-Buffering: no, so Streamable
		// HTTP / SSE chunks reach the client immediately instead of being held
		// at the edge until the buffer fills (which looks like a hung server).
		w.Header().Set("X-Accel-Buffering", "no")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// passthroughBearer is the dev-only fallback when no Authenticator is wired: it
// copies the raw inbound Bearer token into the request context so upstream
// calls carry it. Production always supplies an Authenticator.
func passthroughBearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tok := bearerToken(r); tok != "" {
			r = r.WithContext(client.ContextWithToken(r.Context(), tok))
		}
		next.ServeHTTP(w, r)
	})
}
