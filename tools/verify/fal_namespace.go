package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"
)

const falNamespaceImage = "docker.io/library/postgres:17"

// The parent never opens a fixture socket. Only Podman's isolated child binds
// port 443; image inspection and --pull=never forbid implicit registry access.
func (e *environment) falNamespace() []result {
	start := time.Now()
	failed := func(err error, evidence any) []result {
		return []result{extResult("operations-fal-namespace", start, evidence, err)}
	}
	podman, err := exec.LookPath("podman")
	if err != nil {
		return failed(fmt.Errorf("required local Podman is unavailable: %w", err), nil)
	}
	binary, err := filepath.Abs(e.binary)
	if err != nil || e.binary == "" {
		return failed(fmt.Errorf("built gateway executable is required"), nil)
	}
	verify, err := os.Executable()
	if err != nil {
		return failed(err, nil)
	}
	for _, path := range []string{binary, verify} {
		info, statErr := os.Stat(path)
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return failed(fmt.Errorf("required executable unavailable: %s", path), nil)
		}
		if strings.ContainsAny(path, ":,\n") {
			return failed(fmt.Errorf("executable mount path contains unsupported separator"), nil)
		}
	}
	root, err := os.MkdirTemp("", "hoorific-fal-namespace-")
	if err != nil {
		return failed(err, nil)
	}
	if strings.ContainsAny(root, ":,\n") {
		_ = os.RemoveAll(root)
		return failed(fmt.Errorf("temporary mount path contains unsupported separator"), nil)
	}
	image := os.Getenv("HOORIFIC_VERIFY_FAL_IMAGE")
	if image == "" {
		image = falNamespaceImage
	}
	evidence := map[string]any{"image": image, "network": "none", "private_artifacts": root}
	logfile, err := os.OpenFile(filepath.Join(root, "podman.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return failed(err, evidence)
	}
	defer logfile.Close()
	logs := &falBoundedLog{writer: logfile, remaining: 2 << 20}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	inspectCtx, inspectCancel := context.WithTimeout(ctx, 15*time.Second)
	inspect := exec.CommandContext(inspectCtx, podman, "--remote=false", "image", "inspect", "--format", "{{.Id}}", image)
	inspect.WaitDelay = 2 * time.Second
	var imageID bytes.Buffer
	inspect.Stdout, inspect.Stderr = &falBoundedLog{writer: &imageID, remaining: 4096}, logs
	err = inspect.Run()
	inspectCancel()
	id := strings.TrimSpace(imageID.String())
	if err != nil || !regexp.MustCompile(`^(sha256:)?[0-9a-f]{64}$`).MatchString(id) {
		return failed(fmt.Errorf("required cached image inspection failed (no pull attempted): %v", err), evidence)
	}
	evidence["image_id"] = id
	name := filepath.Base(root)
	args := []string{"--remote=false", "run", "--name", name, "--pull=never", "--network=none",
		"--add-host=queue.fal.run:127.0.0.1", "--add-host=fal.run:127.0.0.1",
		"--label", "io.hoorific.verify.owner=" + name, "--label", "io.hoorific.verify.scenario=operations-fal",
		"--read-only", "--user=0:0", "--cap-drop=ALL", "--cap-add=NET_BIND_SERVICE",
		"--security-opt=no-new-privileges", "--security-opt=label=disable", "--pids-limit=128", "--memory=768m", "--cpus=2",
		"--http-proxy=false", "--tmpfs", "/tmp:rw,nosuid,nodev,size=128m,mode=1777",
		"--volume", binary + ":/fixture/hoorific:ro", "--volume", verify + ":/fixture/verify:ro", "--volume", root + ":/data:rw",
		"--env", "HOORIFIC_FAL_NAMESPACE_CHILD=1", "--env", "TMPDIR=/data", "--env", "HOME=/data",
		"--entrypoint", "/fixture/verify", id, "--fal-namespace-child", "--binary", "/fixture/hoorific", "--output", "/data/result.json"}
	cmd := exec.CommandContext(ctx, podman, args...)
	cmd.WaitDelay = 3 * time.Second
	cmd.Stdout, cmd.Stderr = logs, logs
	runErr := cmd.Run()
	// Removal is attempted even if run failed after creating the container. It
	// addresses only the fresh name owned by this invocation, never a broad label.
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
	remove := exec.CommandContext(cleanupCtx, podman, "--remote=false", "rm", "--force", "--time=2", "--ignore", name)
	remove.WaitDelay = 2 * time.Second
	remove.Stdout, remove.Stderr = logs, logs
	cleanupErr := remove.Run()
	cleanupCancel()
	if cleanupErr != nil {
		return failed(fmt.Errorf("owned container cleanup failed: %w", cleanupErr), evidence)
	}
	file, err := os.Open(filepath.Join(root, "result.json"))
	if err != nil {
		return failed(fmt.Errorf("container produced no private result (exit %v): %w", runErr, err), evidence)
	}
	raw, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	file.Close()
	var out []result
	if err != nil || len(raw) > 1<<20 || json.Unmarshal(raw, &out) != nil || len(out) == 0 {
		return failed(fmt.Errorf("container result is missing, malformed or oversized"), evidence)
	}
	allPassed := true
	for i := range out {
		if !strings.HasPrefix(out[i].Name, "operations-fal-") || (out[i].Status != "passed" && out[i].Status != "failed") {
			return failed(fmt.Errorf("container returned an invalid result contract"), evidence)
		}
		allPassed = allPassed && out[i].Status == "passed"
	}
	if runErr != nil && allPassed {
		return failed(fmt.Errorf("container exited unsuccessfully despite passing results: %w", runErr), evidence)
	}
	out = append(out, extResult("operations-fal-container", start, evidence, runErr))
	return out
}

