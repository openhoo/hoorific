//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"debug/buildinfo"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
	"hoorific/internal/core"
	"hoorific/internal/store"
)

// Suite prerequisites (build separately; the suite never builds or pulls):
//
//	go build -o .artifacts/hoorific ./cmd/hoorific
//	go build -o .artifacts/bench-runner ./tools/bench
//	podman pull docker.io/library/postgres:17
//	podman pull docker.io/library/redis:7
//	.artifacts/bench-runner --suite --binary .artifacts/hoorific --output .artifacts/bench
//
// Linux, /proc, util-linux taskset, rootless local Podman, and four allowed CPUs
// are mandatory. Artifacts contain ephemeral credentials and must remain private.
// Each invocation creates a fresh suite-* directory; nothing is overwritten.
const suitePostgresImage = "docker.io/library/postgres:17"
const suiteRedisImage = "docker.io/library/redis:7"

type suiteEntry struct {
	Deployment string `json:"deployment"`
	Route      string `json:"route"`
	State      string `json:"state"`
	Directory  string `json:"directory"`
	Error      string `json:"error,omitempty"`
}

type suiteLifecycle struct {
	dir, binary, runner, cpus string
	env                       []string
	sequence                  int
	entries                   []suiteEntry
	profile string
}

// runSuite runs the existing child CLI four times. Each child owns its fixture
// and runs every workload, both direct/gateway targets, and all three runs.
// A setup or child failure never removes another matrix entry or its evidence.
func runSuite(parent context.Context, binary, output, profile string) (result error) {
	if parent == nil {
		return errors.New("suite requires a context")
	}
	if profile != "http" && profile != "tls" {
		return errors.New("--upstream-transport must be http or tls")
	}
	if err := os.MkdirAll(output, 0700); err != nil {
		return err
	}
	root, err := filepath.Abs(output)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp(root, "suite-"+profile+"-")
	if err != nil {
		return err
	}
	s := &suiteLifecycle{dir: dir, profile: profile}
	for _, mode := range []string{"standalone", "cluster"} {
		for _, route := range []string{"native", "translated"} {
			s.entries = append(s.entries, suiteEntry{Deployment: mode, Route: route, State: "not_started", Directory: filepath.Join(dir, mode+"-"+route)})
		}
	}
	defer func() {
		for i := range s.entries {
			if s.entries[i].State == "not_started" {
				s.entries[i].State = "unmet_prerequisite_or_cancelled"
				if result != nil {
					s.entries[i].Error = result.Error()
				}
			}
		}
		result = errors.Join(result, s.manifest())
		fmt.Fprintln(os.Stderr, "Suite artifacts:", dir)
	}()
	signals, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(signals, 6*time.Hour)
	defer cancel()
	if runtime.GOOS != "linux" {
		return errors.New("suite requires Linux /proc and taskset")
	}
	s.binary, err = filepath.Abs(binary)
	if err != nil {
		return err
	}
	st, err := os.Stat(s.binary)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0111 == 0 {
		return errors.New("--binary must be a built executable gateway")
	}
	s.runner, err = os.Executable()
	if err != nil {
		return err
	}
	var allowed unix.CPUSet
	if err = unix.SchedGetaffinity(0, &allowed); err != nil {
		return err
	}
	selected := []string{"0", "2", "4", "6"}
	for _, cpu := range []int{0, 2, 4, 6} {
		if !allowed.IsSet(cpu) {
			return fmt.Errorf("suite requires physical-core affinity CPU %d; inherited affinity does not permit it", cpu)
		}
	}
	s.cpus = strings.Join(selected, ",")
	// Explicitly remove inherited service routing, proxy and Go tuning settings.
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if strings.HasPrefix(name, "BENCH_") || strings.HasPrefix(name, "PG") || strings.HasPrefix(name, "REDIS") || strings.HasPrefix(name, "GO") || strings.HasPrefix(name, "SSL_CERT") || strings.Contains(strings.ToUpper(name), "PROXY") || strings.HasPrefix(name, "CONTAINER_") || strings.HasPrefix(name, "DOCKER_") {
			continue
		}
		s.env = append(s.env, value)
	}
	s.env = append(s.env, "GOMAXPROCS=4", "GOTOOLCHAIN=local")
	for _, tool := range []string{"taskset", "podman"} {
		if _, err = exec.LookPath(tool); err != nil {
			return err
		}
	}
	if err = s.inventory(ctx); err != nil {
		return err
	}
	if err = s.manifest(); err != nil {
		return err
	}
	for i := range s.entries {
		if ctx.Err() != nil {
			return errors.Join(result, ctx.Err())
		}
		entry := &s.entries[i]
		entry.State = "running"
		if err = s.manifest(); err != nil {
			return errors.Join(result, err)
		}
		e := s.runEntry(ctx, *entry)
		entry.State = "complete"
		if e != nil {
			entry.State = "failed"
			entry.Error = e.Error()
			result = errors.Join(result, fmt.Errorf("%s/%s: %w", entry.Deployment, entry.Route, e))
		}
		if err = s.manifest(); err != nil {
			return errors.Join(result, err)
		}
	}
	return result
}

func suiteJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err = os.WriteFile(temporary, append(b, '\n'), 0600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}
func (s *suiteLifecycle) manifest() error {
	return suiteJSON(filepath.Join(s.dir, "suite.json"), map[string]any{"entries": s.entries, "cpus": s.cpus, "gomaxprocs": 4, "runs_per_child": 3, "duration": "30s", "warmup": "5s", "gateway_binary": s.binary, "runner_binary": s.runner, "cluster_scope": "one measured gateway process with isolated PostgreSQL and Redis coordination", "upstream_transport": s.profile, "upstream_max_conns_per_host": 1024, "upstream_capacity_evidence": "bootstrap configuration; observed live capacity is separately asserted by each soak's active_stalls_at_measurement_end, not inferred from configuration", "transport_contract": "matched direct and gateway upstream; client-to-gateway HTTP; TLS bridge backend HTTP; separate profiles are separate suites", "transport_evidence": "entry transport-proof.json records provisioning, bridge-transport.json records aggregate inbound bridge observations only", "sqlite_evidence_scope": "observer opened with actual store.Open FULL DSN; not a live gateway-connection PRAGMA measurement", "stream_lifetime": "gateway source contract: 10 minutes; child request/session deadlines explicitly bounded"})
}

