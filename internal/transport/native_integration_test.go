package transport

import (
	"context"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hoorific/internal/core"
)

func TestNativeWireHelperIntegration(t *testing.T) {
	helper := os.Getenv("HOORIFIC_TEST_CODEX_WIRE")
	if helper == "" {
		t.Skip("HOORIFIC_TEST_CODEX_WIRE is not set")
	}
	serverRequests := atomic.Int64{}
	cancelReady := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverRequests.Add(1)
		if r.ProtoMajor != 1 || r.ProtoMinor != 1 {
			t.Errorf("upstream protocol = %s, want HTTP/1.1", r.Proto)
		}
		if got := r.Header.Get("Accept-Encoding"); got != "" {
			t.Errorf("native helper added implicit Accept-Encoding %q", got)
		}
		if r.URL.Path == "/cancel" {
			_, _ = io.WriteString(w, "first")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			close(cancelReady)
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, "stream-")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		_, _ = io.WriteString(w, "complete")
	}))
	defer server.Close()

	certificate := server.Certificate()
	if certificate == nil {
		t.Fatal("TLS fixture has no certificate")
	}
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	oldCertFile, hadCertFile := os.LookupEnv("SSL_CERT_FILE")
	if err := os.Setenv("SSL_CERT_FILE", caFile); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if hadCertFile {
			_ = os.Setenv("SSL_CERT_FILE", oldCertFile)
		} else {
			_ = os.Unsetenv("SSL_CERT_FILE")
		}
	}()

	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	policy := NetworkPolicy{
		AllowedHosts:       []string{u.Hostname()},
		AllowedCIDRs:       []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		AllowPrivate:       true,
		DisableCompression: true,
		NativeCodex:        true,
		NativeScope:        "integration\x00native",
	}
	pool, err := NewPoolWithConfig(core.TransportConfig{NativeEnginePath: helper, MaxConnsPerHost: 1})
	if err != nil {
		t.Fatal(err)
	}
	client, err := pool.Client(policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := client.Transport.(*nativeRoundTripper); !ok {
		t.Fatalf("native client transport = %T, want nativeRoundTripper", client.Transport)
	}

	request, err := http.NewRequest(http.MethodPost, server.URL+"/stream", strings.NewReader("request"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "stream-complete" {
		t.Fatalf("stream body = %q", body)
	}

	cancelContext, cancel := context.WithCancel(context.Background())
	cancelRequest, err := http.NewRequestWithContext(cancelContext, http.MethodGet, server.URL+"/cancel", nil)
	if err != nil {
		t.Fatal(err)
	}
	cancelResponse, err := client.Do(cancelRequest)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelReady:
	case <-time.After(5 * time.Second):
		cancelResponse.Body.Close()
		cancel()
		t.Fatal("upstream did not begin cancellation stream")
	}
	first := make([]byte, len("first"))
	if _, err := io.ReadFull(cancelResponse.Body, first); err != nil {
		t.Fatal(err)
	}
	cancel()
	_, readErr := cancelResponse.Body.Read(make([]byte, 32))
	_ = cancelResponse.Body.Close()
	if !errors.Is(readErr, context.Canceled) {
		t.Fatalf("canceled native read error = %v, want context.Canceled", readErr)
	}

	beforeDenied := serverRequests.Load()
	denied := policy
	denied.AllowedCIDRs = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	deniedClient, err := pool.Client(denied)
	if err != nil {
		t.Fatal(err)
	}
	deniedRequest, err := http.NewRequest(http.MethodGet, server.URL+"/denied", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deniedClient.Do(deniedRequest); err == nil {
		t.Fatal("denied CIDR request unexpectedly succeeded")
	}
	if got := serverRequests.Load(); got != beforeDenied {
		t.Fatalf("denied CIDR reached upstream: requests before=%d after=%d", beforeDenied, got)
	}

	engine := pool.native
	dir := ""
	if engine != nil {
		dir = engine.dir
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	if engine == nil {
		t.Fatal("native engine was not initialized")
	}
	select {
	case <-engine.waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("native helper did not exit after pool close")
	}
	if dir != "" {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("native helper directory still exists: %v", err)
		}
	}
	if _, err := client.Do(request); err == nil {
		t.Fatal("request succeeded after native pool close")
	}
}

func TestNativeWireHelperAllowsMaxConnsPerHost257(t *testing.T) {
	helper := nativeIntegrationHelper(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/max-connections" {
			t.Errorf("native max-connections request = %s %s", r.Method, r.URL.Path)
		}
		_, _ = io.WriteString(w, "max-connections-ok")
	}))
	defer server.Close()

	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := NewPoolWithConfig(core.TransportConfig{
		NativeEnginePath: helper,
		MaxConnsPerHost:  257,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := pool.Close(); err != nil {
			t.Errorf("close native pool: %v", err)
		}
	}()
	client, err := pool.Client(NetworkPolicy{
		AllowedHosts: []string{u.Hostname()},
		AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		AllowPrivate: true,
		NativeCodex:  true,
		NativeScope:  "integration\x00max-connections-257",
	})
	if err != nil {
		t.Fatal(err)
	}

	response, err := client.Get(server.URL + "/max-connections")
	if err != nil {
		t.Fatalf("native max-connections request failed: %v", err)
	}
	body, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("native max-connections status = %d", response.StatusCode)
	}
	if string(body) != "max-connections-ok" {
		t.Fatalf("native max-connections body = %q", body)
	}
}

