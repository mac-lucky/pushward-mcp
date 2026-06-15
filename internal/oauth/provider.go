package oauth

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/mac-lucky/pushward-mcp/internal/client"
)

// Provider is the OAuth 2.1 Authorization Server + Resource Server for the
// remote MCP endpoint. It satisfies httpserve.Authenticator.
type Provider struct {
	cfg    *Config
	store  Store
	signer *signer
	crypto *hlkCipher
	csrf   *csrfTokenizer
	log    *slog.Logger
	now    func() time.Time
	cred   *credCache

	authorizeLimiter *keyedLimiter
	tokenLimiter     *keyedLimiter
	registerLimiter  *keyedLimiter

	// bgCtx scopes the background janitor to the provider lifetime; cancel stops it.
	bgCtx  context.Context
	cancel context.CancelFunc
}

// New builds a Provider. A Postgres store is used when cfg.DatabaseDSN is set,
// otherwise an in-memory store (single-replica/dev only).
func New(ctx context.Context, cfg *Config, log *slog.Logger) (*Provider, error) {
	cr, err := newHLKCipher(cfg.HLKEncKey)
	if err != nil {
		return nil, err
	}
	sg, err := newSigner(cfg.SigningKeyPEM, cfg.Issuer)
	if err != nil {
		return nil, err
	}
	var st Store
	if cfg.DatabaseDSN != "" {
		st, err = newPostgresStore(ctx, cfg.DatabaseDSN, cfg.DBPasswordFile)
		if err != nil {
			return nil, err
		}
	} else {
		log.Warn("oauth using in-memory store; tokens are not shared across replicas and are lost on restart")
		st = newMemoryStore()
	}
	bgCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	p := &Provider{
		cfg:              cfg,
		store:            st,
		signer:           sg,
		crypto:           cr,
		csrf:             newCSRFTokenizer(cr.master, csrfTokenTTL, time.Now),
		log:              log,
		now:              time.Now,
		cred:             newCredCache(credCacheTTL, credCacheMax, time.Now),
		authorizeLimiter: newKeyedLimiter(60, 20),
		tokenLimiter:     newKeyedLimiter(120, 30),
		registerLimiter:  newKeyedLimiter(10, 5),
		bgCtx:            bgCtx,
		cancel:           cancel,
	}
	go p.janitor()
	return p, nil
}

// RegisterRoutes mounts the discovery, authorize, token and registration
// endpoints on mux.
func (p *Provider) RegisterRoutes(mux *http.ServeMux) {
	prm := p.prmHandler()
	mux.Handle(p.cfg.prmPath(), prm)
	mux.Handle(p.cfg.prmPath()+"/mcp", prm) // path-scoped variant some clients probe first
	mux.Handle(p.cfg.asPath(), p.asMetadataHandler())
	mux.Handle(p.cfg.jwksPath(), p.jwksHandler())
	mux.HandleFunc("/oauth/authorize", p.handleAuthorize) // top-level browser nav + form; not a CORS fetch
	mux.Handle("/oauth/token", corsPOST(p.handleToken))
	mux.Handle("/oauth/register", corsPOST(p.handleRegister))
}

// WrapMCP guards the MCP endpoint: it requires a valid access token issued by
// this server, loads the user's encrypted PushWard key, and injects it (plus
// the user id, for audit) into the request context for the tool handlers.
func (p *Provider) WrapMCP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := client.ParseBearer(r.Header.Get("Authorization"))
		if token == "" {
			p.challenge(w, "missing access token")
			return
		}
		claims, err := p.signer.Verify(token, p.cfg.Resource)
		if err != nil {
			p.challenge(w, "invalid access token")
			return
		}
		userID := claims.Subject
		hlk, err := p.loadHLK(r.Context(), userID)
		if err != nil {
			p.challenge(w, "credential unavailable; re-authorization required")
			return
		}
		ctx := client.ContextWithToken(r.Context(), hlk)
		ctx = client.ContextWithUserID(ctx, userID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// loadHLK returns the user's decrypted PushWard key, serving from a short-TTL
// cache so the hot /mcp path skips a DB read plus a key derivation on every
// JSON-RPC call. A cache miss reads and decrypts once, then caches.
func (p *Provider) loadHLK(ctx context.Context, userID string) (string, error) {
	if hlk, ok := p.cred.get(userID); ok {
		return hlk, nil
	}
	blob, err := p.store.GetUserCredential(ctx, userID)
	if err != nil {
		return "", err
	}
	hlk, err := p.crypto.Decrypt(userID, blob)
	if err != nil {
		// Don't cache a failure; log without the credential.
		p.log.Error("hlk decrypt failed", "user", userID)
		p.cred.invalidate(userID)
		return "", err
	}
	p.cred.put(userID, hlk)
	return hlk, nil
}

// janitor periodically purges expired store rows and sweeps the credential
// cache. It exits when the Provider is closed.
func (p *Provider) janitor() {
	t := time.NewTicker(janitorInterval)
	defer t.Stop()
	for {
		select {
		case <-p.bgCtx.Done():
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(p.bgCtx, 30*time.Second)
			if err := p.store.Cleanup(ctx); err != nil {
				p.log.Warn("oauth store cleanup failed", "err", err)
			}
			cancel()
			p.cred.sweep()
		}
	}
}

// challenge writes a 401 with an RFC 9728 resource_metadata pointer so clients
// can discover how to authenticate.
func (p *Provider) challenge(w http.ResponseWriter, desc string) {
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(
		`Bearer resource_metadata=%q, error="invalid_token", error_description=%q`,
		p.cfg.Issuer+p.cfg.prmPath(), desc))
	oauthError(w, http.StatusUnauthorized, "invalid_token", desc)
}

// Close stops the janitor and releases the store. cancel is idempotent, so
// calling Close more than once is safe.
func (p *Provider) Close() {
	p.cancel()
	p.store.Close()
}

// setClock overrides the time source on the provider, its credential cache, and
// (when in-memory) its store, so tests can simulate token expiry and the
// refresh-rotation grace window. Test-only.
func (p *Provider) setClock(now func() time.Time) {
	p.now = now
	p.cred.now = now
	p.csrf.now = now
	if m, ok := p.store.(*memoryStore); ok {
		m.now = now
	}
}