// Every command has an argv artifact, a log, a deadline, and group cancellation.
// podman is forced local even if the user's configuration normally uses remote.
func (s *suiteLifecycle) command(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	s.sequence++
	label := filepath.Join(s.dir, fmt.Sprintf("command-%03d", s.sequence))
	if name == "podman" {
		args = append([]string{"--remote=false"}, args...)
	}
	recordArgs := append([]string(nil), args...)
	for i := 0; i+1 < len(recordArgs); i++ {
		if recordArgs[i] == "-a" || recordArgs[i] == "--requirepass" {
			recordArgs[i+1] = "[REDACTED]"
		}
	}
	if err := suiteJSON(label+".json", map[string]any{"executable": name, "args": recordArgs, "timeout": timeout.String(), "gomaxprocs": 4}); err != nil {
		return nil, err
	}
	log, err := os.OpenFile(label+".log", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	defer log.Close()
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(bounded, name, args...)
	cmd.Env = s.env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 3 * time.Second
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	var out bytes.Buffer
	cmd.Stdout = io.MultiWriter(log, &out)
	cmd.Stderr = log
	err = cmd.Run()
	if err != nil {
		err = fmt.Errorf("%s failed (see %s.log): %w", name, label, err)
	}
	return out.Bytes(), err
}

type suiteProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
	log  *os.File
}

func (s *suiteLifecycle) launch(ctx context.Context, dir string, extraEnv []string, executable string, args ...string) (*suiteProcess, error) {
	argv := append([]string{"--cpu-list", s.cpus, executable}, args...)
	if err := suiteJSON(filepath.Join(dir, "process.json"), map[string]any{"executable": "taskset", "args": argv, "gomaxprocs": 4, "extra_environment": extraEnv}); err != nil {
		return nil, err
	}
	log, err := os.OpenFile(filepath.Join(dir, "process.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "taskset", argv...)
	cmd.Env = append(append([]string{}, s.env...), extraEnv...)
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 35 * time.Second
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	if err = cmd.Start(); err != nil {
		log.Close()
		return nil, err
	}
	p := &suiteProcess{cmd: cmd, done: make(chan struct{}), log: log}
	go func() { p.err = cmd.Wait(); close(p.done) }()
	return p, nil
}
func (p *suiteProcess) close() error {
	defer p.log.Close()
	select {
	case <-p.done:
		return nil
	default:
	}
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM)
	timer := time.NewTimer(35 * time.Second)
	defer timer.Stop()
	select {
	case <-p.done:
		return nil
	case <-timer.C:
	}
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	select {
	case <-p.done:
		return errors.New("process required forced shutdown")
	case <-time.After(5 * time.Second):
		return errors.New("process reap exceeded shutdown bound")
	}
}

func (s *suiteLifecycle) inventory(ctx context.Context) error {
	for _, path := range []string{"/proc/cpuinfo", "/proc/meminfo", "/proc/version", "/proc/self/status", "/proc/self/mountinfo"} {
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		if e = os.WriteFile(filepath.Join(s.dir, strings.ReplaceAll(strings.TrimPrefix(path, "/"), "/", "-")+".txt"), b, 0600); e != nil {
			return e
		}
	}
	for name, path := range map[string]string{"gateway": s.binary, "runner": s.runner} {
		f, e := os.Open(path)
		if e != nil {
			return e
		}
		hash := sha256.New()
		_, e = io.Copy(hash, f)
		f.Close()
		if e != nil {
			return e
		}
		info, e := buildinfo.ReadFile(path)
		if e != nil {
			return fmt.Errorf("%s Go build identity: %w", name, e)
		}
		if e = suiteJSON(filepath.Join(s.dir, name+"-build.json"), map[string]any{"path": path, "sha256": hex.EncodeToString(hash.Sum(nil)), "build": info}); e != nil {
			return e
		}
	}
	_, err := s.command(ctx, 10*time.Second, "taskset", "--version")
	return err
}

// The affinity of every extant thread must equal the requested four-CPU set.
func (s *suiteLifecycle) affinity(dir string, p *suiteProcess) error {
	root := fmt.Sprintf("/proc/%d", p.cmd.Process.Pid)
	tasks, err := os.ReadDir(root + "/task")
	if err != nil {
		return err
	}
	evidence := map[string]string{}
	var expected unix.CPUSet
	for _, v := range strings.Split(s.cpus, ",") {
		n, e := strconv.Atoi(v)
		if e != nil {
			return e
		}
		expected.Set(n)
	}
	for _, task := range tasks {
		n, e := strconv.Atoi(task.Name())
		if e != nil {
			return e
		}
		var got unix.CPUSet
		if e = unix.SchedGetaffinity(n, &got); e != nil {
			if errors.Is(e, syscall.ESRCH) {
				continue
			}
			return e
		}
		if got != expected {
			return fmt.Errorf("thread %d affinity differs from four-CPU requirement", n)
		}
		data, e := os.ReadFile(root + "/task/" + task.Name() + "/status")
		if e != nil {
			if os.IsNotExist(e) {
				continue
			}
			return e
		}
		evidence[task.Name()] = string(data)
	}
	if len(evidence) == 0 {
		return errors.New("no live threads for affinity evidence")
	}
	environment, err := os.ReadFile(root + "/environ")
	if err != nil {
		return err
	}
	if !bytes.Contains(append([]byte{0}, environment...), []byte("\x00GOMAXPROCS=4\x00")) {
		return errors.New("process environment does not enforce GOMAXPROCS=4")
	}
	return suiteJSON(filepath.Join(dir, "affinity.json"), evidence)
}

func suiteAddress() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	a := l.Addr().String()
	err = l.Close()
	return a, err
}

