package gateway

import (
	"net/http"
	"net/url"
	"strings"

	"hoorific/internal/core"
)

// originAllowed applies the gateway's browser-origin policy. Requests without
// Origin are non-browser callers and remain allowed; a supplied Origin must be
// a single valid origin and either match the configured gateway origin or an
// explicitly configured tenant origin.
func (g *Gateway) originAllowed(r *http.Request, policy core.TenantPolicy) bool {
	if r == nil {
		return false
	}
	values := r.Header.Values("Origin")
	if len(values) == 0 {
		return true
	}
	if len(values) != 1 {
		return false
	}
	origin, ok := canonicalOrigin(values[0])
	if !ok {
		return false
	}
	if configured, ok := configuredOrigin(g.deps.PublicURL); ok && origin == configured {
		return true
	}
	if g.deps.PublicURL == "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		if local, ok := canonicalOrigin(scheme + "://" + r.Host); ok && origin == local {
			return true
		}
	}
	for _, allowed := range policy.AllowedOrigins {
		if candidate, valid := canonicalOrigin(allowed); valid && origin == candidate {
			return true
		}
	}
	return false
}

func canonicalOrigin(raw string) (string, bool) {
	if raw == "" || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "\r\n\t ,") {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || u.Opaque != "" || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", false
	}
	port := u.Port()
	if port != "" {
		if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
			port = ""
		}
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host, true
}
func configuredOrigin(raw string) (string, bool) {
	if raw == "" || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "\r\n\t ,") {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return "", false
	}
	return canonicalOrigin(u.Scheme + "://" + u.Host)
}
