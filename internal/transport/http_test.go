package transport

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"testing"
)

func TestPoolCompressionPolicyControlsAcceptEncodingAndCacheIsolation(t *testing.T) {
	received := make(chan string, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("Accept-Encoding")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	base := NetworkPolicy{
		AllowedHosts:       []string{u.Hostname()},
		AllowedCIDRs:       []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		AllowPrivate:       true,
		DisableCompression: false,
	}
	pool := NewPool()
	enabled, err := pool.Client(base)
	if err != nil {
		t.Fatal(err)
	}
	disabledPolicy := base
	disabledPolicy.DisableCompression = true
	disabled, err := pool.Client(disabledPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if enabled == disabled {
		t.Fatal("compression policy reused the same cached client")
	}
	if cached, err := pool.Client(base); err != nil || cached != enabled {
		t.Fatalf("unchanged compression policy was not cached: client=%p err=%v", cached, err)
	}
	enabledTransport, ok := enabled.Transport.(*http.Transport)
	if !ok || enabledTransport.DisableCompression {
		t.Fatal("enabled compression client has DisableCompression set")
	}
	disabledTransport, ok := disabled.Transport.(*http.Transport)
	if !ok || !disabledTransport.DisableCompression {
		t.Fatal("disabled compression client has DisableCompression unset")
	}

	do := func(client *http.Client, header string) {
		req, err := http.NewRequest(http.MethodGet, server.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		if header != "" {
			req.Header.Set("Accept-Encoding", header)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	do(disabled, "")
	if got := <-received; got != "" {
		t.Fatalf("DisableCompression client added Accept-Encoding %q", got)
	}
	do(enabled, "")
	if got := <-received; got != "gzip" {
		t.Fatalf("default transport Accept-Encoding = %q, want gzip", got)
	}
	do(disabled, "br")
	if got := <-received; got != "br" {
		t.Fatalf("explicit Accept-Encoding was not preserved: %q", got)
	}
}