func TestNativeWireHelperRejectsNormalizedNumericLiteralPinMismatch(t *testing.T) {
	helper := nativeIntegrationHelper(t)
	requests := atomic.Int64{}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, "forbidden")
	}))
	countingListener := &nativeIntegrationCountingListener{Listener: server.Listener}
	server.Listener = countingListener
	server.Start()
	defer server.Close()

	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	numericURL := *u
	numericURL.Host = net.JoinHostPort("0x7f000001", u.Port())
	numericURL.Path = "/numeric-literal"

	restoreResolver := installNativeIntegrationResolver(nativeIntegrationResolver(netip.MustParseAddr("203.0.113.7")))
	defer restoreResolver()

	pool, err := NewPoolWithConfig(core.TransportConfig{NativeEnginePath: helper})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := pool.Close(); err != nil {
			t.Errorf("close native pool: %v", err)
		}
	}()
	client, err := pool.Client(NetworkPolicy{
		AllowedHosts: []string{"0x7f000001"},
		AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("203.0.113.7/32")},
		NativeCodex:  true,
		NativeScope:  "integration\x00numeric-literal-pin",
	})
	if err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequest(http.MethodGet, numericURL.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil {
		t.Fatal("normalized numeric literal unexpectedly bypassed address pins")
	}
	if !strings.Contains(err.Error(), "literal address does not match approved pins") {
		t.Fatalf("normalized numeric literal error = %v", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("normalized numeric literal reached upstream: requests=%d", got)
	}
	if got := countingListener.accepts.Load(); got != 0 {
		t.Fatalf("normalized numeric literal reached loopback listener: accepts=%d", got)
	}
}

func TestNativeWireHelperRejectsExitedCachedEngine(t *testing.T) {
	pool, err := NewPoolWithConfig(core.TransportConfig{NativeEnginePath: nativeIntegrationHelper(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	policy := NetworkPolicy{AllowedHosts: []string{"127.0.0.1"}, NativeCodex: true, NativeScope: "integration\x00exited"}
	if _, err := pool.Client(policy); err != nil {
		t.Fatal(err)
	}
	if err := pool.native.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pool.native.waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("native helper did not exit after lifetime-pipe EOF")
	}
	if _, err := pool.Client(policy); err == nil {
		t.Fatal("cached native client accepted an exited helper")
	}
	policy.NativeScope = "integration\x00new-after-exit"
	if _, err := pool.Client(policy); err == nil {
		t.Fatal("new native client accepted an exited helper")
	}
}

func nativeIntegrationHelper(t *testing.T) string {
	t.Helper()
	helper := os.Getenv("HOORIFIC_TEST_CODEX_WIRE")
	if helper == "" {
		t.Skip("HOORIFIC_TEST_CODEX_WIRE is not set")
	}
	return helper
}

type nativeIntegrationCountingListener struct {
	net.Listener
	accepts atomic.Int64
}

func (l *nativeIntegrationCountingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.accepts.Add(1)
	}
	return conn, err
}

var nativeIntegrationResolverMu sync.Mutex

func installNativeIntegrationResolver(resolver *net.Resolver) func() {
	nativeIntegrationResolverMu.Lock()
	previous := net.DefaultResolver
	net.DefaultResolver = resolver
	return func() {
		net.DefaultResolver = previous
		nativeIntegrationResolverMu.Unlock()
	}
}

func nativeIntegrationResolver(ip netip.Addr) *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			client, server := net.Pipe()
			go func() {
				defer server.Close()
				_ = server.SetDeadline(time.Now().Add(5 * time.Second))
				// Resolver uses DNS stream framing for non-PacketConn connections.
				var size [2]byte
				if _, err := io.ReadFull(server, size[:]); err != nil {
					return
				}
				query := make([]byte, binary.BigEndian.Uint16(size[:]))
				if _, err := io.ReadFull(server, query); err != nil {
					return
				}
				response, err := nativeIntegrationDNSResponse(query, ip)
				if err != nil {
					return
				}
				binary.BigEndian.PutUint16(size[:], uint16(len(response)))
				_, _ = server.Write(append(size[:], response...))
			}()
			return client, nil
		},
	}
}

func nativeIntegrationDNSResponse(query []byte, ip netip.Addr) ([]byte, error) {
	if len(query) < 12 {
		return nil, errors.New("test DNS query is too short")
	}
	offset := 12
	for {
		if offset >= len(query) {
			return nil, errors.New("test DNS query name is truncated")
		}
		length := int(query[offset])
		offset++
		if length == 0 {
			break
		}
		if length > 63 || length&0xc0 != 0 || offset+length > len(query) {
			return nil, errors.New("test DNS query name is invalid")
		}
		offset += length
	}
	if offset+4 > len(query) {
		return nil, errors.New("test DNS query type is truncated")
	}
	questionEnd := offset + 4
	qtype := binary.BigEndian.Uint16(query[offset : offset+2])
	answerCount := uint16(0)
	if qtype == 1 && ip.Is4() {
		answerCount = 1
	}
	response := make([]byte, 0, questionEnd+16)
	response = append(response, query[:2]...)
	response = append(response, 0x81, 0x80)
	response = append(response, 0, 1, byte(answerCount>>8), byte(answerCount), 0, 0, 0, 0)
	response = append(response, query[12:questionEnd]...)
	if answerCount == 0 {
		return response, nil
	}
	response = append(response, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4)
	octets := ip.As4()
	response = append(response, octets[:]...)
	return response, nil
}
