package oauth

import (
	"net"
	"net/url"
)

// redirectURIAllowed reports whether a request's redirect_uri matches one of a
// client's registered URIs. Matching is byte-exact, except that loopback URIs
// (http://localhost or http://127.0.0.1 / [::1]) match port-agnostically per
// RFC 8252 section 7.3 - native clients like Claude Code bind an ephemeral port.
func redirectURIAllowed(registered []string, candidate string) bool {
	for _, r := range registered {
		if r == candidate {
			return true
		}
		if loopbackRedirectMatch(r, candidate) {
			return true
		}
	}
	return false
}

func loopbackRedirectMatch(registered, candidate string) bool {
	ru, err := url.Parse(registered)
	if err != nil {
		return false
	}
	cu, err := url.Parse(candidate)
	if err != nil {
		return false
	}
	if !isLoopback(ru.Hostname()) || !isLoopback(cu.Hostname()) {
		return false
	}
	// Schemes and paths must still match exactly; only the port is ignored.
	return ru.Scheme == cu.Scheme && ru.Path == cu.Path && ru.Hostname() == cu.Hostname()
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// validRedirectURI reports whether a registered redirect URI is acceptable:
// https for remote hosts, http only for loopback.
func validRedirectURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		return isLoopback(u.Hostname())
	default:
		return false
	}
}