// Concurrent stdout/stderr writes are bounded so failed subprocesses cannot
// exhaust the host filesystem. Diagnostics remain in the owner's 0700 directory.
type falBoundedLog struct {
	mu        sync.Mutex
	writer    io.Writer
	remaining int
}

func (w *falBoundedLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	if len(p) > w.remaining {
		p = p[:w.remaining]
	}
	if len(p) != 0 {
		written, err := w.writer.Write(p)
		w.remaining -= written
		if err != nil {
			return written, err
		}
	}
	return n, nil
}

// Called by main immediately after flag parsing, before ordinary environment
// setup. A failed prerequisite is a failed qualification, never not-run.
func runFalNamespaceChild(binary, output string) int {
	start := time.Now()
	out := []result{}
	err := func() error {
		if os.Getenv("HOORIFIC_FAL_NAMESPACE_CHILD") != "1" || output != "/data/result.json" || binary != "/fixture/hoorific" {
			return fmt.Errorf("fal child requires the owned namespace launch contract")
		}
		if _, err := os.Stat("/run/.containerenv"); err != nil {
			return fmt.Errorf("Podman container marker unavailable: %w", err)
		}
		interfaces, err := net.Interfaces()
		if err != nil {
			return err
		}
		for _, iface := range interfaces {
			if iface.Flags&net.FlagLoopback == 0 {
				return fmt.Errorf("non-loopback interface present in fal namespace")
			}
		}
		for _, host := range []string{"queue.fal.run", "fal.run"} {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			cancel()
			if err != nil || len(ips) == 0 {
				return fmt.Errorf("container-only host mapping unavailable for %s", host)
			}
			for _, ip := range ips {
				if !ip.IP.Equal(net.ParseIP("127.0.0.1")) {
					return fmt.Errorf("non-loopback fal host mapping")
				}
			}
		}
		cert, ca, err := falNamespaceCertificate()
		if err != nil {
			return err
		}
		caFile := "/data/fixture-ca.pem"
		if err := os.WriteFile(caFile, ca, 0600); err != nil {
			return err
		}
		if err := os.Mkdir("/data/empty-ca", 0700); err != nil {
			return err
		}
		// This happens before migrate/bootstrap/serve. No host trust is changed.
		for key, value := range map[string]string{"SSL_CERT_FILE": caFile, "SSL_CERT_DIR": "/data/empty-ca", "GODEBUG": "netdns=go"} {
			if err := os.Setenv(key, value); err != nil {
				return err
			}
		}
		for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
			if err := os.Unsetenv(key); err != nil {
				return err
			}
		}
		fixture := &falNamespaceFixture{calls: map[string]int{}}
		listener, err := net.Listen("tcp", "127.0.0.1:443")
		if err != nil {
			return fmt.Errorf("container fixture port 443: %w", err)
		}
		server := &http.Server{Handler: fixture, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second}
		served := make(chan error, 1)
		go func() {
			served <- server.Serve(tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}))
		}()
		defer func() {
			_ = server.Close()
			select {
			case <-served:
			case <-time.After(3 * time.Second):
			}
		}()
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			return fmt.Errorf("fixture CA could not be loaded")
		}
		transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
		probe := &http.Client{Transport: transport, Timeout: 3 * time.Second}
		defer transport.CloseIdleConnections()
		for _, origin := range []string{"https://queue.fal.run", "https://127.0.0.1", "https://queue.fal.run:443"} {
			resp, err := probe.Get(origin + "/fixture-ready")
			if err != nil {
				return fmt.Errorf("TLS fixture readiness for %s: %w", origin, err)
			}
			resp.Body.Close()
			if resp.StatusCode != 204 {
				return fmt.Errorf("TLS fixture readiness HTTP %d", resp.StatusCode)
			}
		}
		env, err := newEnvironment("standalone", "", "")
		if err != nil {
			return err
		}
		defer env.fixture.server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
		defer cancel()
		stop, err := falNamespaceStart(ctx, env, binary)
		if err != nil {
			return err
		}
		defer stop()
		if err := falNamespaceBootstrap(ctx, env, binary); err != nil {
			return err
		}
		checks, err := falNamespaceExercise(env, fixture)
		out = append(out, checks...)
		if err != nil {
			return err
		}
		if err := stop(); err != nil {
			return err
		}
		return nil
	}()
	if err != nil {
		out = append(out, extResult("operations-fal-namespace-child", start, nil, err))
	}
	if len(out) == 0 {
		out = append(out, extResult("operations-fal-namespace-child", start, nil, fmt.Errorf("no fal assertions executed")))
	}
	raw, marshalErr := json.MarshalIndent(out, "", "  ")
	if marshalErr != nil {
		return 1
	}
	// Do not permit direct CLI invocation to overwrite an arbitrary host path.
	if output != "/data/result.json" || os.Getenv("HOORIFIC_FAL_NAMESPACE_CHILD") != "1" {
		return 1
	}
	file, writeErr := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if writeErr != nil {
		return 1
	}
	_, writeErr = file.Write(raw)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil || err != nil {
		return 1
	}
	for _, r := range out {
		if r.Status != "passed" {
			return 1
		}
	}
	return 0
}