func (s *suiteLifecycle) runEntry(ctx context.Context, entry suiteEntry) (result error) {
	dir := entry.Directory
	if err := os.Mkdir(dir, 0700); err != nil {
		return err
	}
	cfg := core.BootstrapConfig{SchemaVersion: 1, Mode: entry.Deployment, DataDir: filepath.Join(dir, "data")}
	cfg.Transport.MaxConnsPerHost = 1024
	if err := os.Mkdir(cfg.DataDir, 0700); err != nil {
		return err
	}
	var err error
	cfg.Listeners.Inference, err = suiteAddress()
	if err != nil {
		return err
	}
	cfg.Listeners.Management, err = suiteAddress()
	if err != nil {
		return err
	}
	cfg.PublicURLs = map[string]string{"inference": "http://" + cfg.Listeners.Inference}
	// Port reservations cannot cross exec; bind/readiness failures remain failures.
	fixture, err := suiteAddress()
	if err != nil {
		return err
	}
	cfg.Encryption.KeyFile = filepath.Join(dir, "master-key.json")
	key := make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		return err
	}
	if err = suiteJSON(cfg.Encryption.KeyFile, map[string]any{"current": "bench", "keys": map[string]string{"bench": base64.StdEncoding.EncodeToString(key)}}); err != nil {
		return err
	}
	if entry.Deployment == "standalone" {
		cfg.Storage.SQLite.Path = filepath.Join(cfg.DataDir, "gateway.sqlite")
	} else {
		cleanup, e := s.infrastructure(ctx, dir, &cfg)
		if cleanup != nil {
			defer func() { result = errors.Join(result, cleanup()) }()
		}
		if e != nil {
			return e
		}
	}
	configPath := filepath.Join(dir, "config.json")
	if err = suiteJSON(configPath, cfg); err != nil {
		return err
	}
	if _, err = s.command(ctx, 45*time.Second, "taskset", "--cpu-list", s.cpus, s.binary, "migrate", "--config", configPath); err != nil {
		return err
	}
	// Bootstrap must precede serve: both CLI operations take the data-dir lock.
	code, err := s.command(ctx, 45*time.Second, "taskset", "--cpu-list", s.cpus, s.binary, "admin", "bootstrap", "--config", configPath, "--tenant", "bench", "--subject", "bench-owner")
	if err != nil {
		return err
	}
	upstream, ca := "http://"+fixture, ""
	var gatewayEnv []string
	if s.profile == "tls" {
		var closeBridge func() error
		upstream, ca, closeBridge, err = suiteTLSBridge(ctx, dir, fixture)
		if err != nil {
			return err
		}
		defer func() { result = errors.Join(result, closeBridge()) }()
		gatewayEnv = []string{"SSL_CERT_FILE=" + ca}
	}
	gatewayDir := filepath.Join(dir, "gateway")
	if err = os.Mkdir(gatewayDir, 0700); err != nil {
		return err
	}
	gateway, err := s.launch(ctx, gatewayDir, gatewayEnv, s.binary, "serve", "--config", configPath)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, gateway.close()) }()
	client, closeClient := newClient(5 * time.Second)
	defer closeClient()
	management := "http://" + cfg.Listeners.Management
	if err = suiteReady(ctx, client, management+"/health/ready", gateway); err != nil {
		return err
	}
	if err = s.affinity(gatewayDir, gateway); err != nil {
		return err
	}
	api := suiteAdmin{ctx: ctx, client: client, origin: management, dir: dir}
	if err = api.bootstrap(strings.TrimSpace(string(code))); err != nil {
		return err
	}
	alias := "bench-" + entry.Route
	token, err := api.provision(alias, entry.Route, upstream)
	if err != nil {
		return err
	}
	before, err := suiteSQL(ctx, dir, "before", cfg)
	if err != nil {
		return err
	}
	childDir := filepath.Join(dir, "child")
	if err = os.Mkdir(childDir, 0700); err != nil {
		return err
	}
	keyFile := filepath.Join(dir, "gateway.key")
	urlFile := filepath.Join(dir, "gateway.url")
	if err = os.WriteFile(keyFile, []byte(token), 0600); err != nil {
		return err
	}
	if err = os.WriteFile(urlFile, []byte("http://"+cfg.Listeners.Inference+"/v1/chat/completions"), 0600); err != nil {
		return err
	}
	caDigest := ""
	if ca != "" {
		caBytes, readErr := os.ReadFile(ca)
		if readErr != nil {
			return readErr
		}
		caDigest = fmt.Sprintf("%x", sha256.Sum256(caBytes))
	}
	proofFile := filepath.Join(dir, "transport-proof.json")
	if err = suiteJSON(proofFile, map[string]any{
		"profile": s.profile,
		"direct_endpoint": upstream + "/v1/chat/completions",
		"gateway_endpoint": "http://" + cfg.Listeners.Inference + "/v1/chat/completions",
		"gateway_upstream": upstream + "/v1",
		"fixture_origin": "http://" + fixture,
		"ca_file": ca, "ca_sha256": caDigest,
		"bridge_present": s.profile == "tls",
		"source": "suite-owned provisioning",
	}); err != nil {
		return err
	}
	childCtx, cancel := context.WithTimeout(ctx, 90*time.Minute)
	defer cancel()
	child, err := s.launch(childCtx, childDir, nil, s.runner, "--suite=false", "--upstream-transport", s.profile, "--transport-proof-file", proofFile, "--deployment", entry.Deployment, "--route", entry.Route, "--gateway-pid", strconv.Itoa(gateway.cmd.Process.Pid), "--url-file", urlFile, "--key-file", keyFile, "--fixture-listen", fixture, "--model", alias, "--binary", s.binary, "--output", childDir, "--duration", "30s", "--warmup", "5s", "--session-timeout", "85m")
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, child.close()) }()
	// The fixture is child-owned; its successful TCP bind also proves taskset exec
	// has finished before the child's environment/affinity is inspected.
	if err = suiteFixtureReady(childCtx, fixture, child); err != nil {
		result = errors.Join(result, err)
	} else if err = s.affinity(childDir, child); err != nil {
		result = errors.Join(result, err)
		cancel()
	}
	select {
	case <-child.done:
		result = errors.Join(result, child.err)
	case <-gateway.done:
		result = errors.Join(result, fmt.Errorf("gateway exited during matrix entry: %v", gateway.err))
		cancel()
		result = errors.Join(result, child.close())
	case <-childCtx.Done():
		result = errors.Join(result, childCtx.Err())
		result = errors.Join(result, child.close())
	}
	// Observation and teardown use fresh bounded contexts even after a signal.
	evidenceCtx, evidenceCancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer evidenceCancel()
	result = errors.Join(result, s.affinity(gatewayDir, gateway))
	after, e := suiteSQL(evidenceCtx, dir, "after", cfg)
	result = errors.Join(result, e)
	if e == nil && (after["attempts"] <= before["attempts"] || after["usage_ledger"] <= before["usage_ledger"]) {
		result = errors.Join(result, errors.New("durable attempt/usage ledger did not increase"))
	}
	if e = childReportComplete(childDir); e != nil {
		result = errors.Join(result, e)
	}
	// Stop writer before final persistent SQL inspection; this is not a crash test.
	result = errors.Join(result, gateway.close())
	_, e = suiteSQL(evidenceCtx, dir, "after-shutdown", cfg)
	result = errors.Join(result, e)
	return result
}

