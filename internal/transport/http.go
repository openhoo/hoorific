package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"hoorific/internal/core"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

type NetworkPolicy struct {
	AllowedHosts                          []string
	AllowedCIDRs                          []netip.Prefix
	AllowPrivate, AllowSameOriginRedirect bool
	DisableCompression                    bool
	NativeCodex                           bool
	NativeScope                           string
}
type Pool struct {
	mu               sync.Mutex
	clients          map[string]*http.Client
	maxConnsPerHost  int
	nativeEnginePath string
	nativeMu         sync.Mutex
	native           *nativeEngine
	closed           bool
}

func NewPool() *Pool {
	return &Pool{clients: make(map[string]*http.Client), maxConnsPerHost: core.DefaultMaxConnsPerHost}
}

func NewPoolWithConfig(config core.TransportConfig) (*Pool, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &Pool{clients: make(map[string]*http.Client), maxConnsPerHost: config.ConnectionLimit(), nativeEnginePath: config.NativeEnginePath}, nil
}
func (p *Pool) Client(policy NetworkPolicy) (*http.Client, error) {
	hosts := append([]string(nil), policy.AllowedHosts...)
	sort.Strings(hosts)
	cidrs := make([]string, len(policy.AllowedCIDRs))
	for i, c := range policy.AllowedCIDRs {
		if !c.IsValid() {
			return nil, errors.New("invalid egress CIDR")
		}
		cidrs[i] = c.String()
	}
	sort.Strings(cidrs)
	key := strings.Join(hosts, "\x00") + "|" + strings.Join(cidrs, ",")
	if policy.AllowPrivate {
		key += "|private"
	}
	if policy.AllowSameOriginRedirect {
		key += "|redirect"
	}
	if policy.DisableCompression {
		key += "|no-compression"
	}
	if policy.NativeCodex {
		if policy.NativeScope == "" {
			return nil, errors.New("native transport requires an isolated scope")
		}
		key += "|native\x00" + policy.NativeScope
	} else {
		key += "|portable\x00" + policy.NativeScope
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errors.New("transport pool is closed")
	}
	if c := p.clients[key]; c != nil {
		enginePath := p.nativeEnginePath
		p.mu.Unlock()
		if policy.NativeCodex {
			if _, err := p.ensureNative(enginePath); err != nil {
				return nil, err
			}
		}
		return c, nil
	}
	maxConns := p.maxConnsPerHost
	enginePath := p.nativeEnginePath
	p.mu.Unlock()

	var engine *nativeEngine
	var err error
	if policy.NativeCodex {
		engine, err = p.ensureNative(enginePath)
		if err != nil {
			return nil, err
		}
	}

	policy.AllowedHosts = hosts
	policy.AllowedCIDRs = append([]netip.Prefix(nil), policy.AllowedCIDRs...)
	var c *http.Client
	if policy.NativeCodex {
		rt := newNativeRoundTripper(engine, policy, key, maxConns)
		c = &http.Client{Transport: rt, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
	} else {
		dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		t := &http.Transport{Proxy: nil, ForceAttemptHTTP2: true, DisableCompression: policy.DisableCompression, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 120 * time.Second, IdleConnTimeout: 90 * time.Second, MaxIdleConns: 256, MaxIdleConnsPerHost: 64, MaxConnsPerHost: maxConns}
		t.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			if len(policy.AllowedHosts) > 0 {
				ok := false
				for _, h := range policy.AllowedHosts {
					if strings.EqualFold(strings.TrimSuffix(host, "."), strings.TrimSuffix(h, ".")) {
						ok = true
						break
					}
				}
				if !ok {
					return nil, errors.New("egress host denied")
				}
			}
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			if len(ips) == 0 {
				return nil, errors.New("egress DNS returned no addresses")
			}
			// Reject the entire mixed DNS answer, rather than fail over to a forbidden address.
			for _, ip := range ips {
				if !AllowedAddress(ip, policy) {
					return nil, errors.New("egress address denied")
				}
			}
			var last error
			for _, ip := range ips {
				c, e := dialer.DialContext(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
				if e == nil {
					return c, nil
				}
				last = e
			}
			return nil, last
		}
		c = &http.Client{Transport: t, CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if !policy.AllowSameOriginRedirect || len(via) == 0 {
				return http.ErrUseLastResponse
			}
			if len(via) >= 5 {
				return errors.New("redirect limit exceeded")
			}
			if !SameOrigin(req.URL, via[0].URL) {
				return errors.New("cross-origin redirect denied")
			}
			return nil
		}}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		if policy.NativeCodex {
			_ = engine.Close()
		}
		return nil, errors.New("transport pool is closed")
	}
	if existing := p.clients[key]; existing != nil {
		return existing, nil
	}
	p.clients[key] = c
	return c, nil
}
func SameOrigin(a, b *url.URL) bool {
	return a != nil && b != nil && strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}