func falNamespaceCertificate() (tls.Certificate, []byte, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ephemeral fal namespace CA"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "queue.fal.run"}, DNSNames: []string{"queue.fal.run", "fal.run", "localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &serverKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(serverKey)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	cert, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), err
}

// Unlike environment.Start, this child-specific launcher gives every process a
// context and WaitDelay, and has exactly one waiter for the gateway process.
func falNamespaceStart(ctx context.Context, e *environment, binary string) (func() error, error) {
	logfile, err := os.OpenFile(filepath.Join(e.root, "fal-gateway.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	logs := &falBoundedLog{writer: logfile, remaining: 2 << 20}
	migrationCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	migration := exec.CommandContext(migrationCtx, binary, "migrate", "--config", e.config)
	migration.WaitDelay = 2 * time.Second
	migration.Stdout, migration.Stderr, migration.Dir = logs, logs, e.root
	err = migration.Run()
	cancel()
	if err != nil {
		logfile.Close()
		return nil, fmt.Errorf("isolated gateway migrate: %w", err)
	}
	serveCtx, serveCancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(serveCtx, binary, "serve", "--config", e.config)
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdout, cmd.Stderr, cmd.Dir = logs, logs, e.root
	if err := cmd.Start(); err != nil {
		serveCancel()
		logfile.Close()
		return nil, err
	}
	e.server, e.binary = cmd, binary
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var once sync.Once
	var stopErr error
	stop := func() error {
		once.Do(func() {
			_ = cmd.Process.Signal(os.Interrupt)
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				serveCancel()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					stopErr = fmt.Errorf("gateway did not reap after kill")
				}
			}
			serveCancel()
			logfile.Close()
		})
		return stopErr
	}
	readyCtx, readyCancel := context.WithTimeout(ctx, 15*time.Second)
	defer readyCancel()
	for {
		req, _ := http.NewRequestWithContext(readyCtx, "GET", "http://"+e.management+"/health/live", nil)
		resp, err := e.client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return stop, nil
			}
		}
		select {
		case <-readyCtx.Done():
			_ = stop()
			return nil, fmt.Errorf("isolated gateway readiness deadline exceeded")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func falNamespaceBootstrap(ctx context.Context, e *environment, binary string) error {
	bootstrapCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(bootstrapCtx, binary, "admin", "bootstrap", "--config", e.config)
	cmd.WaitDelay = 2 * time.Second
	var stdout bytes.Buffer
	cmd.Stdout = &falBoundedLog{writer: &stdout, remaining: 65536}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("isolated bootstrap command: %w", err)
	}
	match := regexp.MustCompile(`(?m)([A-Za-z0-9_-]{32,})`).FindStringSubmatch(stdout.String())
	if len(match) != 2 {
		return fmt.Errorf("isolated bootstrap omitted redemption code")
	}
	raw, _ := json.Marshal(map[string]string{"code": match[1]})
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://"+e.management+"/admin/api/v1/auth/bootstrap", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 {
		return fmt.Errorf("isolated bootstrap redemption HTTP %d", resp.StatusCode)
	}
	var envelope struct {
		Principal struct{ TenantID string } `json:"principal"`
	}
	if json.Unmarshal(data, &envelope) != nil || envelope.Principal.TenantID == "" {
		return fmt.Errorf("isolated bootstrap omitted tenant")
	}
	e.tenantID = envelope.Principal.TenantID
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "hoorific_session" {
			e.cookie = cookie.Name + "=" + cookie.Value
		}
	}
	if e.cookie == "" {
		return fmt.Errorf("isolated bootstrap omitted session")
	}
	req, _ = http.NewRequestWithContext(ctx, "GET", "http://"+e.management+"/admin/api/v1/session", nil)
	req.Header.Set("Cookie", e.cookie)
	resp, err = e.client.Do(req)
	if err != nil {
		return err
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	var session struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err != nil || resp.StatusCode != 200 || json.Unmarshal(data, &session) != nil || session.CSRFToken == "" {
		return fmt.Errorf("isolated bootstrap session unavailable")
	}
	e.csrf = session.CSRFToken
	return nil
}

