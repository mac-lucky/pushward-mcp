package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// dcrRequest is the subset of RFC 7591 client metadata we accept.
type dcrRequest struct {
	RedirectURIs            []string `json:"redirect_uris"`
	ClientName              string   `json:"client_name"`
	GrantTypes              []string `json:"grant_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

// handleRegister implements RFC 7591 Dynamic Client Registration (the DCR
// fallback for clients that don't use CIMD).
func (p *Provider) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		oauthError(w, http.StatusMethodNotAllowed, "invalid_request", "POST required")
		return
	}
	if !p.registerLimiter.Allow(clientIP(r, p.cfg.TrustProxy)) {
		oauthError(w, http.StatusTooManyRequests, "temporarily_unavailable", "rate limited")
		return
	}
	var req dcrRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "invalid JSON body")
		return
	}
	if len(req.RedirectURIs) == 0 {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris is required")
		return
	}
	for _, ru := range req.RedirectURIs {
		if !validRedirectURI(ru) {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uri must be https or loopback http")
			return
		}
	}
	id, err := randomToken(24)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not generate client id")
		return
	}
	clientID := "dcr_" + id
	c := &Client{ID: clientID, Name: req.ClientName, RedirectURIs: req.RedirectURIs, CreatedAt: time.Now()}
	if err := p.store.SaveClient(r.Context(), c); err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not persist client")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  clientID,
		"redirect_uris":              req.RedirectURIs,
		"grant_types":                []string{grantTypeAuthorizationCode, grantTypeRefreshToken},
		"token_endpoint_auth_method": authMethodNone,
		"client_name":                req.ClientName,
	})
}

// resolveClient returns the Client for a client_id presented at /authorize.
// A client_id that is an https URL is treated as a Client ID Metadata Document
// (CIMD): the document is fetched (SSRF-guarded), validated, and cached. Any
// other client_id is looked up in the store (DCR-registered).
func (p *Provider) resolveClient(ctx context.Context, clientID string) (*Client, error) {
	if strings.HasPrefix(clientID, "https://") {
		if c, err := p.store.GetClient(ctx, clientID); err == nil {
			return c, nil
		}
		return p.fetchCIMD(ctx, clientID)
	}
	return p.store.GetClient(ctx, clientID)
}

// cimdDoc is the subset of a Client ID Metadata Document we consume.
type cimdDoc struct {
	ClientID     string   `json:"client_id"`
	ClientName   string   `json:"client_name"`
	RedirectURIs []string `json:"redirect_uris"`
}

func (p *Provider) fetchCIMD(ctx context.Context, clientID string) (*Client, error) {
	body, err := safeGet(ctx, clientID)
	if err != nil {
		return nil, fmt.Errorf("fetch client metadata: %w", err)
	}
	var doc cimdDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("parse client metadata: %w", err)
	}
	// The document's client_id MUST equal the URL it was fetched from.
	if doc.ClientID != clientID {
		return nil, fmt.Errorf("client metadata client_id mismatch")
	}
	if len(doc.RedirectURIs) == 0 {
		return nil, fmt.Errorf("client metadata has no redirect_uris")
	}
	for _, ru := range doc.RedirectURIs {
		if !validRedirectURI(ru) {
			return nil, fmt.Errorf("client metadata has invalid redirect_uri")
		}
	}
	c := &Client{ID: clientID, Name: doc.ClientName, RedirectURIs: doc.RedirectURIs, IsCIMD: true, CreatedAt: time.Now()}
	_ = p.store.SaveClient(ctx, c) // cache; best-effort
	return c, nil
}

// safeGet performs an SSRF-guarded HTTPS GET: only https, and the resolved IPs
// must all be public (no loopback, private, link-local, or ULA ranges — which
// blocks cloud metadata endpoints like 169.254.169.254).
func safeGet(ctx context.Context, rawurl string) ([]byte, error) {
	u, err := url.Parse(rawurl)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("url must be https with a host")
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			var dialIP net.IP
			for _, ip := range ips {
				if isBlockedIP(ip.IP) {
					return nil, fmt.Errorf("blocked address %s", ip.IP)
				}
				if dialIP == nil {
					dialIP = ip.IP
				}
			}
			if dialIP == nil {
				return nil, fmt.Errorf("no address for host")
			}
			d := &net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, network, net.JoinHostPort(dialIP.String(), port))
		},
		TLSHandshakeTimeout: 5 * time.Second,
	}
	hc := &http.Client{
		Transport: transport,
		Timeout:   8 * time.Second,
		// The DialContext above re-validates the IP on every hop (so a redirect to
		// an internal address is still blocked), but also forbid scheme downgrade
		// and cap the redirect chain.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return fmt.Errorf("too many redirects")
			}
			if req.URL.Scheme != "https" {
				return fmt.Errorf("redirect to non-https blocked")
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metadata fetch returned %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<10))
}

// blockedCIDRs are ranges not covered by the net.IP.IsX helpers that must never
// be reachable via a CIMD fetch: carrier-grade NAT (Tailscale and the OKE pod
// network live here) and IETF protocol/benchmark/documentation assignments.
// net.IP.IsPrivate already covers RFC 1918 and IPv6 ULA (fc00::/7).
var blockedCIDRs = func() []*net.IPNet {
	out := make([]*net.IPNet, 0, 6)
	for _, c := range []string{
		"100.64.0.0/10",   // RFC 6598 CGNAT
		"192.0.0.0/24",    // RFC 6890 IETF protocol assignments
		"198.18.0.0/15",   // RFC 2544 benchmarking
		"192.0.2.0/24",    // TEST-NET-1
		"198.51.100.0/24", // TEST-NET-2
		"203.0.113.0/24",  // TEST-NET-3
	} {
		if _, n, err := net.ParseCIDR(c); err == nil {
			out = append(out, n)
		}
	}
	return out
}()

func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	for _, n := range blockedCIDRs {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIP returns the rate-limit key. With trustProxy set (the server runs
// behind Cloudflare + the Traefik gateway, which overwrite these headers), the
// real client is CF-Connecting-IP, falling back to the left-most X-Forwarded-For
// hop. With trustProxy off, ONLY RemoteAddr is used — the forwarding headers are
// client-forgeable and would let an attacker mint a fresh bucket per request.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if cf := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); cf != "" {
			return cf
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if first, _, _ := strings.Cut(xff, ","); strings.TrimSpace(first) != "" {
				return strings.TrimSpace(first)
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
