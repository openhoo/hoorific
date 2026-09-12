// Command verify qualifies a built Hoorific process against isolated deterministic fixtures.
// It never creates fake gateway successes: missing executable, credentials, or provider
// configuration are reported as setup failures/not-run coverage.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"hoorific/internal/core"
)

type result struct {
	Name, Status, Detail string
	DurationMS           int64 `json:"duration_ms"`
	Evidence             any   `json:"evidence,omitempty"`
}
type report struct {
	StartedAt              time.Time `json:"started_at"`
	Mode, Scenario, Binary string
	Results                []result `json:"results"`
	Passed                 int      `json:"passed"`
	Failed                 int      `json:"failed"`
	NotRun                 int      `json:"not_run"`
	Diagnostics            string   `json:"diagnostics,omitempty"`
}

func main() {
	binary := flag.String("binary", "", "built gateway executable (required)")
	mode := flag.String("mode", "standalone", "standalone or cluster")
	scenario := flag.String("scenario", "all", "all, smoke, protocol, docs-capture, sdk, browser, security, streaming, resources, governance, cluster/governance, credentials, credential-lifecycle, realtime, realtime/bedrock, operations, operations/fal, cost-safety, deep-admin, deep-protocol, codex, packaging, telemetry, live")
	allowPaid := flag.Bool("allow-paid", false, "permit explicitly requested live qualification (still requires credentials)")
	connections := flag.String("connections", "", "comma-separated configured connection IDs for live qualification")
	keyFile := flag.String("gateway-key-file", "", "optional gateway key file for routed scenarios")
	config := flag.String("config", "", "reserved; external configurations are rejected to protect user services")
	fixtureMode := flag.String("fixture-mode", "normal", "deterministic upstream mode: normal, 429, disconnect, truncate, slow, unicode, named-error, or tool")
	keepAlive := flag.Duration("keep-alive", 0, "keep the isolated gateway and fixture alive for this bounded duration for browser proof; zero disables")
	out := flag.String("output", "", "optional JSON report path")
	falChild := flag.Bool("fal-namespace-child", false, "internal isolated fal container entry point")
	qualificationBinary := flag.String("qualification-binary", "", "optional -tags qualification gateway executable for credential crash-boundary proof")
	var live liveOptions
	flag.StringVar(&live.GatewayURL, "live-gateway-url", "", "explicit gateway inference origin for paid qualification")
	flag.StringVar(&live.ManagementURL, "live-management-url", "", "explicit authenticated management origin for paid qualification")
	flag.StringVar(&live.InferenceKeyFile, "live-inference-key-file", "", "private raw gateway inference key file; no ambient credentials")
	flag.StringVar(&live.AdminCookieFile, "live-admin-cookie-file", "", "private raw admin session value file")
	flag.StringVar(&live.CSRFFile, "live-csrf-file", "", "private raw admin CSRF token file")
	flag.StringVar(&live.RequestsFile, "live-cases-file", "", "private strict version-1 live operation case JSON file")
	flag.Int64Var(&live.SpendCeilingNano, "live-spend-ceiling-nanodollars", 0, "explicit positive aggregate worst-case USD nanodollar ceiling")
	flag.Parse()
	if *falChild {
		os.Exit(runFalNamespaceChild(*binary, *out))
	}
	rep := report{StartedAt: time.Now().UTC(), Mode: *mode, Scenario: *scenario, Binary: *binary}
	if err := initializeVerificationProgress(*out, rep); err != nil {
		rep.Results = append(rep.Results, result{"checkpoint/setup", "failed", "cannot initialize private qualification checkpoint", 0, nil})
		rep.Failed++
		finish(rep, *out)
		os.Exit(2)
	}
	add := func(r result) {
		verificationResult(r)
		rep.Results = append(rep.Results, r)
		switch r.Status {
		case "passed":
			rep.Passed++
		case "failed":
			rep.Failed++
		case "not-run":
			rep.NotRun++
		}
	}
	if *scenario == "live" {
		executeLive(add, *allowPaid, *connections, live)
		finish(rep, *out)
		if rep.Failed > 0 {
			os.Exit(1)
		}
		return
	}
	if *config != "" {
		add(result{"setup", "failed", "external configs are forbidden: qualification owns isolated data and services", 0, nil})
		finish(rep, *out)
		os.Exit(2)
	}
	valid := map[string]bool{"all": true, "smoke": true, "protocol": true, "docs-capture": true, "sdk": true, "browser": true, "security": true, "streaming": true, "resources": true, "governance": true, "cluster/governance": true, "credentials": true, "credential-lifecycle": true, "realtime": true, "realtime/bedrock": true, "operations": true, "operations/fal": true, "cost-safety": true, "packaging": true, "telemetry": true, "codex": true}
	valid["deep-admin"], valid["deep-protocol"] = true, true
	valid["browser"] = true
	if !valid[*scenario] {
		add(result{"setup", "failed", "unknown scenario: " + *scenario, 0, nil})
		finish(rep, *out)
		os.Exit(2)
	}
	if *binary == "" {
		add(result{"setup", "failed", "--binary is required; run against a built release process", 0, nil})
		finish(rep, *out)
		os.Exit(2)
	}
	if *mode != "standalone" && *mode != "cluster" {
		add(result{"setup", "failed", "--mode must be standalone or cluster", 0, nil})
		finish(rep, *out)
		os.Exit(2)
	}
	if *scenario == "cluster/governance" && *mode != "cluster" {
		add(result{"setup", "failed", "cluster/governance requires --mode cluster", 0, nil})
		finish(rep, *out)
		os.Exit(2)
	}
	if *scenario == "docs-capture" && *keepAlive <= 0 {
		add(result{"setup", "failed", "docs-capture requires a positive --keep-alive duration", 0, nil})
		finish(rep, *out)
		os.Exit(2)
	}
	if _, err := os.Stat(*binary); err != nil {
		add(result{"setup", "failed", fmt.Sprintf("binary unavailable: %v", err), 0, nil})
		finish(rep, *out)
		os.Exit(2)
	}
	var env *environment
	var telemetry *telemetryQualification
	var err error
	if *scenario == "telemetry" {
		telemetry, err = newTelemetryQualification(*mode)
		if err == nil {
			env = telemetry.env
		}
	} else {
		env, err = newEnvironment(*mode, *config, *fixtureMode)
	}
	if err != nil {
		add(result{"setup", "failed", err.Error(), 0, nil})
		finish(rep, *out)
		os.Exit(2)
	}
	env.qualificationBinary = *qualificationBinary
	if err := env.Start(context.Background(), *binary); err != nil {
		add(result{"setup", "failed", err.Error(), 0, nil})
		if diagnostics, diagnosticsErr := env.preserveDiagnostics(*out); diagnosticsErr == nil {
			rep.Diagnostics = diagnostics
		}
		env.Close()
		if telemetry != nil {
			telemetry.closeCollector()
		}
		finish(rep, *out)
		os.Exit(2)
	}
	add(result{"isolation", "passed", "owned temporary state isolated for this run and removed after shutdown", 0, map[string]any{"directory": env.root}})
	add(env.health())
	bootstrapResult := env.bootstrap()
	add(bootstrapResult)
	key := readKey(*keyFile)
	if key == "" {
		key = env.key
	}
	if key == "" && bootstrapResult.Status == "failed" {
		add(result{"seed", "failed", "automatic fixture seeding did not complete", 0, nil})
	}
	if *scenario == "all" || *scenario == "smoke" {
		add(env.authRejection())
	}
	if key == "" {
		for _, name := range []string{"protocol", "sdk", "browser", "security", "streaming", "resources", "governance", "cluster/governance", "credentials", "credential-lifecycle", "realtime", "realtime/bedrock", "operations", "operations/fal", "cost-safety", "deep-admin", "deep-protocol", "telemetry", "codex"} {
			if *scenario == "all" || *scenario == name {
				add(result{name, "failed", "required gateway key could not be seeded; scenario cannot execute", 0, nil})
			}
		}
	} else {
		if *scenario == "all" || *scenario == "protocol" || *scenario == "smoke" {
			add(env.chat(key))
		}
		if *scenario == "all" || *scenario == "security" {
			add(env.unknownField(key))
			add(env.largeLateModel(key))
			add(env.duplicateJSON(key))
		}
		if *scenario == "all" || *scenario == "resources" {
			add(env.resources(key))
		}
		if *scenario == "all" || *scenario == "governance" {
			add(env.concurrent(key))
			add(env.restartRecovery())
		}
		if *scenario == "all" || *scenario == "credentials" {
			add(env.credentialConfusion(key))
		}
		if *scenario == "all" || *scenario == "streaming" {
			add(env.streaming(key))
		}
		var focused func() []result
		switch *scenario {
		case "browser":
			focused = func() []result { return env.browserScenarios(*out) }
		case "deep-admin":
			focused = env.deepAdminScenarios
		case "deep-protocol":
			focused = env.deepProtocolScenarios
		case "credential-lifecycle":
			focused = func() []result {
				return append(env.credentialLifecycle(), env.refreshAdmissionScenarios()...)
			}
		case "realtime":
			focused = env.realtimeSuccess
		case "realtime/bedrock":
			focused = env.rtBedrockContracts
		case "operations":
			focused = env.operationsSuccess
		case "operations/fal":
			focused = env.falNamespace
		case "cost-safety":
			focused = env.costSafetyScenarios
		case "sdk":
			focused = env.extSDK
		case "codex":
			focused = func() []result { return []result{env.codexParityScenario()} }
		case "cluster/governance":
			focused = env.clusterScenarios
		case "telemetry":
			focused = func() []result { return telemetry.exercise(key) }
		}
		if focused != nil {
			verificationProgress(*scenario, "started", 0)
			for _, r := range focused() {
				add(r)
			}
		}
		if *scenario == "all" {
			for _, r := range env.extendedScenarios() {
				add(r)
			}
			for _, r := range env.costSafetyScenarios() {
				add(r)
			}
			for _, r := range env.deepAdminScenarios() {
				add(r)
			}
			for _, r := range env.deepProtocolScenarios() {
				add(r)
			}
			for _, r := range env.refreshAdmissionScenarios() {
				add(r)
			}
			if env.mode == "cluster" {
				for _, r := range env.clusterScenarios() {
					add(r)
				}
			}
		}
	}
	if *scenario == "all" || *scenario == "packaging" {
		for _, r := range env.packagingScenarios() {
			add(r)
		}
	}
	if *keepAlive > 0 {
		start := time.Now()
		storageState, storageErr := env.browserStorageState()
		if storageErr != nil {
			add(result{"browser-fixture", "failed", "could not write private authenticated browser storage state: " + storageErr.Error(), 0, nil})
		}
		fmt.Fprintf(os.Stderr, "browser-proof endpoints inference=http://%s management=http://%s config=%s fixture=%s storage_state=%s directory=%s\n", env.inference, env.management, env.config, env.fixture.server.URL, storageState, env.root)
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		timer := time.NewTimer(*keepAlive)
		select {
		case <-timer.C:
		case <-ctx.Done():
		}
		cancel()
		add(result{"keep-alive", "passed", "isolated gateway remained available for the requested bounded browser-proof window", time.Since(start).Milliseconds(), map[string]any{"duration": keepAlive.String(), "directory": env.root, "storage_state": storageState}})
	}
	if rep.Failed > 0 {
		if diagnostics, diagnosticsErr := env.preserveDiagnostics(*out); diagnosticsErr == nil {
			rep.Diagnostics = diagnostics
		}
	}
	env.Close()
	if telemetry != nil {
		add(telemetry.shutdownResult())
		telemetry.closeCollector()
	}
	if *scenario == "all" {
		for _, r := range runTelemetryQualification(*mode, *binary, *out) {
			add(r)
		}
	}
	finish(rep, *out)
	if rep.Failed > 0 {
		os.Exit(1)
	}
}