type falNamespaceFixture struct {
	mu        sync.Mutex
	calls     map[string]int
	faults    []string
	cancelled map[string]bool
}

func (f *falNamespaceFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == "GET" && r.URL.Path == "/fixture-ready" {
		w.WriteHeader(204)
		return
	}
	owner := ""
	switch r.Header.Get("Authorization") {
	case "Key fal-namespace-secret-a":
		owner = "a"
	case "Key fal-namespace-secret-b":
		owner = "b"
	default:
		f.faults = append(f.faults, "upstream auth was not the exact connection-owned fal Key credential")
		http.Error(w, "invalid fixture authorization", 401)
		return
	}
	if r.Host != "queue.fal.run" || r.TLS == nil || r.TLS.ServerName != "queue.fal.run" {
		f.faults = append(f.faults, "unexpected upstream host, TLS or SNI")
		http.Error(w, "invalid fixture origin", 400)
		return
	}
	key := owner + " " + r.Method + " " + r.URL.RequestURI()
	f.calls[key]++
	if f.cancelled == nil {
		f.cancelled = map[string]bool{}
	}
	w.Header().Set("Content-Type", "application/json")
	write := func(status int, value any) { w.WriteHeader(status); _ = json.NewEncoder(w).Encode(value) }
	if r.Method == "POST" && r.URL.Path == "/owner/app" && r.URL.RawQuery == "" {
		var body map[string]any
		dec := json.NewDecoder(io.LimitReader(r.Body, 4097))
		var extra any
		if dec.Decode(&body) != nil || dec.Decode(&extra) != io.EOF || len(body) != 1 {
			f.faults = append(f.faults, "submit body framing mismatch")
			write(422, map[string]any{"error": "body"})
			return
		}
		prompt, ok := body["prompt"].(string)
		if !ok || (prompt != "job" && prompt != "cancel-direct" && prompt != "cancel-control" && prompt != "evil") {
			f.faults = append(f.faults, "submit prompt mismatch")
			write(422, map[string]any{"error": "prompt"})
			return
		}
		base := "https://queue.fal.run/owner/app/requests/" + prompt
		if prompt == "evil" {
			base = "https://external.invalid/requests/evil"
		}
		write(200, map[string]any{"request_id": prompt, "status": "IN_QUEUE", "status_url": base + "/status?logs=1", "response_url": base, "cancel_url": base + "/cancel"})
		return
	}
	prefix := "/owner/app/requests/"
	if strings.HasPrefix(r.URL.Path, prefix) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, prefix), "/")
		id := parts[0]
		if id == "job" || id == "cancel-direct" || id == "cancel-control" {
			if r.Method == "GET" && len(parts) == 2 && parts[1] == "status" && (r.URL.RawQuery == "" || r.URL.RawQuery == "logs=1") {
				status := "COMPLETED"
				if id != "job" {
					status = "IN_QUEUE"
					if f.cancelled[owner+" "+id] {
						status = "CANCELLED"
					}
				}
				body := map[string]any{"request_id": id, "status": status, "logs": []any{map[string]any{"message": "fixture-log", "level": "INFO"}}}
				if id == "job" {
					body["response_url"] = "https://queue.fal.run/owner/app/requests/job"
				}
				write(200, body)
				return
			}
			if r.Method == "GET" && len(parts) == 1 && id == "job" && r.URL.RawQuery == "" {
				write(200, map[string]any{"answer": "fixture-result-" + owner, "seed": 17, "values": []any{1, "two", true}, "metadata": map[string]any{"exact": true}})
				return
			}
			if r.Method == "PUT" && len(parts) == 2 && parts[1] == "cancel" && id != "job" && r.URL.RawQuery == "" {
				if f.cancelled[owner+" "+id] {
					f.faults = append(f.faults, "same request cancelled twice")
				}
				f.cancelled[owner+" "+id] = true
				write(200, map[string]any{"status": "CANCELLED"})
				return
			}
		}
	}
	f.faults = append(f.faults, "unexpected upstream method, path or query")
	write(404, map[string]any{"error": "unexpected route"})
}

