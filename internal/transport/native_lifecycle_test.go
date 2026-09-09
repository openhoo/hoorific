package transport

import (
	"context"
	"net/http"
	"net/netip"
	"net/url"
	"testing"

	"hoorific/internal/core"
)

func TestNativePoolMissingHelperFailsClosed(t *testing.T) {
	pool, err := NewPoolWithConfig(core.TransportConfig{NativeEnginePath: "/definitely/missing/hoorific-codex-wire"})
	if err != nil {
		t.Fatal(err)
	}
	policy := NetworkPolicy{
		AllowedHosts: []string{"127.0.0.1"},
		AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		AllowPrivate: true,
		NativeCodex:  true,
		NativeScope:  "test\x00missing",
	}
	if _, err := pool.Client(policy); err == nil {
		t.Fatal("missing native helper was accepted")
	}
	if pool.native == nil {
		t.Fatal("native helper was not supervised")
	}
	if _, err := pool.Client(policy); err == nil {
		t.Fatal("failed native helper was retried as a usable client")
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeAddressPolicyRejectsDeniedCIDRBeforeWire(t *testing.T) {
	u, err := url.Parse("https://127.0.0.1:443/v1/responses")
	if err != nil {
		t.Fatal(err)
	}
	_, err = resolveNativeAddresses(context.Background(), u, NetworkPolicy{
		AllowedHosts: []string{"127.0.0.1"},
		AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
		AllowPrivate: true,
	})
	if err == nil || err.Error() != "egress address denied" {
		t.Fatalf("denied CIDR error = %v", err)
	}
}

func TestPoolScopeIsolatesPortableClients(t *testing.T) {
	pool := NewPool()
	defer pool.Close()
	first, err := pool.Client(NetworkPolicy{AllowedHosts: []string{"example.test"}, NativeScope: "tenant-a\x00connection"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := pool.Client(NetworkPolicy{AllowedHosts: []string{"example.test"}, NativeScope: "tenant-b\x00connection"})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("portable clients shared transport state across scopes")
	}
}

func TestNativeAddressPolicyRejectsMixedDNSAnswer(t *testing.T) {
	_, err := pinNativeAddresses(
		[]netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("10.0.0.1")},
		"443",
		NetworkPolicy{
			AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
			AllowPrivate: true,
		},
	)
	if err == nil || err.Error() != "egress address denied" {
		t.Fatalf("mixed DNS answer error = %v", err)
	}
}

func TestNativeHeadersExcludeProxyAndFramingFields(t *testing.T) {
	headers := nativeHeaders(http.Header{
		"Authorization":       {"Bearer token"},
		"Cookie":              {"private"},
		"Content-Length":      {"3"},
		"Transfer-Encoding":   {"chunked"},
		"Proxy-Authorization": {"secret"},
		"X-Test":              {"ok"},
	})
	for _, pair := range headers {
		if len(pair) != 2 {
			t.Fatalf("invalid header pair: %#v", pair)
		}
		switch pair[0] {
		case "Cookie", "Content-Length", "Transfer-Encoding", "Proxy-Authorization":
			t.Fatalf("forbidden header forwarded: %q", pair[0])
		}
	}
}