func suiteReady(ctx context.Context, client *http.Client, address string, p *suiteProcess) error {
	wait, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var last error
	for {
		req, e := http.NewRequestWithContext(wait, http.MethodGet, address, nil)
		if e != nil {
			return e
		}
		resp, e := client.Do(req)
		if e == nil {
			_, e = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if e == nil && resp.StatusCode == 200 {
				return nil
			}
			last = fmt.Errorf("readiness HTTP %d: %v", resp.StatusCode, e)
		} else {
			last = e
		}
		select {
		case <-wait.Done():
			return fmt.Errorf("readiness deadline: %w (%v)", wait.Err(), last)
		case <-p.done:
			return fmt.Errorf("gateway exited before ready: %v", p.err)
		case <-time.After(200 * time.Millisecond):
		}
	}
}
func suiteFixtureReady(ctx context.Context, address string, p *suiteProcess) error {
	wait, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		conn, e := (&net.Dialer{Timeout: time.Second}).DialContext(wait, "tcp", address)
		if e == nil {
			conn.Close()
			return nil
		}
		select {
		case <-wait.Done():
			return wait.Err()
		case <-p.done:
			return fmt.Errorf("child exited before fixture readiness: %v", p.err)
		case <-time.After(50 * time.Millisecond):
		}
	}
}
func childReportComplete(dir string) error {
	b, err := os.ReadFile(filepath.Join(dir, "results.json"))
	if err != nil {
		return err
	}
	var r struct {
		State   string `json:"state"`
		Results []struct {
			Run      int    `json:"run"`
			Target   string `json:"target"`
			Workload string `json:"workload"`
		} `json:"results"`
	}
	if err = json.Unmarshal(b, &r); err != nil {
		return err
	}
	expected := map[string]bool{}
	for run := 1; run <= 3; run++ {
		for _, w := range workloads() {
			for _, target := range []string{"direct", "gateway"} {
				expected[fmt.Sprintf("%d/%s/%s", run, target, w.Name)] = false
			}
		}
	}
	for _, v := range r.Results {
		k := fmt.Sprintf("%d/%s/%s", v.Run, v.Target, v.Workload)
		seen, ok := expected[k]
		if !ok || seen {
			return fmt.Errorf("unexpected or duplicate child case %s", k)
		}
		expected[k] = true
	}
	for k, seen := range expected {
		if !seen {
			return fmt.Errorf("missing child case %s (state %s)", k, r.State)
		}
	}
	if r.State != "complete" {
		return fmt.Errorf("child preserved failures: %s", r.State)
	}
	return nil
}

