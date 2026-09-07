package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type transportProof struct {
	Profile string `json:"profile"`
	DirectEndpoint string `json:"direct_endpoint"`
	GatewayEndpoint string `json:"gateway_endpoint"`
	GatewayUpstream string `json:"gateway_upstream"`
	FixtureOrigin string `json:"fixture_origin"`
	CAFile string `json:"ca_file"`
	CASHA256 string `json:"ca_sha256"`
	BridgePresent bool `json:"bridge_present"`
	Source string `json:"source"`
}

type linkEvidence struct {
	Status string `json:"status"`
	Connections int64 `json:"got_conn_events"`
	Reused int64 `json:"reused_connection_events"`
	TLSVersions map[string]int64 `json:"tls_version_events"`
}

type transportFacts struct {
	Profile string `json:"profile"`
	Proof *transportProof `json:"proof,omitempty"`
	ProofStatus string `json:"proof_status"`
	TrustStatus string `json:"direct_trust_status"`
	DirectUpstreamScheme string `json:"direct_upstream_scheme"`
	GatewayUpstreamScheme string `json:"gateway_upstream_scheme"`
	Links map[string]linkEvidence `json:"links"`
}

type childTransport struct {
	proof transportProof
	roots *x509.CertPool
	mu sync.Mutex
	links map[string]linkEvidence
}

// Exact spelling disallows alternate paths, encoded separators and DNS rebinding.
func loopbackURL(raw, scheme, path string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != scheme || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.Path != path || u.RawPath != "" {
		return nil, fmt.Errorf("expected canonical %s loopback URL with path %q", scheme, path)
	}
	host, port, err := net.SplitHostPort(u.Host)
	ip := net.ParseIP(host)
	n, portErr := strconv.Atoi(port)
	if err != nil || ip == nil || !ip.IsLoopback() || portErr != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port || raw != scheme+"://"+net.JoinHostPort(ip.String(), port)+path {
		return nil, fmt.Errorf("endpoint must use a canonical loopback literal and explicit nonzero port")
	}
	return u, nil
}

func privateTransportFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil { return nil, fmt.Errorf("cannot open transport file") }
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > 64*1024 {
		return nil, fmt.Errorf("transport file must be private, regular and at most 64 KiB")
	}
	b, err := io.ReadAll(io.LimitReader(f, 64*1024+1))
	if err != nil || len(b) > 64*1024 { return nil, fmt.Errorf("cannot read transport file") }
	return b, nil
}

func loadChildTransport(o options, endpoint, fixture string) (*childTransport, error) {
	if o.proofFile == "" { return nil, fmt.Errorf("--transport-proof-file is required, including manual mode") }
	data, err := privateTransportFile(o.proofFile)
	if err != nil { return nil, err }
	// Decode fields individually to reject missing, duplicate, unknown and null values.
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') { return nil, fmt.Errorf("proof must be a JSON object") }
	fields := make(map[string]json.RawMessage)
	for dec.More() {
		tok, err = dec.Token()
		if err != nil { return nil, fmt.Errorf("invalid proof field") }
		name, ok := tok.(string)
		if !ok { return nil, fmt.Errorf("invalid proof field name") }
		if _, exists := fields[name]; exists { return nil, fmt.Errorf("duplicate proof field") }
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) { return nil, fmt.Errorf("invalid proof value") }
		fields[name] = value
	}
	if _, err = dec.Token(); err != nil { return nil, fmt.Errorf("invalid proof object") }
	if err = dec.Decode(new(any)); err != io.EOF { return nil, fmt.Errorf("trailing proof content") }
	p := transportProof{}
	wanted := map[string]any{"profile": &p.Profile, "direct_endpoint": &p.DirectEndpoint, "gateway_endpoint": &p.GatewayEndpoint, "gateway_upstream": &p.GatewayUpstream, "fixture_origin": &p.FixtureOrigin, "ca_file": &p.CAFile, "ca_sha256": &p.CASHA256, "bridge_present": &p.BridgePresent, "source": &p.Source}
	if len(fields) != len(wanted) { return nil, fmt.Errorf("proof requires exactly the nine contract fields") }
	for name, dst := range wanted {
		value, ok := fields[name]
		if !ok || json.Unmarshal(value, dst) != nil { return nil, fmt.Errorf("missing or invalid proof field %s", name) }
	}
	if p.Profile != o.profile || p.GatewayEndpoint != endpoint || p.FixtureOrigin != fixture { return nil, fmt.Errorf("proof does not match child profile, gateway or fixture") }
	if p.Source != "suite-owned provisioning" && p.Source != "manual configuration assertion" { return nil, fmt.Errorf("proof source must identify suite-owned provisioning or manual configuration assertion") }
	if _, err = loopbackURL(p.FixtureOrigin, "http", ""); err != nil { return nil, err }
	if _, err = loopbackURL(endpoint, "http", "/v1/chat/completions"); err != nil { return nil, err }
	directScheme := "http"
	if o.profile == "tls" { directScheme = "https" }
	direct, err := loopbackURL(p.DirectEndpoint, directScheme, "/v1/chat/completions")
	if err != nil { return nil, err }
	origin := direct.Scheme + "://" + direct.Host
	if p.GatewayUpstream != origin+"/v1" || p.GatewayEndpoint == p.DirectEndpoint || endpoint == p.FixtureOrigin+"/v1/chat/completions" { return nil, fmt.Errorf("proof endpoint roles or upstream do not match") }
	c := &childTransport{proof: p, links: map[string]linkEvidence{
		"direct": {Status: "not observed", TLSVersions: map[string]int64{}},
		"client_to_gateway": {Status: "not observed", TLSVersions: map[string]int64{}},
		"gateway_upstream": {Status: "unavailable in child; provisioning/assertion is not observation"},
	}}
	if o.profile == "http" {
		if p.BridgePresent || p.CAFile != "" || p.CASHA256 != "" || origin != p.FixtureOrigin { return nil, fmt.Errorf("HTTP proof must use the fixture directly without bridge or CA") }
		return c, nil
	}
	if !p.BridgePresent || p.CAFile == "" || direct.Host == strings.TrimPrefix(p.FixtureOrigin, "http://") { return nil, fmt.Errorf("TLS proof requires a distinct bridge and CA") }
	ca, err := privateTransportFile(p.CAFile)
	if err != nil { return nil, err }
	digest := sha256.Sum256(ca)
	if p.CASHA256 != hex.EncodeToString(digest[:]) { return nil, fmt.Errorf("CA SHA256 does not match exact PEM bytes") }
	c.roots = x509.NewCertPool()
	remaining, count := ca, 0
	for len(bytes.TrimSpace(remaining)) > 0 {
		remaining = bytes.TrimSpace(remaining)
		if !bytes.HasPrefix(remaining, []byte("-----BEGIN CERTIFICATE-----")) { return nil, fmt.Errorf("CA must contain only certificate PEM blocks") }
		block, rest := pem.Decode(remaining)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 { return nil, fmt.Errorf("invalid CA PEM") }
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !cert.IsCA || !cert.BasicConstraintsValid || cert.KeyUsage&x509.KeyUsageCertSign == 0 { return nil, fmt.Errorf("PEM certificate is not a signing CA") }
		c.roots.AddCert(cert)
		count++
		remaining = rest
	}
	if count == 0 { return nil, fmt.Errorf("CA PEM is empty") }
	return c, nil
}