func (e *environment) browserStorageState() (string, error) {
	parts := strings.SplitN(e.cookie, "=", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", errors.New("authenticated session cookie is unavailable")
	}
	host, _, err := net.SplitHostPort(e.management)
	if err != nil || host == "" {
		return "", fmt.Errorf("management listener is not a host:port origin: %q", e.management)
	}
	state := map[string]any{
		"cookies": []map[string]any{{
			"name": parts[0], "value": parts[1], "domain": host, "path": "/",
			"expires": -1, "httpOnly": true, "secure": false, "sameSite": "Lax",
		}},
		"origins": []any{},
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	path := filepath.Join(e.root, "browser-storage-state.json")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	if _, err = file.Write(append(raw, '\n')); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err = file.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}
func (e *environment) preserveDiagnostics(output string) (string, error) {
	if e == nil || e.root == "" {
		return "", nil
	}
	entries, err := os.ReadDir(e.root)
	if err != nil {
		return "", err
	}
	parent := os.TempDir()
	prefix := "hoorific-verify-diagnostics-"
	if output != "" {
		parent = filepath.Dir(output)
		prefix = strings.TrimSuffix(filepath.Base(output), filepath.Ext(output)) + "-diagnostics-"
		relative, relativeErr := filepath.Rel(e.root, parent)
		if relativeErr == nil && (relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))) {
			parent = os.TempDir()
			prefix = "hoorific-verify-diagnostics-"
		}
	}
	if err = os.MkdirAll(parent, 0700); err != nil {
		return "", err
	}
	destination, err := os.MkdirTemp(parent, prefix)
	if err != nil {
		return "", err
	}
	allowed := func(name string) bool {
		switch name {
		case "serve.log", "refresh-crash.log", "fal-gateway.log", "podman.log", "cluster-cleanup.log", "cluster-infrastructure.json":
			return true
		default:
			return strings.HasPrefix(name, "container-command-") && strings.HasSuffix(name, ".log")
		}
	}
	copied := 0
	for _, entry := range entries {
		name := entry.Name()
		if !allowed(name) {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			continue
		}
		raw, readErr := os.ReadFile(filepath.Join(e.root, name))
		if readErr != nil {
			continue
		}
		if len(raw) > 2<<20 {
			raw = raw[:2<<20]
		}
		if writeErr := os.WriteFile(filepath.Join(destination, name), raw, 0600); writeErr != nil {
			_ = os.RemoveAll(destination)
			return "", writeErr
		}
		copied++
	}
	if copied == 0 {
		_ = os.RemoveAll(destination)
		return "", nil
	}
	return destination, nil
}