// No externally supplied DSN, Redis URL, socket, network, or container is used.
// Images must already be cached. Published ports bind only to loopback.
func (s *suiteLifecycle) infrastructure(ctx context.Context, dir string, cfg *core.BootstrapConfig) (func() error, error) {
	if os.Geteuid() == 0 {
		return nil, errors.New("cluster suite requires rootless Podman (non-root UID)")
	}
	info, err := s.command(ctx, 15*time.Second, "podman", "info", "--format", "{{.Host.Security.Rootless}}")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(info)) != "true" {
		return nil, errors.New("Podman did not report rootless=true")
	}
	if _, err = s.command(ctx, 15*time.Second, "podman", "version"); err != nil {
		return nil, err
	}
	postgresImage := os.Getenv("HOORIFIC_POSTGRES_IMAGE")
	if postgresImage == "" {
		postgresImage = suitePostgresImage
	}
	redisImage := os.Getenv("HOORIFIC_REDIS_IMAGE")
	if redisImage == "" {
		redisImage = suiteRedisImage
	}
	images := make(map[string]any, 2)
	imageIDs := make(map[string]string, 2)
	for _, selected := range []struct {
		service, reference string
	}{
		{"postgres", postgresImage},
		{"redis", redisImage},
	} {
		raw, inspectErr := s.command(ctx, 15*time.Second, "podman", "image", "inspect", "--format", "{{json .}}", selected.reference)
		if inspectErr != nil {
			return nil, fmt.Errorf("selected image %q must be available locally (no pull): %w", selected.reference, inspectErr)
		}
		var metadata struct {
			ID          string   `json:"Id"`
			RepoDigests []string `json:"RepoDigests"`
		}
		if err = json.Unmarshal(bytes.TrimSpace(raw), &metadata); err != nil {
			return nil, fmt.Errorf("selected local image %q returned invalid metadata: %w", selected.reference, err)
		}
		idBytes, decodeErr := hex.DecodeString(strings.TrimPrefix(metadata.ID, "sha256:"))
		if decodeErr != nil || len(idBytes) != 32 {
			return nil, fmt.Errorf("selected local image %q has no usable image ID", selected.reference)
		}
		imageIDs[selected.service] = strings.TrimPrefix(metadata.ID, "sha256:")
		images[selected.service] = map[string]any{"reference": selected.reference, "id": metadata.ID, "repo_digests": metadata.RepoDigests}
	}
	random := make([]byte, 12)
	if _, err = rand.Read(random); err != nil {
		return nil, err
	}
	name := "hoorific-bench-" + hex.EncodeToString(random)
	names := []string{name + "-postgres", name + "-redis"}
	// Register cleanup before run: a timed-out command can still have created its container.
	attempted := []string{}
	verified := []string{}
	cleanup := func() error {
		var failures error
		for _, n := range verified {
			c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			failures = errors.Join(failures, s.containerAffinity(c, dir, n, name, "after", false))
			cancel()
		}
		for i := len(attempted) - 1; i >= 0; i-- {
			n := attempted[i]
			c, cancel := context.WithTimeout(context.Background(), 55*time.Second)
			_, logErr := s.command(c, 10*time.Second, "podman", "logs", n)
			_, inspectErr := s.command(c, 10*time.Second, "podman", "inspect", n)
			_, removeErr := s.command(c, 25*time.Second, "podman", "rm", "--force", "--volumes", "--ignore", n)
			cancel()
			failures = errors.Join(failures, logErr, inspectErr, removeErr)
		}
		return failures
	}
	password := base64.RawURLEncoding.EncodeToString(random)
	envFile := filepath.Join(dir, "postgres.env")
	if err = os.WriteFile(envFile, []byte("POSTGRES_USER=bench\nPOSTGRES_DB=bench\nPOSTGRES_PASSWORD="+password+"\n"), 0600); err != nil {
		return cleanup, err
	}
	pgAddress, err := suiteAddress()
	if err != nil {
		return cleanup, err
	}
	redisAddress, err := suiteAddress()
	if err != nil {
		return cleanup, err
	}
	attempted = append(attempted, names[0])
	if _, err = s.command(ctx, 45*time.Second, "podman", "run", "--detach", "--pull=never", "--name", names[0], "--label", "hoorific.bench="+name, "--pid=private", "--publish", pgAddress+":5432", "--env-file", envFile, postgresImage, "postgres", "-c", "synchronous_commit=on", "-c", "fsync=on", "-c", "full_page_writes=on"); err != nil {
		return cleanup, err
	}
	attempted = append(attempted, names[1])
	if _, err = s.command(ctx, 45*time.Second, "podman", "run", "--detach", "--pull=never", "--name", names[1], "--label", "hoorific.bench="+name, "--pid=private", "--publish", redisAddress+":6379", redisImage, "redis-server", "--save", "", "--appendonly", "no", "--requirepass", password); err != nil {
		return cleanup, err
	}
	ready, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	for {
		_, pgErr := s.command(ready, 5*time.Second, "podman", "exec", names[0], "pg_isready", "-h", "127.0.0.1", "-p", "5432", "-U", "bench", "-d", "bench")
		pong, redisErr := s.command(ready, 5*time.Second, "podman", "exec", names[1], "redis-cli", "-a", password, "PING")
		if pgErr == nil && redisErr == nil && strings.TrimSpace(string(pong)) == "PONG" {
			break
		}
		select {
		case <-ready.Done():
			return cleanup, fmt.Errorf("isolated services readiness: %w", ready.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	containerImages := make(map[string]string, 2)
	for _, selected := range []struct {
		service, name string
	}{
		{"postgres", names[0]},
		{"redis", names[1]},
	} {
		raw, inspectErr := s.command(ctx, 10*time.Second, "podman", "inspect", "--format", "{{.Image}}", selected.name)
		if inspectErr != nil {
			return cleanup, inspectErr
		}
		actual := strings.TrimSpace(string(raw))
		if strings.TrimPrefix(actual, "sha256:") != imageIDs[selected.service] {
			return cleanup, fmt.Errorf("running %s container image %q differs from selected local image %q", selected.service, actual, imageIDs[selected.service])
		}
		containerImages[selected.service] = actual
		if err = s.containerAffinity(ctx, dir, selected.name, name, "before", true); err != nil {
			return cleanup, err
		}
		verified = append(verified, selected.name)
	}
	cfg.Storage.Postgres.DSNFile = filepath.Join(dir, "postgres.dsn")
	cfg.Coordination.Redis.URLFile = filepath.Join(dir, "redis.url")
	dsn := "postgres://bench:" + password + "@" + pgAddress + "/bench?sslmode=disable&connect_timeout=5&synchronous_commit=on"
	if err = os.WriteFile(cfg.Storage.Postgres.DSNFile, []byte(dsn), 0600); err != nil {
		return cleanup, err
	}
	if err = os.WriteFile(cfg.Coordination.Redis.URLFile, []byte("redis://:"+password+"@"+redisAddress+"/0"), 0600); err != nil {
		return cleanup, err
	}
	postgresExecutable, err := s.command(ctx, 10*time.Second, "podman", "exec", names[0], "postgres", "--version")
	if err != nil {
		return cleanup, err
	}
	postgresDatabase, err := s.command(ctx, 10*time.Second, "podman", "exec", names[0], "psql", "-U", "bench", "-d", "bench", "-X", "-v", "ON_ERROR_STOP=1", "-c", "SELECT version(); SHOW synchronous_commit; SHOW fsync; SHOW full_page_writes;")
	if err != nil {
		return cleanup, err
	}
	redisExecutable, err := s.command(ctx, 10*time.Second, "podman", "exec", names[1], "redis-server", "--version")
	if err != nil {
		return cleanup, err
	}
	redisDatabase, err := s.command(ctx, 10*time.Second, "podman", "exec", names[1], "redis-cli", "-a", password, "INFO", "server")
	if err != nil {
		return cleanup, err
	}
	if err = suiteJSON(filepath.Join(dir, "cluster-infrastructure.json"), map[string]any{
		"cluster":             name,
		"postgres_container":  names[0],
		"redis_container":     names[1],
		"images":              images,
		"container_image_ids": containerImages,
		"versions": map[string]string{
			"postgres_executable": strings.TrimSpace(string(postgresExecutable)),
			"postgres_database":   strings.TrimSpace(string(postgresDatabase)),
			"redis_executable":    strings.TrimSpace(string(redisExecutable)),
			"redis_database":      strings.TrimSpace(string(redisDatabase)),
		},
	}); err != nil {
		return cleanup, err
	}
	return cleanup, nil
}

// Container affinity is measured, not inferred from the Podman client's mask.
// No cpuset controller or image taskset is needed. A host lacking permission to
// set affinity on a rootless subuid-owned service fails a truthful prerequisite.
// The original image entrypoints (including privilege dropping) remain intact.
func (s *suiteLifecycle) containerAffinity(parent context.Context, dir, name, owner, phase string, enforce bool) (result error) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	evidence := map[string]any{"container": name, "owner": owner, "expected": s.cpus, "phase": phase, "passed": false}
	samples := []map[string]any{}
	defer func() {
		evidence["threads"] = samples
		if result != nil {
			evidence["error"] = result.Error()
		}
		result = errors.Join(result, suiteJSON(filepath.Join(dir, name+"-affinity-"+phase+".json"), evidence))
	}()
	raw, err := s.command(ctx, 10*time.Second, "podman", "inspect", "--format", "{{json .}}", name)
	if err != nil {
		return err
	}
	var container struct {
		ID     string `json:"Id"`
		Config struct {
			Labels map[string]string
		}
		HostConfig struct {
			PidMode string `json:"PidMode"`
		}
		State struct {
			Pid     int
			Running bool
		}
	}
	if err = json.Unmarshal(bytes.TrimSpace(raw), &container); err != nil {
		return err
	}
	id, err := hex.DecodeString(strings.TrimPrefix(container.ID, "sha256:"))
	if err != nil || len(id) != 32 || !container.State.Running || container.State.Pid <= 1 ||
		container.Config.Labels["hoorific.bench"] != owner || (container.HostConfig.PidMode != "" && container.HostConfig.PidMode != "private") {
		return errors.New("container affinity prerequisite: cannot establish running suite-owned private PID container")
	}
	evidence["container_id"] = container.ID
	evidence["host_pid"] = container.State.Pid
	// Start times detect exit/PID reuse across the bounded observation.
	identity := func(path string) (string, error) {
		b, e := os.ReadFile(path + "/stat")
		if e != nil {
			return "", e
		}
		end := strings.LastIndexByte(string(b), ')')
		if end < 0 {
			return "", errors.New("invalid proc stat")
		}
		fields := strings.Fields(string(b[end+1:]))
		if len(fields) < 20 {
			return "", errors.New("short proc stat")
		}
		return fields[19], nil // field 22: starttime
	}
	root := fmt.Sprintf("/proc/%d", container.State.Pid)
	start, err := identity(root)
	if err != nil {
		return fmt.Errorf("container affinity prerequisite: read host init identity: %w", err)
	}
	var expected unix.CPUSet
	for _, cpu := range strings.Split(s.cpus, ",") {
		n, e := strconv.Atoi(cpu)
		if e != nil {
			return e
		}
		expected.Set(n)
	}
	previous := map[string]string{}
	unshareAttempted := map[int]bool{}
	// Two complete inventories: optionally enforce the first, then verify only.
	// Churn/incomplete proc access fails closed, never counts as CPU evidence.
	for pass := range 2 {
		top, e := s.command(ctx, 10*time.Second, "podman", "top", container.ID, "hpid", "pid")
		if e != nil {
			return fmt.Errorf("container affinity prerequisite: host PID inventory: %w", e)
		}
		lines := strings.Split(strings.TrimSpace(string(top)), "\n")
		current := map[string]string{}
		foundInit := false
		header := strings.Fields(lines[0])
		if len(lines) < 2 || len(header) != 2 || !strings.EqualFold(header[1], "pid") ||
			(!strings.EqualFold(header[0], "hpid") && !strings.EqualFold(header[0], "hid")) {
			return errors.New("container affinity prerequisite: invalid or empty podman top hpid/pid inventory")
		}
		for _, line := range lines[1:] {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fields := strings.Fields(line)
			if len(fields) != 2 {
				return errors.New("container affinity prerequisite: invalid process identity row")
			}
			pid, e := strconv.Atoi(fields[0])
			if e != nil || pid <= 1 {
				return errors.New("container affinity prerequisite: invalid host PID")
			}
			if pid == container.State.Pid && fields[1] == "1" {
				foundInit = true
			}
			process := fmt.Sprintf("/proc/%d", pid)
			tasks, e := os.ReadDir(process + "/task")
			if e != nil || len(tasks) == 0 {
				return fmt.Errorf("container affinity prerequisite: no readable threads for host PID %d: %v", pid, e)
			}
			for _, task := range tasks {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				tid, e := strconv.Atoi(task.Name())
				if e != nil {
					return e
				}
				path := process + "/task/" + task.Name()
				stamp, e := identity(path)
				if e != nil {
					return e
				}
				status, e := os.ReadFile(path + "/status")
				if e != nil {
					return e
				}
				values := map[string]string{}
				for _, row := range strings.Split(string(status), "\n") {
					key, value, ok := strings.Cut(row, ":")
					if ok {
						values[key] = strings.TrimSpace(value)
					}
				}
				if values["Tgid"] != fields[0] || values["Pid"] != task.Name() {
					return errors.New("container affinity prerequisite: host thread ownership changed")
				}
				if values["Cpus_allowed_list"] == "" {
					return errors.New("container affinity prerequisite: missing Cpus_allowed_list")
				}
				sample := map[string]any{
					"pass":                     pass,
					"host_pid":                 pid,
					"container_pid":            fields[1],
					"host_tid":                 tid,
					"starttime":                stamp,
					"cpus_allowed_list_before": values["Cpus_allowed_list"],
					"status_before":            string(status),
				}
				samples = append(samples, sample)
				var got unix.CPUSet
				if e = unix.SchedGetaffinity(tid, &got); e != nil {
					return e
				}
				if got != expected && enforce && pass == 0 {
					if e = unix.SchedSetaffinity(tid, &expected); e != nil {
						if unshareAttempted[pid] {
							return fmt.Errorf("container affinity prerequisite: cannot set owned host thread %d to %s: %w", tid, s.cpus, e)
						}
						unshareAttempted[pid] = true
						if _, fallbackErr := s.command(ctx, 10*time.Second, "podman", "unshare", "taskset", "-apc", s.cpus, strconv.Itoa(pid)); fallbackErr != nil {
							return fmt.Errorf("container affinity prerequisite: direct affinity failed for host thread %d and podman-unshare taskset failed: %w; direct error: %v", tid, fallbackErr, e)
						}
					}
					sample["enforced"] = true
				}
				if e = unix.SchedGetaffinity(tid, &got); e != nil {
					return e
				}
				status, e = os.ReadFile(path + "/status")
				if e != nil {
					return e
				}
				values = map[string]string{}
				for _, row := range strings.Split(string(status), "\n") {
					key, value, ok := strings.Cut(row, ":")
					if ok {
						values[key] = strings.TrimSpace(value)
					}
				}
				sample["cpus_allowed_list_after"] = values["Cpus_allowed_list"]
				sample["status_after"] = string(status)
				if got != expected || values["Cpus_allowed_list"] != s.cpus {
					return fmt.Errorf("container %s host thread %d effective affinity differs from %s", name, tid, s.cpus)
				}
				after, e := identity(path)
				if e != nil || after != stamp {
					return fmt.Errorf("container affinity prerequisite: thread %d identity changed: %v", tid, e)
				}
				current[fmt.Sprintf("%d/%d", pid, tid)] = stamp
			}
		}
		if !foundInit || len(current) == 0 {
			return errors.New("container affinity prerequisite: missing live container init or threads")
		}
		if pass == 1 {
			if len(current) != len(previous) {
				return errors.New("container affinity prerequisite: thread inventory changed during verification")
			}
			for thread, stamp := range current {
				if previous[thread] != stamp {
					return errors.New("container affinity prerequisite: process/thread identity changed during verification")
				}
			}
		}
		previous = current
	}
	after, err := identity(root)
	if err != nil || after != start {
		return fmt.Errorf("container affinity prerequisite: container init identity changed: %v", err)
	}
	evidence["passed"] = true
	return nil
}

// The bridge changes transport only, never gateway responses or fixture content.
// A private self-signed CA is trusted by this gateway process only.
func suiteTLSBridge(ctx context.Context, dir, fixture string) (string, string, func() error, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", "", nil, err
	}
	cert := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "isolated benchmark fixture"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(8 * time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		return "", "", nil, err
	}
	ca := filepath.Join(dir, "fixture-ca.pem")
	if err = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		return "", "", nil, err
	}
	upstream := &url.URL{Scheme: "http", Host: fixture}
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	transport := &http.Transport{Proxy: nil, MaxIdleConns: 2048, MaxIdleConnsPerHost: 2048, IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: 15 * time.Second}
	proxy.Transport = transport
	proxy.FlushInterval = -1
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", nil, err
	}
	var activeEvents atomic.Uint64
	var resumedEvents atomic.Uint64
	var versionMu sync.Mutex
	versions := map[string]uint64{}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	secure := tls.NewListener(listener, tlsConfig)
	server := &http.Server{Handler: proxy, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }, ConnState: func(conn net.Conn, state http.ConnState) {
		if state == http.StateActive {
			if tlsConn, ok := conn.(*tls.Conn); ok {
				state := tlsConn.ConnectionState()
				if state.HandshakeComplete {
					activeEvents.Add(1)
					if state.DidResume {
						resumedEvents.Add(1)
					}
					versionMu.Lock()
					versions[tlsVersionName(state.Version)]++
					versionMu.Unlock()
				}
			}
		}
	}}
	done := make(chan error, 1)
	go func() { done <- server.Serve(secure) }()
	closeBridge := func() error {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		e := server.Shutdown(shutdown)
		if e != nil {
			_ = server.Close()
		}
		transport.CloseIdleConnections()
		select {
		case serveErr := <-done:
			if !errors.Is(serveErr, http.ErrServerClosed) {
				e = errors.Join(e, serveErr)
			}
		case <-time.After(time.Second):
			e = errors.Join(e, errors.New("TLS bridge shutdown deadline"))
		}
		versionMu.Lock()
		versionCopy := map[string]uint64{}
		for version, count := range versions {
			versionCopy[version] = count
		}
		versionMu.Unlock()
		_ = suiteJSON(filepath.Join(dir, "bridge-transport.json"), map[string]any{
			"link": "combined_direct_and_gateway_to_bridge",
			"observation": "bridge aggregate",
			"tls_versions": versionCopy,
			"tls_active_events": activeEvents.Load(),
			"tls_session_resumption_events": resumedEvents.Load(),
			"connection_reuse": "not directly observable at bridge server; child client traces label reuse per link",
		})
		return e
	}
	return "https://" + listener.Addr().String(), ca, closeBridge, nil
}
func tlsVersionName(version uint16) string {
	switch version {
	case tls.VersionTLS12:
		return "TLS1.2"
	case tls.VersionTLS13:
		return "TLS1.3"
	default:
		return fmt.Sprintf("0x%04x", version)
	}
}

