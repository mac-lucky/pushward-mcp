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

// Bounds on attacker-controlled client metadata, so a flood of DCR registrations
// or CIMD fetches cannot inflate stored rows (each row is otherwise capped only by
// the request/document body limit).
const (
	maxClientNameLen  = 256
	maxRedirectURIs   = 8
	maxRedirectURILen = 2048
)

// validateClientMetadata bounds and validates the redirect_uris and client_name a
// client supplies (DCR body or fetched CIMD document). It returns the (possibly
// truncated) name to store and an error if the redirect set is unacceptable.
func validateClientMetadata(name string, uris []string) (string, error) {
	if len(uris) == 0 {
		return "", fmt.Errorf("redirect_uris is required")
	}
	if len(uris) > maxRedirectURIs {
		return "", fmt.Errorf("too many redirect_uris (max %d)", maxRedirectURIs)
	}
	for _, ru := range uris {
		if len(ru) > maxRedirectURILen || !validRedirectURI(ru) {
			return "", fmt.Errorf("redirect_uri must be https or loopback http and at most %d bytes", maxRedirectURILen)
		}
	}
	if len(name) > maxClientNameLen {
		name = name[:maxClientNameLen]
	}
	return name, nil
}

// handleRegister implements RFC 7591 Dynamic Client Registration (the DCR
// fallback for clients that don't use CIMD).
func (p *Provider) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		oauthError(w, http.StatusMethodNotAllowed, "invalid_request", "POST required")
		return
	}
	if !p.registerLimiter.Allow(p.clientIP(r)) {
		oauthError(w, http.StatusTooManyRequests, "temporarily_unavailable", "rate limited")
		return
	}
	var req dcrRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "invalid JSON body")
		return
	}
	name, err := validateClientMetadata(req.ClientName, req.RedirectURIs)
	if err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", err.Error())
		return
	}
	id, err := randomToken(24)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not generate client id")
		return
	}
	clientID := "dcr_" + id
	c := &Client{ID: clientID, Name: name, RedirectURIs: req.RedirectURIs, CreatedAt: time.Now()}
	if err := p.store.SaveClient(r.Context(), c); err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not persist client")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  clientID,
		"redirect_uris":              req.RedirectURIs,
		"grant_types":                []string{grantTypeAuthorizationCode, grantTypeRefreshToken},
		"token_endpoint_auth_method": authMethodNone,
		"client_name":                name,
	})
}