func falNamespaceConnection(e *environment, id, base, owner string) (*operationFixture, error) {
	if err := e.extCreate("connections", id, map[string]any{
		"connector": "fal", "account_id": id, "base_url": base, "dedicated": true, "enabled": true,
		"settings": map[string]string{"endpoint": "owner/app", "allow_private": "true", "allowed_cidrs": "127.0.0.1/32", "credential_prefix": "Key"},
	}); err != nil {
		return nil, err
	}
	if err := e.extImportCredential(id, "fal", id, "fal-namespace-secret-"+owner); err != nil {
		return nil, err
	}
	key, err := falNamespaceKey(e, id+"-key", id, true)
	if err != nil {
		return nil, err
	}
	return &operationFixture{e: e, id: id, key: key}, nil
}

func falNamespaceKey(e *environment, name, connection string, native bool) (string, error) {
	raw, _ := json.Marshal(map[string]any{"data": map[string]any{
		"name": name, "role": "operator", "permissions": []string{"inference:invoke", "connection:" + connection},
		"connections": []string{connection}, "operations": []string{"native"}, "portable": false, "native_account": native, "realtime": false,
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	obs, err := e.extRequest(ctx, true, "POST", "/admin/api/v1/api_keys/"+name+"/issue", raw, nil)
	if err != nil {
		return "", err
	}
	var issued struct {
		Token string `json:"token"`
	}
	if obs.Status != 200 || json.Unmarshal([]byte(obs.Body), &issued) != nil || issued.Token == "" {
		// Never include messages, rejected values, the response body or token.
		var problem struct {
			Code   string `json:"code"`
			Errors []struct {
				Location string `json:"location"`
			} `json:"errors"`
		}
		_ = json.Unmarshal([]byte(obs.Body), &problem)
		code := "unclassified"
		if obs.Status == 422 {
			code = "validation_failed"
		}
		if obs.Status == 200 {
			code = "invalid_issue_response"
		}
		for _, candidate := range []string{obs.ErrorCode, problem.Code} {
			switch candidate {
			case "invalid_request", "validation_failed", "unauthorized", "forbidden", "version_conflict", "unavailable", "not_found", "unsupported_operation":
				code = candidate
			}
		}
		fields := []string{}
		for _, field := range []string{"body.data", "body.data.name", "body.data.role", "body.data.permissions", "body.data.aliases", "body.data.connections", "body.data.operations", "body.data.portable", "body.data.native_account", "body.data.realtime"} {
			for _, detail := range problem.Errors {
				if detail.Location == field {
					fields = append(fields, field)
					break
				}
			}
		}
		return "", fmt.Errorf("fal key issuance HTTP %d code=%s validation_fields=%v", obs.Status, code, fields)
	}
	return issued.Token, nil
}

func falNamespaceObject(reply operationReply, status int, want map[string]any) (map[string]any, error) {
	if reply.status != status {
		return nil, fmt.Errorf("expected HTTP %d, got %d code=%s", status, reply.status, reply.header.Get("X-Hoorific-Error-Code"))
	}
	var got map[string]any
	if json.Unmarshal(reply.body, &got) != nil {
		return nil, fmt.Errorf("fal response was not a JSON object")
	}
	encoded, err := json.Marshal(want)
	if err != nil {
		return nil, err
	}
	var normalized map[string]any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		return nil, err
	}
	for key, value := range normalized {
		if !reflect.DeepEqual(got[key], value) {
			return nil, fmt.Errorf("fal decoded field %s differed", key)
		}
	}
	return got, nil
}

func falNamespaceControl(e *environment, id string, value any) (string, error) {
	raw, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("control URL was not a string")
	}
	u, err := url.Parse(raw)
	prefix := "/connect/" + id + "/continuations/"
	if err != nil || u.Scheme != "http" || u.Host != e.inference || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || !strings.HasPrefix(u.Path, prefix) || strings.TrimPrefix(u.Path, prefix) == "" || strings.Contains(strings.TrimPrefix(u.Path, prefix), "/") {
		return "", fmt.Errorf("returned control URL was not exact gateway-owned continuation origin and scope")
	}
	return u.Path, nil
}

func falNamespaceExercise(e *environment, fixture *falNamespaceFixture) ([]result, error) {
	start := time.Now()
	a, err := falNamespaceConnection(e, "fal-namespace-a", "https://queue.fal.run", "a")
	if err != nil {
		return nil, err
	}
	b, err := falNamespaceConnection(e, "fal-namespace-b", "https://queue.fal.run", "b")
	if err != nil {
		return nil, err
	}
	noNative, err := falNamespaceKey(e, "fal-no-native", a.id, false)
	if err != nil {
		return nil, err
	}
	// Both invalid origins are physically reachable with a valid fixture cert;
	// rejection therefore measures provider restrictions, not a DNS/TLS failure.
	bad, err := falNamespaceConnection(e, "fal-invalid-host", "https://127.0.0.1", "a")
	if err != nil {
		return nil, err
	}
	port, err := falNamespaceConnection(e, "fal-explicit-port", "https://queue.fal.run:443", "a")
	if err != nil {
		return nil, err
	}
	submit := func(f *operationFixture, id string) (map[string]string, error) {
		raw, _ := json.Marshal(map[string]string{"prompt": id})
		reply, err := f.request("POST", "owner/app", "application/json", raw)
		if err != nil {
			return nil, err
		}
		obj, err := falNamespaceObject(reply, 200, map[string]any{"request_id": id, "status": "IN_QUEUE"})
		if err != nil {
			return nil, err
		}
		if len(obj) != 5 {
			return nil, fmt.Errorf("submit envelope field count differed")
		}
		controls := map[string]string{}
		for _, field := range []string{"status_url", "response_url", "cancel_url"} {
			controls[field], err = falNamespaceControl(e, f.id, obj[field])
			if err != nil {
				return nil, err
			}
		}
		if controls["status_url"] == controls["response_url"] || controls["cancel_url"] == controls["status_url"] || controls["cancel_url"] == controls["response_url"] {
			return nil, fmt.Errorf("control URL identities collided")
		}
		return controls, nil
	}
	controls, err := submit(a, "job")
	if err != nil {
		return nil, err
	}
	other, err := submit(b, "job")
	if err != nil {
		return nil, err
	}
	resultWant := func(owner string) map[string]any {
		return map[string]any{"answer": "fixture-result-" + owner, "seed": 17, "values": []any{1, "two", true}, "metadata": map[string]any{"exact": true}}
	}
	checkResult := func(f *operationFixture, path, owner string) error {
		reply, err := f.request("GET", path, "", nil)
		if err != nil {
			return err
		}
		obj, err := falNamespaceObject(reply, 200, resultWant(owner))
		if err == nil && len(obj) != 4 {
			err = fmt.Errorf("model-specific result changed shape")
		}
		return err
	}
	statusResult := ""
	for _, path := range []string{"owner/app/requests/job/status?logs=1", controls["status_url"]} {
		reply, err := a.request("GET", path, "", nil)
		if err != nil {
			return nil, err
		}
		obj, err := falNamespaceObject(reply, 200, map[string]any{"request_id": "job", "status": "COMPLETED", "logs": []any{map[string]any{"message": "fixture-log", "level": "INFO"}}})
		if err != nil {
			return nil, err
		}
		if len(obj) != 4 {
			return nil, fmt.Errorf("status envelope changed shape")
		}
		statusResult, err = falNamespaceControl(e, a.id, obj["response_url"])
		if err != nil {
			return nil, err
		}
	}
	for _, path := range []string{"owner/app/requests/job", controls["response_url"], statusResult} {
		if err := checkResult(a, path, "a"); err != nil {
			return nil, err
		}
	}
	if err := checkResult(b, other["response_url"], "b"); err != nil {
		return nil, err
	}
	for _, id := range []string{"cancel-direct", "cancel-control"} {
		cancelControls, err := submit(a, id)
		if err != nil {
			return nil, err
		}
		path := "owner/app/requests/" + id + "/cancel"
		if id == "cancel-control" {
			path = cancelControls["cancel_url"]
		}
		reply, err := a.request("PUT", path, "application/json", []byte(`{}`))
		if err != nil {
			return nil, err
		}
		obj, err := falNamespaceObject(reply, 200, map[string]any{"status": "CANCELLED"})
		if err != nil {
			return nil, err
		}
		if len(obj) != 1 {
			return nil, fmt.Errorf("cancel response was not status-only")
		}
		reply, err = a.request("GET", "owner/app/requests/"+id+"/status?logs=1", "", nil)
		if err != nil {
			return nil, err
		}
		obj, err = falNamespaceObject(reply, 200, map[string]any{"request_id": id, "status": "CANCELLED", "logs": []any{map[string]any{"message": "fixture-log", "level": "INFO"}}})
		if err != nil || len(obj) != 3 {
			return nil, fmt.Errorf("cancelled status did not preserve exact envelope: %v", err)
		}
	}
	out := []result{extResult("operations-fal-lifecycle", start, map[string]any{"submits": 4, "direct_and_rewritten": true, "model_specific_results": 4, "distinct_pending_cancellations": 2, "collision_scopes": 2}, nil)}
	start = time.Now()
	// Every denial must occur before the TLS fixture sees a request. Automatic
	// polling is excluded using its no-query status route, not timing assumptions.
	foreground := func() int {
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		n := len(fixture.faults)
		for key, count := range fixture.calls {
			if !strings.HasSuffix(key, "/status") {
				n += count
			}
		}
		return n
	}
	before := foreground()
	denied := []struct {
		name              string
		f                 *operationFixture
		method, path, key string
		status            int
		code              string
	}{
		{"unauthenticated", a, "GET", controls["response_url"], "invalid", 401, "unauthorized"},
		{"native-grant", a, "GET", controls["response_url"], noNative, 403, "forbidden"},
		{"cross-key", b, "GET", controls["response_url"], b.key, 403, "forbidden"},
		{"cross-scope", b, "GET", strings.Replace(controls["response_url"], "/connect/"+a.id+"/", "/connect/"+b.id+"/", 1), b.key, 404, "unsupported_operation"},
		{"wrong-method", a, "PUT", controls["response_url"], a.key, 405, "unsupported_operation"},
		{"fixed-production-host", bad, "POST", "owner/app", bad.key, 400, "configuration_stale"},
		{"fixed-production-port", port, "POST", "owner/app", port.key, 400, "configuration_stale"},
	}
	for _, test := range denied {
		client := &operationFixture{e: e, id: test.f.id, key: test.key}
		reply, err := client.request(test.method, test.path, "application/json", []byte(`{"prompt":"job"}`))
		if err != nil {
			return out, fmt.Errorf("%s: %w", test.name, err)
		}
		if reply.status != test.status || reply.header.Get("X-Hoorific-Error-Code") != test.code {
			return out, fmt.Errorf("%s expected %d/%s, got %d/%s", test.name, test.status, test.code, reply.status, reply.header.Get("X-Hoorific-Error-Code"))
		}
		if foreground() != before {
			return out, fmt.Errorf("%s reached fixture despite denial", test.name)
		}
	}
	// An actual upstream envelope with an unauthorized control origin must fail
	// closed; never follow it even though network-none is the final safety fence.
	reply, err := a.request("POST", "owner/app", "application/json", []byte(`{"prompt":"evil"}`))
	if err != nil {
		return out, err
	}
	if reply.status != 502 || reply.header.Get("X-Hoorific-Error-Code") != "unsupported_operation" {
		return out, fmt.Errorf("unapproved control origin expected 502/unsupported_operation, got %d/%s", reply.status, reply.header.Get("X-Hoorific-Error-Code"))
	}
	out = append(out, extResult("operations-fal-isolation", start, map[string]any{"denials": len(denied), "unauthorized_control_origin": 502, "denied_fixture_calls": 0, "namespace_interfaces": "loopback-only"}, nil))
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.faults) != 0 {
		return out, fmt.Errorf("TLS fixture recorded %d auth/path/body/origin faults", len(fixture.faults))
	}
	wantCounts := map[string]int{
		"a POST /owner/app": 4, "b POST /owner/app": 1,
		"a GET /owner/app/requests/job/status?logs=1": 2,
		"a GET /owner/app/requests/job":               3, "b GET /owner/app/requests/job": 1,
		"a PUT /owner/app/requests/cancel-direct/cancel": 1, "a PUT /owner/app/requests/cancel-control/cancel": 1,
		"a GET /owner/app/requests/cancel-direct/status?logs=1": 1, "a GET /owner/app/requests/cancel-control/status?logs=1": 1,
	}
	background := 0
	for key, count := range fixture.calls {
		if strings.HasSuffix(key, "/status") {
			background += count
			continue
		}
		if wantCounts[key] != count {
			return out, fmt.Errorf("exact fixture count mismatch for %s: got %d want %d", key, count, wantCounts[key])
		}
	}
	for key, count := range wantCounts {
		if fixture.calls[key] != count {
			return out, fmt.Errorf("required fixture count mismatch for %s", key)
		}
	}
	counts := map[string]int{}
	for key, value := range fixture.calls {
		counts[key] = value
	}
	out = append(out, extResult("operations-fal-counts-auth", start, map[string]any{"calls": counts, "background_status_polls": background, "auth_faults": 0, "gateway_credentials_upstream": 0}, nil))
	return out, nil
}