func (c *childTransport) snapshot() transportFacts {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.proof
	direct, _ := url.Parse(p.DirectEndpoint)
	gatewayUpstream, _ := url.Parse(p.GatewayUpstream)
	f := transportFacts{Profile: p.Profile, Proof: &p, ProofStatus: "validated matching configuration assertion; source is declared, not authenticated", TrustStatus: "not applicable: HTTP", DirectUpstreamScheme: direct.Scheme, GatewayUpstreamScheme: gatewayUpstream.Scheme, Links: make(map[string]linkEvidence, len(c.links))}
	if c.roots != nil {
		f.TrustStatus = "digest-matched CA PEM loaded; actual peer trust enforced by TLS handshake"
	}
	for name, link := range c.links {
		copy := link
		copy.TLSVersions = make(map[string]int64, len(link.TLSVersions))
		for version, count := range link.TLSVersions { copy.TLSVersions[version] = count }
		f.Links[name] = copy
	}
	return f
}

func (c *childTransport) observe(link string, info httptrace.GotConnInfo) {
	version := "none (HTTP)"
	if conn, ok := info.Conn.(*tls.Conn); ok { version = tls.VersionName(conn.ConnectionState().Version) }
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.links[link]
	e.Status = "observed by client httptrace.GotConn; counts are connection-use events, not unique connections"
	e.Connections++
	if info.Reused { e.Reused++ }
	e.TLSVersions[version]++
	c.links[link] = e
}

type targetTransport struct {
	base *http.Transport
	endpoint, link string
	owner *childTransport
}

func (t *targetTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.String() != t.endpoint { return nil, fmt.Errorf("request endpoint differs from transport proof") }
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { t.owner.observe(t.link, info) }}
	return t.base.RoundTrip(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
}
func extendedClient(client *http.Client, timeout time.Duration) (*http.Client, func()) {
	if wrapped, ok := client.Transport.(*targetTransport); ok {
		base := wrapped.base.Clone()
		base.ResponseHeaderTimeout = timeout
		extended := &http.Client{
			Transport: &targetTransport{base: base, endpoint: wrapped.endpoint, link: wrapped.link, owner: wrapped.owner},
			Timeout: timeout,
			CheckRedirect: client.CheckRedirect,
		}
		return extended, base.CloseIdleConnections
	}
	return newClient(timeout)
}
func (c *childTransport) client(t target, timeout time.Duration) (*http.Client, func()) {
	client, closeClient := newClient(timeout)
	tr := client.Transport.(*http.Transport)
	link := "client_to_gateway"
	endpoint := c.proof.GatewayEndpoint
	if t.Name == "direct" {
		link, endpoint = "direct", c.proof.DirectEndpoint
		if c.roots != nil { tr.TLSClientConfig = &tls.Config{RootCAs: c.roots, MinVersion: tls.VersionTLS12} }
	}
	client.Transport = &targetTransport{base: tr, endpoint: endpoint, link: link, owner: c}
	return client, closeClient
}

// The marker is permanent: even a failed run must use a fresh output directory.
func claimProfile(dir, profile string) error {
	if profile != "http" && profile != "tls" { return fmt.Errorf("--upstream-transport must be http or tls") }
	b, err := json.Marshal(map[string]any{"profile": profile, "pid": os.Getpid()})
	if err != nil { return err }
	if err = os.MkdirAll(dir, 0700); err != nil { return err }
	f, err := os.OpenFile(filepath.Join(dir, "profile.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil { return fmt.Errorf("output profile marker already exists or cannot be claimed: %w", err) }
	_, writeErr := f.Write(append(b, '\n'))
	closeErr := f.Close()
	if writeErr != nil { return writeErr }
	if closeErr != nil { return closeErr }
	for _, name := range []string{"results.json", "summary.txt"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) { return fmt.Errorf("output already contains report artifacts or cannot be inspected") }
	}
	return nil
}