// resolveClient returns the Client for a client_id presented at /authorize.
// A client_id that is an https URL is treated as a Client ID Metadata Document
// (CIMD): the document is fetched (SSRF-guarded), validated, and cached. Any
// other client_id is looked up in the store (DCR-registered).
func (p *Provider) resolveClient(ctx context.Context, clientID string) (*Client, error) {
	if strings.HasPrefix(clientID, "https://") {
		if c, err := p.store.GetClient(ctx, clientID); err == nil {
			// Re-fetch a stale CIMD document so a rotated/removed redirect_uri stops
			// being honored (and a newly added one starts working). If the re-fetch
			// fails, fall back to the cached copy rather than breaking the flow.
			if c.IsCIMD && p.now().Sub(c.UpdatedAt) > cimdCacheTTL {
				if fresh, ferr := p.fetchCIMD(ctx, clientID); ferr == nil {
					return fresh, nil
				}
			}
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
	name, err := validateClientMetadata(doc.ClientName, doc.RedirectURIs)
	if err != nil {
		return nil, fmt.Errorf("client metadata: %w", err)
	}
	c := &Client{ID: clientID, Name: name, RedirectURIs: doc.RedirectURIs, IsCIMD: true, CreatedAt: time.Now()}
	_ = p.store.SaveClient(ctx, c) // cache; best-effort
	return c, nil
}

// safeGet performs an SSRF-guarded HTTPS GET: only https, and the resolved IPs
// must all be public (no loopback, private, link-local, or ULA ranges - which
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
				// Allowlist posture: every resolved IP must be a public global-unicast
				// address that is not on the SSRF denylist. Requiring IsGlobalUnicast
				// rejects the odd ranges the denylist might miss; isBlockedIP also
				// de-embeds IPv6 transition forms that hide an internal IPv4.
				if isBlockedIP(ip.IP) || !ip.IP.IsGlobalUnicast() {
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
// network live here), IETF protocol/benchmark/documentation assignments, and the
// IPv6 transition prefixes (NAT64/6to4/Teredo) whose addresses can tunnel to an
// internal IPv4. net.IP.IsPrivate already covers RFC 1918 and IPv6 ULA (fc00::/7).
var blockedCIDRs = func() []*net.IPNet {
	out := make([]*net.IPNet, 0, 11)
	for _, c := range []string{
		"100.64.0.0/10",   // RFC 6598 CGNAT
		"192.0.0.0/24",    // RFC 6890 IETF protocol assignments
		"198.18.0.0/15",   // RFC 2544 benchmarking
		"192.0.2.0/24",    // TEST-NET-1
		"198.51.100.0/24", // TEST-NET-2
		"203.0.113.0/24",  // TEST-NET-3
		"64:ff9b::/96",    // RFC 6052 NAT64 well-known prefix
		"64:ff9b:1::/48",  // RFC 8215 NAT64 local-use prefix
		"2002::/16",       // RFC 3056 6to4
		"2001::/32",       // RFC 4380 Teredo
		"2001:db8::/32",   // RFC 3849 documentation
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
	if isBlockedIPBasic(ip) {
		return true
	}
	// IPv6 transition forms (NAT64, 6to4, IPv4-compatible) can embed an internal
	// IPv4 that the net.IP helpers miss because they only de-map the ::ffff: form.
	// Extract any embedded IPv4 and re-check it.
	if v4 := embeddedIPv4(ip); v4 != nil && isBlockedIPBasic(v4) {
		return true
	}
	return false
}

func isBlockedIPBasic(ip net.IP) bool {
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

// embeddedIPv4 returns the IPv4 address embedded in an IPv6 transition address
// (NAT64 64:ff9b::/96, 6to4 2002::/16, or the IPv4-compatible ::a.b.c.d form), or
// nil when ip carries no embedded IPv4. The ::ffff: mapped form is handled
// natively by the net.IP helpers (via To4), so it is intentionally skipped here.
func embeddedIPv4(ip net.IP) net.IP {
	ip16 := ip.To16()
	if ip16 == nil || ip.To4() != nil {
		return nil
	}
	switch {
	case allZero(ip16[:10]): // ::a.b.c.d (IPv4-compatible)
		return net.IPv4(ip16[12], ip16[13], ip16[14], ip16[15]).To4()
	case ip16[0] == 0x00 && ip16[1] == 0x64 && ip16[2] == 0xff && ip16[3] == 0x9b && allZero(ip16[4:12]):
		return net.IPv4(ip16[12], ip16[13], ip16[14], ip16[15]).To4() // NAT64 64:ff9b::/96
	case ip16[0] == 0x20 && ip16[1] == 0x02:
		return net.IPv4(ip16[2], ip16[3], ip16[4], ip16[5]).To4() // 6to4 2002:V4::/48
	}
	return nil
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

// clientIP returns the rate-limit key. The forwarding headers (CF-Connecting-IP,
// then the left-most X-Forwarded-For hop) are honored ONLY when TrustProxy is set
// AND the immediate peer is a trusted proxy (see peerTrusted) - otherwise a
// directly-connecting client could forge them to mint a fresh bucket per request.
// When not trusted, the peer's RemoteAddr is used.
func (p *Provider) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if p.cfg.TrustProxy && p.peerTrusted(host) {
		if cf := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); cf != "" {
			return cf
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if first, _, _ := strings.Cut(xff, ","); strings.TrimSpace(first) != "" {
				return strings.TrimSpace(first)
			}
		}
	}
	return host
}

// peerTrusted reports whether the immediate connection peer (RemoteAddr) is
// allowed to set the forwarding headers. With TrustedProxyCIDRs configured the
// peer must fall inside one of them; otherwise the default is the in-cluster proxy
// tier (loopback / RFC1918 private / CGNAT / link-local), which is where the
// Traefik gateway connects from - so a directly-connecting public client is never
// trusted to assert its own forwarded IP.
func (p *Provider) peerTrusted(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if len(p.cfg.TrustedProxyCIDRs) > 0 {
		for _, n := range p.cfg.TrustedProxyCIDRs {
			if n.Contains(ip) {
				return true
			}
		}
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || cgnatNet.Contains(ip)
}

var cgnatNet = func() *net.IPNet {
	_, n, _ := net.ParseCIDR("100.64.0.0/10")
	return n
}()