type suiteAdmin struct {
	ctx                       context.Context
	client                    *http.Client
	origin, dir, cookie, csrf string
	sequence                  int
}

func (a *suiteAdmin) call(method, path string, data any) ([]byte, *http.Response, error) {
	var body []byte
	var err error
	if data != nil {
		body, err = json.Marshal(data)
		if err != nil {
			return nil, nil, err
		}
	}
	bounded, cancel := context.WithTimeout(a.ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(bounded, method, a.origin+path, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", a.origin)
	if a.cookie != "" {
		req.Header.Set("Cookie", a.cookie)
	}
	if a.csrf != "" {
		req.Header.Set("X-CSRF-Token", a.csrf)
	}
	if strings.HasSuffix(path, "/import") {
		req.Header.Set("If-Match", "1")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	b, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	resp.Body.Close()
	a.sequence++
	recordErr := suiteJSON(filepath.Join(a.dir, fmt.Sprintf("admin-%02d.json", a.sequence)), map[string]any{"method": method, "path": path, "request": json.RawMessage(bodyOrNull(body)), "status": resp.StatusCode, "response": string(b)})
	if readErr != nil || recordErr != nil {
		return b, resp, errors.Join(readErr, recordErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return b, resp, fmt.Errorf("admin %s %s returned %d (private response preserved)", method, path, resp.StatusCode)
	}
	return b, resp, nil
}
func bodyOrNull(b []byte) []byte {
	if len(b) == 0 {
		return []byte("null")
	}
	return b
}
func (a *suiteAdmin) bootstrap(code string) error {
	_, resp, err := a.call(http.MethodPost, "/admin/api/v1/auth/bootstrap", map[string]string{"code": code})
	if err != nil {
		return err
	}
	for _, c := range resp.Cookies() {
		if c.Name == "hoorific_session" {
			a.cookie = c.Name + "=" + c.Value
		}
	}
	if a.cookie == "" {
		return errors.New("bootstrap returned no session cookie")
	}
	b, _, err := a.call(http.MethodGet, "/admin/api/v1/session", nil)
	if err != nil {
		return err
	}
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	if err = json.Unmarshal(b, &session); err != nil {
		return err
	}
	a.csrf = session.CSRF
	if a.csrf == "" {
		return errors.New("session returned no CSRF token")
	}
	return nil
}
func (a *suiteAdmin) provision(alias, route, upstream string) (string, error) {
	provider := "openai"
	if route == "translated" {
		provider = "anthropic"
	}
	// Both are portable aliases: native is OpenAI-chat -> OpenAI-chat (no codec
	// translation), translated is OpenAI-chat -> Anthropic-messages. Neither is
	// an account-scoped native-resource API benchmark.
	resources := []struct {
		kind, id string
		data     any
	}{
		{"connections", "bench-connection", map[string]any{"connector": provider, "account_id": "bench-account", "base_url": strings.TrimRight(upstream, "/") + "/v1", "dedicated": true, "enabled": true, "settings": map[string]string{"allow_private": "true", "allowed_cidrs": "127.0.0.1/32"}}},
		{"models", "bench-model", map[string]any{"connection_id": "bench-connection", "upstream_id": "bench-fixture", "operations": []string{"generate"}, "input_modalities": []string{"text"}, "output_modalities": []string{"text"}, "features": map[string]string{"streaming": "supported"}, "context_limit": 1048576, "output_limit": 1024, "price": map[string]any{"version": "bench-v1", "input_per_million": 1000000000, "output_per_million": 2000000000, "maximum_unit_cost": 1000000000, "unit_operation": "generate"}, "enabled": true, "provenance": "deterministic child fixture"}},
		{"model_aliases", alias, map[string]any{"model_ids": []string{"bench-model"}, "enabled": true}},
		{"route_policies", alias, map[string]any{"alias": alias, "targets": []any{map[string]any{"connection_id": "bench-connection", "model_id": "bench-model", "priority": 0, "weight": 1}}, "fallback": false, "affinity": false}},
	}
	for _, r := range resources {
		if _, _, err := a.call(http.MethodPost, "/admin/api/v1/"+r.kind, map[string]any{"id": r.id, "data": r.data}); err != nil {
			return "", err
		}
	}
	credentialData := map[string]any{"data": map[string]any{"provider": provider, "account_id": "bench-account", "kind": "api_key", "secret": "isolated-fixture-only"}}
	if _, _, err := a.call(http.MethodPost, "/admin/api/v1/connections/bench-connection/import", credentialData); err != nil {
		return "", err
	}
	b, _, err := a.call(http.MethodPost, "/admin/api/v1/api_keys/bench-key/issue", map[string]any{"data": map[string]any{"name": "benchmark uncapped key", "role": "operator", "permissions": []string{"inference:invoke", "connection:bench-connection"}, "aliases": []string{alias}, "connections": []string{"bench-connection"}, "operations": []string{"generate"}, "portable": true, "native_account": false, "realtime": false}})
	if err != nil {
		return "", err
	}
	var issued struct {
		Token string `json:"token"`
	}
	if err = json.Unmarshal(b, &issued); err != nil {
		return "", err
	}
	if issued.Token == "" {
		return "", errors.New("key issue returned no token")
	}
	return issued.Token, nil
}

// This observer uses the same store constructor and transaction implementation
// as the gateway. Its connection settings are explicitly labelled, never passed
// off as observations of another process's connection-local SQLite PRAGMAs.
func suiteSQL(ctx context.Context, dir, phase string, cfg core.BootstrapConfig) (map[string]int64, error) {
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	db, err := store.Open(bounded, cfg)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	evidence := map[string]any{"phase": phase, "dialect": db.Dialect, "connection_scope": "observer via gateway store.Open and WithTx", "queries": map[string]string{"attempts": "SELECT COUNT(*) FROM attempts", "usage_ledger": "SELECT COUNT(*) FROM usage_ledger", "policy_limits": "SELECT COUNT(*) FROM policy_limits"}}
	counts := map[string]int64{}
	err = db.WithTx(bounded, func(tx *sql.Tx) error {
		if db.Dialect == "sqlite" {
			var synchronous int
			var journal, version string
			if e := tx.QueryRowContext(bounded, "PRAGMA synchronous").Scan(&synchronous); e != nil {
				return e
			}
			if e := tx.QueryRowContext(bounded, "PRAGMA journal_mode").Scan(&journal); e != nil {
				return e
			}
			if e := tx.QueryRowContext(bounded, "SELECT sqlite_version()").Scan(&version); e != nil {
				return e
			}
			evidence["synchronous"] = synchronous
			evidence["journal_mode"] = journal
			evidence["version"] = version
			if synchronous != 2 || strings.ToLower(journal) != "wal" {
				return errors.New("SQLite observer is not WAL/FULL")
			}
		} else {
			for _, setting := range []string{"synchronous_commit", "fsync", "full_page_writes"} {
				var value string
				if e := tx.QueryRowContext(bounded, "SHOW "+setting).Scan(&value); e != nil {
					return e
				}
				evidence[setting] = value
				if value != "on" {
					return fmt.Errorf("PostgreSQL %s is not on", setting)
				}
			}
			var version string
			if e := tx.QueryRowContext(bounded, "SELECT version()").Scan(&version); e != nil {
				return e
			}
			evidence["version"] = version
		}
		for _, table := range []string{"attempts", "usage_ledger", "policy_limits", "admissions", "api_keys", "resources"} {
			var n int64
			if e := tx.QueryRowContext(bounded, "SELECT COUNT(*) FROM "+table).Scan(&n); e != nil {
				return e
			}
			counts[table] = n
		}
		if counts["policy_limits"] != 0 {
			return errors.New("benchmark database has policy caps")
		}
		rows, e := tx.QueryContext(bounded, "SELECT state, COUNT(*) FROM attempts GROUP BY state")
		if e != nil {
			return e
		}
		defer rows.Close()
		states := map[string]int64{}
		for rows.Next() {
			var state string
			var n int64
			if e = rows.Scan(&state, &n); e != nil {
				return e
			}
			states[state] = n
		}
		if e = rows.Err(); e != nil {
			return e
		}
		evidence["attempt_states"] = states
		return nil
	})
	evidence["counts"] = counts
	if err != nil {
		evidence["error"] = err.Error()
	}
	return counts, errors.Join(err, suiteJSON(filepath.Join(dir, "sql-"+phase+".json"), evidence))
}