type environment struct {
	root, config, key     string
	mode                  string
	seed                  bool
	tenantID              string
	binary                string
	qualificationBinary   string
	inference, management string
	server                *exec.Cmd
	client                *http.Client
	fixture               *fixture
	cookie, csrf          string
	stop                  sync.Once
	infrastructureClose   func()
}

func newEnvironment(mode, supplied, fixtureMode string) (*environment, error) {
	root, err := os.MkdirTemp("", "hoorific-verify-")
	if err != nil {
		return nil, err
	}
	var infrastructureClose func()
	complete := false
	defer func() {
		if !complete {
			if infrastructureClose != nil {
				infrastructureClose()
			}
			_ = os.RemoveAll(root)
		}
	}()
	f := newFixture()
	inf, err := freeAddr()
	if err != nil {
		f.server.Close()
		return nil, err
	}
	mgmt, err := freeAddr()
	if err != nil {
		f.server.Close()
		return nil, err
	}
	cfg := supplied
	if cfg == "" {
		dsnFile, redisFile := "", ""
		if mode == "cluster" {
			dsnFile, redisFile, infrastructureClose, err = prepareCluster(root)
			if err != nil {
				f.server.Close()
				return nil, err
			}
		}
		key := make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			f.server.Close()
			return nil, err
		}
		keyPath := filepath.Join(root, "master.key")
		keyring, _ := json.Marshal(map[string]any{"current": "fixture", "keys": map[string]string{"fixture": base64.RawStdEncoding.EncodeToString(key)}})
		if err = os.WriteFile(keyPath, keyring, 0600); err != nil {
			f.server.Close()
			return nil, err
		}
		cfgPath := filepath.Join(root, "config.json")
		body := map[string]any{
			"schema_version": 1, "mode": mode, "data_dir": root,
			"listeners": map[string]any{"inference": inf, "management": mgmt},
			"storage": map[string]any{"sqlite": map[string]string{"path": func() string {
				if mode == "cluster" {
					return ""
				}
				return filepath.Join(root, "hoorific.db")
			}()}, "postgres": map[string]string{"dsn_file": dsnFile}},
			"coordination": map[string]any{"redis": map[string]string{"url_file": redisFile}},
			"encryption":   map[string]string{"key_file": keyPath},
			"oidc":         map[string]string{"issuer": "", "client_id": "", "client_secret_file": ""},
			"public_urls":  map[string]string{"inference": "http://" + inf}, "subscription_connectors": map[string]bool{"enabled": false},
		}
		raw, _ := json.MarshalIndent(body, "", "  ")
		if err = os.WriteFile(cfgPath, raw, 0600); err != nil {
			f.server.Close()
			return nil, err
		}
		cfg = cfgPath
	} else {
		raw, readErr := os.ReadFile(supplied)
		if readErr != nil {
			f.server.Close()
			return nil, fmt.Errorf("read supplied config: %w", readErr)
		}
		text := strings.ReplaceAll(string(raw), "$HOORIFIC_FIXTURE_URL", f.server.URL)
		text = strings.ReplaceAll(text, "${HOORIFIC_FIXTURE_URL}", f.server.URL)
		cfg = filepath.Join(root, "config.json")
		if err = os.WriteFile(cfg, []byte(text), 0600); err != nil {
			f.server.Close()
			return nil, err
		}
		var addresses struct {
			Listeners struct{ Inference, Management string } `json:"listeners"`
		}
		if json.Unmarshal([]byte(text), &addresses) == nil {
			if addresses.Listeners.Inference != "" {
				inf = addresses.Listeners.Inference
			}
			if addresses.Listeners.Management != "" {
				mgmt = addresses.Listeners.Management
			}
		}
	}
	f.mode.Store(fixtureMode)
	complete = true
	return &environment{root: root, config: cfg, mode: mode, seed: supplied == "", inference: inf, management: mgmt, client: &http.Client{Timeout: 15 * time.Second}, fixture: f, infrastructureClose: infrastructureClose}, nil
}
func freeAddr() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	a := l.Addr().String()
	l.Close()
	return a, nil
}
func (e *environment) Start(ctx context.Context, binary string) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	abs, err := filepath.Abs(binary)
	if err != nil {
		return err
	}
	e.binary = abs
	migrate := exec.CommandContext(ctx, abs, "migrate", "--config", e.config)
	migrate.Dir = e.root
	var mb bytes.Buffer
	migrate.Stdout, migrate.Stderr = &mb, &mb
	if err = migrate.Run(); err != nil {
		return fmt.Errorf("migrate failed: %w: %s", err, trim(mb.String()))
	}
	e.server = exec.Command(abs, "serve", "--config", e.config)
	e.server.Dir = e.root
	logf, err := os.OpenFile(filepath.Join(e.root, "serve.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	e.server.Stdout, e.server.Stderr = logf, logf
	if err = e.server.Start(); err != nil {
		logf.Close()
		return fmt.Errorf("serve failed: %w", err)
	}
	logf.Close()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		r, requestErr := e.client.Get("http://" + e.management + "/health/live")
		if requestErr == nil {
			r.Body.Close()
			if r.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("management health did not become live; see %s", filepath.Join(e.root, "serve.log"))
}
func (e *environment) health() result {
	started := time.Now()
	if e.management == "" || e.client == nil {
		return result{"health", "failed", "management health endpoint is not configured", 0, nil}
	}
	response, err := e.client.Get("http://" + e.management + "/health/ready")
	if err != nil {
		return result{"health", "failed", err.Error(), time.Since(started).Milliseconds(), nil}
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result{"health", "failed", fmt.Sprintf("readiness returned %d: %s", response.StatusCode, trim(string(body))), time.Since(started).Milliseconds(), nil}
	}
	return result{"health", "passed", "readiness returned 200 from the running process", time.Since(started).Milliseconds(), map[string]any{"status": response.StatusCode}}
}
func (e *environment) concurrent(key string) result {
	started := time.Now()
	const total = 16
	before := e.fixture.count()
	var wg sync.WaitGroup
	outcomes := make(chan error, total)
	body := []byte(`{"model":"assistant","messages":[{"role":"user","content":"Reply exactly OK"}],"max_tokens":16}`)
	for range total {
		wg.Add(1)
		go func() {
			defer wg.Done()
			response, err := e.do("concurrent", key, body)
			if err != nil {
				outcomes <- err
				return
			}
			responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, 2<<20))
			response.Body.Close()
			if readErr != nil {
				outcomes <- readErr
				return
			}
			if response.StatusCode < 200 || response.StatusCode >= 300 {
				outcomes <- fmt.Errorf("status %d", response.StatusCode)
				return
			}
			if !bytes.Contains(responseBody, []byte("OK")) {
				outcomes <- errors.New("response omitted fixture text")
				return
			}
			outcomes <- nil
		}()
	}
	wg.Wait()
	close(outcomes)
	good, failed := 0, 0
	for err := range outcomes {
		if err == nil {
			good++
		} else {
			failed++
		}
	}
	after := e.fixture.count()
	live := e.health()
	if live.Status != "passed" {
		return result{"governance/concurrency", "failed", "management health failed after concurrent requests", time.Since(started).Milliseconds(), map[string]any{"successful": good, "failed": failed}}
	}
	if good != total || failed != 0 || after-before != total {
		return result{"governance/concurrency", "failed", "not all concurrent requests returned validated fixture responses", time.Since(started).Milliseconds(), map[string]any{"expected": total, "successful": good, "failed": failed, "fixture_calls": after - before}}
	}
	return result{"governance/concurrency", "passed", "16 concurrent requests returned validated fixture responses while process remained live", time.Since(started).Milliseconds(), map[string]any{"successful": good, "failed": failed, "fixture_calls": after - before}}
}
func (e *environment) restartRecovery() result {
	started := time.Now()
	if e.server == nil || e.server.Process == nil {
		return result{"governance/restart", "failed", "served process was not started", 0, nil}
	}
	_ = e.server.Process.Kill()
	_ = e.server.Wait()
	if err := e.Start(context.Background(), e.binary); err != nil {
		return result{"governance/restart", "failed", "restart after child termination failed: " + err.Error(), time.Since(started).Milliseconds(), nil}
	}
	ready := e.health()
	if ready.Status != "passed" {
		return result{"governance/restart", "failed", "readiness did not recover after restart", time.Since(started).Milliseconds(), nil}
	}
	return result{"governance/restart", "passed", "migrate/serve recovered the isolated process after termination", time.Since(started).Milliseconds(), nil}
}
func (e *environment) Close() {
	e.stop.Do(func() {
		if e.server != nil && e.server.Process != nil && e.server.ProcessState == nil {
			_ = e.server.Process.Signal(os.Interrupt)
			done := make(chan struct{})
			go func() { _ = e.server.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(35 * time.Second):
				_ = e.server.Process.Kill()
				<-done
			}
		}
		if e.fixture != nil {
			e.fixture.server.Close()
		}
		if e.infrastructureClose != nil {
			e.infrastructureClose()
		}
		_ = os.RemoveAll(e.root)
	})
}
func (e *environment) bootstrap() result {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.server.Path, "admin", "bootstrap", "--config", e.config)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		evidence := map[string]any{"exit_error": err.Error(), "stderr": trim(stderr.String())}
		if ctx.Err() != nil {
			evidence["context_error"] = ctx.Err().Error()
		}
		if !e.seed {
			return result{"bootstrap", "not-run", "supplied config owns existing administration state; bootstrap seed was not attempted", time.Since(start).Milliseconds(), evidence}
		}
		return result{"bootstrap", "failed", "admin bootstrap failed; automatic fixture seeding cannot proceed", time.Since(start).Milliseconds(), evidence}
	}
	re := regexp.MustCompile(`(?m)([A-Za-z0-9_-]{32,})`)
	match := re.FindStringSubmatch(string(out))
	if len(match) == 0 {
		return result{"bootstrap", "failed", "bootstrap exited without a redeemable code", time.Since(start).Milliseconds(), nil}
	}
	payload, _ := json.Marshal(map[string]string{"code": match[1]})
	req, requestErr := http.NewRequest(http.MethodPost, "http://"+e.management+"/admin/api/v1/auth/bootstrap", bytes.NewReader(payload))
	if requestErr != nil {
		return result{"bootstrap", "failed", requestErr.Error(), time.Since(start).Milliseconds(), nil}
	}
	req.Header.Set("Content-Type", "application/json")
	response, requestErr := e.client.Do(req)
	if requestErr != nil {
		return result{"bootstrap", "failed", "bootstrap code redemption failed: " + requestErr.Error(), time.Since(start).Milliseconds(), nil}
	}
	responseBody, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result{"bootstrap", "failed", fmt.Sprintf("bootstrap redemption returned %d: %s", response.StatusCode, trim(string(responseBody))), time.Since(start).Milliseconds(), nil}
	}
	var bootstrapEnvelope struct {
		Principal core.Principal `json:"principal"`
	}
	if json.Unmarshal(responseBody, &bootstrapEnvelope) == nil {
		e.tenantID = bootstrapEnvelope.Principal.TenantID
	}
	if e.tenantID == "" {
		return result{"bootstrap", "failed", "bootstrap redemption returned no tenant principal", time.Since(start).Milliseconds(), nil}
	}
	for _, value := range response.Cookies() {
		if value.Name == "hoorific_session" {
			e.cookie = value.Name + "=" + value.Value
		}
	}
	if e.cookie == "" {
		return result{"bootstrap", "failed", "bootstrap redemption returned no session cookie", time.Since(start).Milliseconds(), nil}
	}
	sessionReq, _ := http.NewRequest(http.MethodGet, "http://"+e.management+"/admin/api/v1/session", nil)
	sessionReq.Header.Set("Cookie", e.cookie)
	sessionResp, sessionErr := e.client.Do(sessionReq)
	if sessionErr != nil {
		return result{"bootstrap", "failed", "session lookup failed: " + sessionErr.Error(), time.Since(start).Milliseconds(), nil}
	}
	var session struct {
		CSRFToken string `json:"csrf_token"`
	}
	sessionBody, _ := io.ReadAll(io.LimitReader(sessionResp.Body, 1<<20))
	sessionResp.Body.Close()
	if sessionResp.StatusCode != http.StatusOK || json.Unmarshal(sessionBody, &session) != nil || session.CSRFToken == "" {
		return result{"bootstrap", "failed", "session lookup returned no CSRF token", time.Since(start).Milliseconds(), nil}
	}
	e.csrf = session.CSRFToken
	if err := e.seedResources(); err != nil {
		return result{"bootstrap", "failed", "automatic fixture seeding failed: " + err.Error(), time.Since(start).Milliseconds(), nil}
	}
	return result{"bootstrap", "passed", "one-time code redeemed and deterministic tenant/connection/model/alias/key seeded through admin APIs", time.Since(start).Milliseconds(), map[string]any{"code_length": len(match[1]), "session_cookie": true, "fixture_url": e.fixture.server.URL}}
}
func (e *environment) seedResources() error {
	resources := []struct {
		kind, id string
		data     any
	}{
		{"tenants", "fixture-tenant", map[string]any{"name": "Deterministic fixture tenant", "enabled": true}},
		{"connections", "fixture-connection", map[string]any{"connector": "anthropic", "account_id": "fixture-account", "base_url": e.fixture.server.URL, "dedicated": true, "enabled": true, "settings": map[string]string{"allow_private": "true", "allowed_cidrs": "127.0.0.1/32"}}},
		{"models", "fixture-model", map[string]any{"connection_id": "fixture-connection", "upstream_id": "fixture-chat", "operations": []string{"generate"}, "input_modalities": []string{"text"}, "output_modalities": []string{"text"}, "features": map[string]string{"streaming": "supported"}, "context_limit": 32768, "output_limit": 1024, "provenance": "deterministic fixture", "price": map[string]any{"version": "fixture-v1", "input_per_million": 1000000000, "output_per_million": 2000000000, "maximum_unit_cost": 1000000, "unit_operation": "generate"}, "enabled": true}},
		{"model_aliases", "assistant", map[string]any{"model_ids": []string{"fixture-model"}, "description": "deterministic fixture alias", "enabled": true}},
		{"route_policies", "assistant", map[string]any{"alias": "assistant", "targets": []any{map[string]any{"connection_id": "fixture-connection", "model_id": "fixture-model", "priority": 1, "weight": 100}}, "fallback": false, "affinity": false}},
	}
	for _, resource := range resources {
		raw, _ := json.Marshal(map[string]any{"id": resource.id, "data": resource.data})
		req, err := http.NewRequest(http.MethodPost, "http://"+e.management+"/admin/api/v1/"+resource.kind, bytes.NewReader(raw))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Cookie", e.cookie)
		req.Header.Set("Origin", "http://"+e.management)
		req.Header.Set("X-CSRF-Token", e.csrf)
		resp, err := e.client.Do(req)
		if err != nil {
			return fmt.Errorf("%s: %w", resource.kind, err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("%s returned %d: %s", resource.kind, resp.StatusCode, trim(string(body)))
		}
	}
	credentialData, _ := json.Marshal(map[string]any{"data": map[string]any{"provider": "anthropic", "account_id": "fixture-account", "kind": "api_key", "secret": "fixture-upstream-secret"}})
	importReq, err := http.NewRequest(http.MethodPost, "http://"+e.management+"/admin/api/v1/connections/fixture-connection/import", bytes.NewReader(credentialData))
	if err != nil {
		return err
	}
	importReq.Header.Set("Content-Type", "application/json")
	importReq.Header.Set("Cookie", e.cookie)
	importReq.Header.Set("Origin", "http://"+e.management)
	importReq.Header.Set("X-CSRF-Token", e.csrf)
	importReq.Header.Set("If-Match", "1")
	importResp, err := e.client.Do(importReq)
	if err != nil {
		return fmt.Errorf("connection credential import: %w", err)
	}
	importBody, _ := io.ReadAll(io.LimitReader(importResp.Body, 1<<20))
	importResp.Body.Close()
	if importResp.StatusCode < 200 || importResp.StatusCode >= 300 {
		return fmt.Errorf("connection credential import returned %d: %s", importResp.StatusCode, trim(string(importBody)))
	}
	raw, _ := json.Marshal(map[string]any{"data": map[string]any{"name": "fixture key", "role": "operator", "permissions": []string{"inference:invoke", "connection:fixture-connection"}, "aliases": []string{"assistant"}, "connections": []string{"fixture-connection"}, "operations": []string{"generate"}, "portable": true, "native_account": false, "realtime": false}})
	issueReq, err := http.NewRequest(http.MethodPost, "http://"+e.management+"/admin/api/v1/api_keys/fixture-key/issue", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	issueReq.Header.Set("Content-Type", "application/json")
	issueReq.Header.Set("Cookie", e.cookie)
	issueReq.Header.Set("Origin", "http://"+e.management)
	issueReq.Header.Set("X-CSRF-Token", e.csrf)
	issueResp, err := e.client.Do(issueReq)
	if err != nil {
		return fmt.Errorf("api_keys issue: %w", err)
	}
	issueBody, _ := io.ReadAll(io.LimitReader(issueResp.Body, 1<<20))
	issueResp.Body.Close()
	if issueResp.StatusCode < 200 || issueResp.StatusCode >= 300 {
		return fmt.Errorf("api_keys issue returned %d: %s", issueResp.StatusCode, trim(string(issueBody)))
	}
	var issued struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(issueBody, &issued) != nil || issued.Token == "" {
		return errors.New("API key issue returned no one-time token")
	}
	e.key = issued.Token
	if e.key == "" {
		return errors.New("API key issue returned no one-time secret")
	}
	return nil
}
func (e *environment) authRejection() result {
	start := time.Now()
	body := bytes.NewReader([]byte(`{"model":"assistant","messages":[{"role":"user","content":"unauthenticated"}],"max_tokens":1}`))
	req, err := http.NewRequest(http.MethodPost, "http://"+e.inference+"/v1/chat/completions", body)
	if err != nil {
		return result{"auth-rejection", "failed", err.Error(), time.Since(start).Milliseconds(), nil}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return result{"auth-rejection", "failed", err.Error(), time.Since(start).Milliseconds(), nil}
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return result{"auth-rejection", "failed", readErr.Error(), time.Since(start).Milliseconds(), nil}
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return result{"auth-rejection", "failed", fmt.Sprintf("unauthenticated POST returned %d, expected 401: %s", resp.StatusCode, trim(string(raw))), time.Since(start).Milliseconds(), map[string]any{"status": resp.StatusCode}}
	}
	return result{"auth-rejection", "passed", "valid chat-completions POST without credentials returned 401", time.Since(start).Milliseconds(), map[string]any{"status": resp.StatusCode}}
}
func (e *environment) chat(key string) result {
	body := []byte(`{"model":"assistant","messages":[{"role":"user","content":"Reply exactly OK"}],"max_tokens":16}`)
	r := e.post("chat", key, body, func(resp *http.Response, b []byte) error {
		if resp.StatusCode != 200 {
			return fmt.Errorf("chat returned %d: %s", resp.StatusCode, trim(string(b)))
		}
		if !bytes.Contains(b, []byte("OK")) {
			return errors.New("chat response omitted expected fixture text")
		}
		return nil
	})
	if r.Status == "passed" {
		call := e.fixture.last()
		if call.CredentialHeader != "X-Api-Key" || call.CredentialCount != 1 || call.CredentialConflict || call.Credential != "fixture-upstream-secret" {
			r.Status, r.Detail = "failed", "gateway credential was not replaced with configured upstream credential"
			r.Evidence = map[string]any{"path": call.Path, "credential_present": call.Credential != ""}
		} else {
			r.Evidence = map[string]any{"path": call.Path, "upstream_credential_replaced": true}
		}
	}
	return r
}
func (e *environment) resources(key string) result {
	started := time.Now()
	start := e.fixture.count()
	req, err := http.NewRequest(http.MethodGet, "http://"+e.inference+"/v1/files", nil)
	if err != nil {
		return result{"resources", "failed", err.Error(), 0, nil}
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := e.client.Do(req)
	if err != nil {
		return result{"resources", "failed", err.Error(), time.Since(started).Milliseconds(), nil}
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		return result{"resources", "failed", fmt.Sprintf("root native file list returned %d; expected connection-bound rejection", resp.StatusCode), time.Since(started).Milliseconds(), nil}
	}
	end := e.fixture.count()
	if end != start {
		return result{"resources", "failed", "root native file list reached upstream", time.Since(started).Milliseconds(), map[string]any{"fixture_calls_before": start, "fixture_calls_after": end}}
	}
	return result{"resources", "passed", "root native resource access was rejected without upstream dispatch", time.Since(started).Milliseconds(), map[string]any{"status": resp.StatusCode, "fixture_calls": start, "response": trim(string(body))}}
}
func (e *environment) unknownField(key string) result {
	before := e.fixture.count()
	body := []byte(`{"model":"assistant","messages":[{"role":"user","content":"x"}],"store":true}`)
	r := e.post("scope-store", key, body, func(resp *http.Response, b []byte) error {
		if resp.StatusCode != 400 {
			return fmt.Errorf("store:true returned %d, expected 400 before dispatch", resp.StatusCode)
		}
		if !bytes.Contains(b, []byte("connection_required")) {
			return fmt.Errorf("missing connection_required code: %s", trim(string(b)))
		}
		return nil
	})
	after := e.fixture.count()
	if r.Status == "passed" && after != before {
		r.Status, r.Detail = "failed", "scope rejection dispatched to upstream"
		r.Evidence = map[string]any{"fixture_calls_before": before, "fixture_calls_after": after}
	}
	return r
}
func (e *environment) largeLateModel(key string) result {
	before := e.fixture.count()
	body := append([]byte(`{"messages":[{"role":"user","content":"x"}],"padding":"`), bytes.Repeat([]byte{'x'}, 300*1024)...)
	body = append(body, []byte(`","model":"assistant"}`)...)
	r := e.post("late-model", key, body, func(resp *http.Response, b []byte) error {
		if resp.StatusCode < 400 || resp.StatusCode >= 500 {
			return fmt.Errorf("late model returned %d, expected client rejection", resp.StatusCode)
		}
		return nil
	})
	after := e.fixture.count()
	if r.Status == "passed" && after != before {
		r.Status, r.Detail = "failed", "late model rejection dispatched to upstream"
		r.Evidence = map[string]any{"fixture_calls_before": before, "fixture_calls_after": after}
	}
	return r
}
func (e *environment) credentialConfusion(key string) result {
	started := time.Now()
	before := e.fixture.count()
	body := []byte(`{"model":"assistant","messages":[{"role":"user","content":"x"}],"max_tokens":1}`)
	req, err := http.NewRequest(http.MethodPost, "http://"+e.inference+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return result{"credentials", "failed", err.Error(), 0, nil}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Api-Key", "contradictory")
	resp, err := e.client.Do(req)
	if err != nil {
		return result{"credentials", "failed", err.Error(), time.Since(started).Milliseconds(), nil}
	}
	responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	after := e.fixture.count()
	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		return result{"credentials", "failed", fmt.Sprintf("contradictory credentials returned %d", resp.StatusCode), time.Since(started).Milliseconds(), nil}
	}
	if after != before {
		return result{"credentials", "failed", "contradictory credentials reached upstream", time.Since(started).Milliseconds(), map[string]any{"before": before, "after": after}}
	}
	return result{"credentials", "passed", "contradictory gateway credentials rejected before dispatch", time.Since(started).Milliseconds(), map[string]any{"status": resp.StatusCode, "body": trim(string(responseBody))}}
}
func (e *environment) duplicateJSON(key string) result {
	started := time.Now()
	before := e.fixture.count()
	body := []byte(`{"model":"assistant","model":"other","messages":[{"role":"user","content":"x"}]}`)
	r := e.post("duplicate-json", key, body, func(resp *http.Response, b []byte) error {
		if resp.StatusCode < 400 || resp.StatusCode >= 500 {
			return fmt.Errorf("duplicate model keys returned %d", resp.StatusCode)
		}
		return nil
	})
	after := e.fixture.count()
	if r.Status == "passed" && after != before {
		r.Status, r.Detail = "failed", "duplicate key request reached upstream"
		r.Evidence = map[string]any{"before": before, "after": after}
	}
	r.DurationMS = time.Since(started).Milliseconds()
	return r
}
func (e *environment) streaming(key string) result {
	body := []byte(`{"model":"assistant","messages":[{"role":"user","content":"Reply exactly OK"}],"max_tokens":16,"stream":true}`)
	start := time.Now()
	r, err := e.do("streaming", key, body)
	if err != nil {
		return result{"streaming", "failed", err.Error(), time.Since(start).Milliseconds(), nil}
	}
	defer r.Body.Close()
	b, ttfb, err := readBodyWithTTFB(r.Body, 2<<20, start)
	if err != nil {
		return result{"streaming", "failed", err.Error(), time.Since(start).Milliseconds(), nil}
	}
	if r.StatusCode != 200 {
		return result{"streaming", "failed", fmt.Sprintf("status %d: %s", r.StatusCode, trim(string(b))), time.Since(start).Milliseconds(), nil}
	}
	if !bytes.Contains(b, []byte("[DONE]")) {
		return result{"streaming", "failed", "stream omitted terminal [DONE]", time.Since(start).Milliseconds(), nil}
	}
	return result{"streaming", "passed", "actual gateway stream contained terminal marker", time.Since(start).Milliseconds(), map[string]any{"bytes": len(b), "ttfb_ms": ttfb.Milliseconds()}}
}
func (e *environment) get(name, url string, h func(*http.Request), check func(*http.Response, []byte) error) result {
	start := time.Now()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if h != nil {
		h(req)
	}
	r, err := e.client.Do(req)
	if err != nil {
		return result{name, "failed", err.Error(), time.Since(start).Milliseconds(), nil}
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err = check(r, b); err != nil {
		return result{name, "failed", err.Error(), time.Since(start).Milliseconds(), nil}
	}
	return result{name, "passed", "observed actual process response", time.Since(start).Milliseconds(), map[string]any{"status": r.StatusCode}}
}
func (e *environment) post(name, key string, b []byte, check func(*http.Response, []byte) error) result {
	start := time.Now()
	r, err := e.do(name, key, b)
	if err != nil {
		return result{name, "failed", err.Error(), time.Since(start).Milliseconds(), nil}
	}
	defer r.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err = check(r, out); err != nil {
		return result{name, "failed", err.Error(), time.Since(start).Milliseconds(), nil}
	}
	return result{name, "passed", "observed actual process response", time.Since(start).Milliseconds(), map[string]any{"status": r.StatusCode}}
}
func (e *environment) do(name, key string, b []byte) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, "http://"+e.inference+"/v1/chat/completions", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	return e.client.Do(req)
}
func readKey(path string) string {
	if path == "" {
		return strings.TrimSpace(os.Getenv("HOORIFIC_VERIFY_KEY"))
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
func finish(rep report, path string) {
	rep.Results = append([]result(nil), rep.Results...)
	raw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "encode report:", err)
		os.Exit(2)
	}
	fmt.Print(string(raw) + "\n")
	if path != "" {
		if err = os.MkdirAll(filepath.Dir(path), 0700); err == nil {
			err = os.WriteFile(path, raw, 0600)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "write report:", err)
			os.Exit(2)
		}
	}
}
func trim(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 1000 {
		return s[:1000] + "..."
	}
	return s
}