func AllowedAddress(ip netip.Addr, p NetworkPolicy) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	private := ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || !ip.IsGlobalUnicast() || netip.MustParsePrefix("100.64.0.0/10").Contains(ip)
	explicit := false
	for _, c := range p.AllowedCIDRs {
		if c.Contains(ip) {
			explicit = true
			break
		}
	}
	if len(p.AllowedCIDRs) > 0 && !explicit {
		return false
	}
	if private {
		return p.AllowPrivate && explicit
	}
	return true
}
func (p *Pool) CloseIdleConnections() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.clients {
		c.CloseIdleConnections()
	}
}

// Close prevents new clients, closes idle and active native work, and
// supervises the native helper until it exits.
func (p *Pool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	clients := make([]*http.Client, 0, len(p.clients))
	for _, c := range p.clients {
		clients = append(clients, c)
	}
	p.mu.Unlock()
	for _, c := range clients {
		c.CloseIdleConnections()
	}
	p.nativeMu.Lock()
	engine := p.native
	p.nativeMu.Unlock()
	if engine == nil {
		return nil
	}
	return engine.Close()
}
func stripHop(h http.Header) {
	for _, line := range h.Values("Connection") {
		for _, n := range strings.Split(line, ",") {
			h.Del(strings.TrimSpace(n))
		}
	}
	for _, n := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(n)
	}
}

// SanitizeRequestHeaders must run before credential injection or signing.
func SanitizeRequestHeaders(h http.Header) {
	stripHop(h)
	for n := range h {
		l := strings.ToLower(n)
		if l == "authorization" || l == "x-api-key" || l == "x-goog-api-key" || l == "api-key" || l == "cookie" || l == "host" || l == "forwarded" || strings.HasPrefix(l, "x-forwarded-") || strings.HasPrefix(l, "x-hoorific-") {
			h.Del(n)
		}
	}
}

// SanitizeResponseHeaders is the authoritative boundary for relayed upstream
// responses. Upstream bytes are data: they must never execute as an active
// document on a Hoorific origin, including when a user agent navigates a relay
// URL directly. Active content types are neutralized, sniffing is disabled, and
// a sandboxing CSP keeps any remaining document inert. Header choices made here
// are captured by response caches such as idempotency replay, so callers must
// not rely on setting these headers earlier in the dispatch chain.
func SanitizeResponseHeaders(h http.Header) {
	stripHop(h)
	h.Del("Set-Cookie")
	h.Del("Server")
	h.Del("Clear-Site-Data")
	h.Del("Refresh")
	h.Del("Content-Security-Policy-Report-Only")
	if activeContentType(h.Get("Content-Type")) {
		h.Set("Content-Type", "text/plain; charset=utf-8")
	}
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox")
}

// activeContentType reports whether the media type renders as an executable
// or script-bearing document in browsers. Unknown types stay untouched; the
// sandboxing CSP above already keeps any rendered document inert.
func activeContentType(contentType string) bool {
	media := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	switch media {
	case "text/html", "application/xhtml+xml", "image/svg+xml", "text/xml", "application/xml":
		return true
	}
	return false
}
func SanitizeRequest(req *http.Request) {
	if req == nil {
		return
	}
	req.Host = ""
	SanitizeRequestHeaders(req.Header)
}
