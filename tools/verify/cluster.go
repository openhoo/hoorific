package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"hoorific/internal/core"
)

// Infrastructure is indexed by its private evidence directory, never discovered
// from a user's configuration. Fault injection cannot target external services.
type verifyCluster struct {
	root, runtime, owner, postgres, redis string
	mu                                    sync.Mutex
	sequence                              uint64
	closeOnce                             sync.Once
}

var verifyClusters sync.Map
var clusterProcessSequence atomic.Uint64

type clusterProcess struct {
	done chan struct{}
	err  error
}

var clusterProcesses sync.Map

func clusterFor(root string) (*verifyCluster, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	value, ok := verifyClusters.Load(absolute)
	if !ok {
		return nil, errors.New("cluster faults require infrastructure owned by this verification invocation")
	}
	return value.(*verifyCluster), nil
}

// command has a hard wall-clock bound and retains output even on timeout. The
// arguments never contain passwords: PostgreSQL reads a private environment file.
func (c *verifyCluster) command(timeout time.Duration, args ...string) ([]byte, error) {
	if timeout <= 0 {
		return nil, errors.New("command deadline must be positive")
	}
	c.mu.Lock()
	c.sequence++
	number := c.sequence
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.runtime, args...)
	cmd.WaitDelay = 2 * time.Second
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	started := time.Now()
	err := cmd.Run()
	record := fmt.Sprintf("command: %s %s\nstarted: %s\nduration: %s\nerror: %v\n", c.runtime, strings.Join(args, " "), started.UTC().Format(time.RFC3339Nano), time.Since(started), err)
	path := filepath.Join(c.root, fmt.Sprintf("container-command-%04d.log", number))
	writeErr := os.WriteFile(path, append([]byte(record), output.Bytes()...), 0600)
	if ctx.Err() != nil {
		err = fmt.Errorf("command deadline: %w", ctx.Err())
	}
	if err != nil {
		return output.Bytes(), fmt.Errorf("%s (evidence %s): %w", args[0], path, err)
	}
	if writeErr != nil {
		return output.Bytes(), writeErr
	}
	return output.Bytes(), nil
}

func (c *verifyCluster) owned(id string) error {
	if id == "" {
		return errors.New("empty container identity")
	}
	raw, err := c.command(10*time.Second, "inspect", "--format", `{{ index .Config.Labels "io.hoorific.verify.owner" }}`, id)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(raw)) != c.owner {
		return errors.New("refusing operation on container without this invocation's ownership label")
	}
	return nil
}
func (c *verifyCluster) fault(service, action string) error {
	id := ""
	switch service {
	case "postgres":
		id = c.postgres
	case "redis":
		id = c.redis
	default:
		return fmt.Errorf("unknown owned service %q", service)
	}
	if err := c.owned(id); err != nil {
		return err
	}
	switch action {
	case "stop":
		_, err := c.command(20*time.Second, "stop", "--time", "5", id)
		return err
	case "start":
		_, err := c.command(20*time.Second, "start", id)
		return err
	case "restart":
		_, err := c.command(30*time.Second, "restart", "--time", "5", id)
		return err
	default:
		return fmt.Errorf("unsupported fault action %q", action)
	}
}
func (c *verifyCluster) sql(query string) (string, error) {
	if err := c.owned(c.postgres); err != nil {
		return "", err
	}
	raw, err := c.command(15*time.Second, "exec", "-e", "PGOPTIONS=-c statement_timeout=10000", c.postgres, "psql", "-X", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1", "-U", "hoorific", "-d", "hoorific", "-c", query)
	return strings.TrimSpace(string(raw)), err
}
func (c *verifyCluster) ready() error {
	deadline := time.Now().Add(60 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		if _, last = c.command(5*time.Second, "exec", c.postgres, "pg_isready", "-U", "hoorific", "-d", "hoorific"); last == nil {
			var raw []byte
			raw, last = c.command(5*time.Second, "exec", c.redis, "redis-cli", "PING")
			if last == nil && strings.TrimSpace(string(raw)) == "PONG" {
				value, err := c.sql("SELECT 1")
				if err == nil && value == "1" {
					return nil
				}
				last = err
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("owned PostgreSQL/Redis readiness deadline: %v", last)
}
func (c *verifyCluster) shutdown() {
	c.closeOnce.Do(func() {
		var failures []string
		for _, id := range []string{c.redis, c.postgres} {
			if id == "" {
				continue
			}
			if err := c.owned(id); err != nil {
				failures = append(failures, err.Error())
				continue
			}
			// Logs are taken before and after stop, including a container whose startup failed.
			_, _ = c.command(10*time.Second, "logs", "--timestamps", id)
			_, _ = c.command(20*time.Second, "stop", "--time", "5", id)
			_, _ = c.command(10*time.Second, "logs", "--timestamps", id)
			if _, err := c.command(15*time.Second, "rm", "--force", "--volumes", id); err != nil {
				failures = append(failures, err.Error())
			}
		}
		_ = os.WriteFile(filepath.Join(c.root, "cluster-cleanup.log"), []byte(strings.Join(failures, "\n")), 0600)
		verifyClusters.Delete(c.root)
	})
}

func prepareCluster(root string) (dsnFile, redisFile string, close func(), err error) {
	close = func() {}
	root, err = filepath.Abs(root)
	if err != nil {
		return
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return
	}
	nonce := make([]byte, 16)
	if _, err = rand.Read(nonce); err != nil {
		return
	}
	owner := hex.EncodeToString(nonce)
	c := &verifyCluster{root: root, owner: owner}
	close = c.shutdown
	complete := false
	defer func() {
		if !complete {
			close()
		}
	}()
	// A broken Podman installation does not mask a usable Docker daemon.
	for _, candidate := range []string{"podman", "docker"} {
		path, lookupErr := exec.LookPath(candidate)
		if lookupErr != nil {
			continue
		}
		c.runtime = path
		if _, probeErr := c.command(15*time.Second, "info"); probeErr == nil {
			break
		}
		c.runtime = ""
	}
	if c.runtime == "" {
		err = errors.New("isolated cluster requires a working Podman (preferred) or Docker runtime; see container command logs")
		return
	}
	postgresImage := os.Getenv("HOORIFIC_POSTGRES_IMAGE")
	if postgresImage == "" {
		postgresImage = "docker.io/library/postgres:17"
	}
	redisImage := os.Getenv("HOORIFIC_REDIS_IMAGE")
	if redisImage == "" {
		redisImage = "docker.io/library/redis:7"
	}
	images := make(map[string]any, 2)
	imageIDs := make(map[string]string, 2)
	for _, selected := range []struct {
		service, reference string
	}{
		{"postgres", postgresImage},
		{"redis", redisImage},
	} {
		raw, inspectErr := c.command(15*time.Second, "image", "inspect", "--format", "{{json .}}", selected.reference)
		if inspectErr != nil {
			err = fmt.Errorf("selected image %q must be available locally (no pull): %w", selected.reference, inspectErr)
			return
		}
		var metadata struct {
			ID          string   `json:"Id"`
			RepoDigests []string `json:"RepoDigests"`
		}
		if err = json.Unmarshal(bytes.TrimSpace(raw), &metadata); err != nil {
			return "", "", close, fmt.Errorf("selected local image %q returned invalid metadata: %w", selected.reference, err)
		}
		idBytes, decodeErr := hex.DecodeString(strings.TrimPrefix(metadata.ID, "sha256:"))
		if decodeErr != nil || len(idBytes) != 32 {
			err = fmt.Errorf("selected local image %q has no usable image ID", selected.reference)
			return
		}
		imageIDs[selected.service] = strings.TrimPrefix(metadata.ID, "sha256:")
		images[selected.service] = map[string]any{"reference": selected.reference, "id": metadata.ID, "repo_digests": metadata.RepoDigests}
	}
	passwordBytes := make([]byte, 32)
	if _, err = rand.Read(passwordBytes); err != nil {
		return
	}
	password := hex.EncodeToString(passwordBytes)
	envFile := filepath.Join(root, "postgres.env")
	if err = os.WriteFile(envFile, []byte("POSTGRES_USER=hoorific\nPOSTGRES_DB=hoorific\nPOSTGRES_PASSWORD="+password+"\n"), 0600); err != nil {
		return
	}
	// Save names before create so timeout/partial-create cleanup still checks the
	// exact generated name AND ownership label. Never use prune or name prefixes.
	c.postgres = "hoorific-verify-pg-" + owner
	c.redis = "hoorific-verify-redis-" + owner
	definitions := []struct {
		name, image, port string
		args              []string
	}{
		{c.postgres, postgresImage, "5432", []string{"--env-file", envFile}},
		{c.redis, redisImage, "6379", nil},
	}
	for _, definition := range definitions {
		args := []string{"create", "--pull=never", "--name", definition.name, "--label", "io.hoorific.verify.owner=" + owner, "--publish", "127.0.0.1::" + definition.port}
		args = append(args, definition.args...)
		args = append(args, definition.image)
		if definition.name == c.redis {
			args = append(args, "redis-server", "--save", "", "--appendonly", "no")
		}
		if _, err = c.command(180*time.Second, args...); err != nil {
			return
		}
		if _, err = c.command(30*time.Second, "start", definition.name); err != nil {
			return
		}
	}
	var pgAddress, redisAddress string
	pgAddress, err = c.port(c.postgres, "5432/tcp")
	if err != nil {
		return
	}
	redisAddress, err = c.port(c.redis, "6379/tcp")
	if err != nil {
		return
	}
	if err = c.ready(); err != nil {
		return
	}
	containerImages := make(map[string]string, 2)
	for _, selected := range []struct {
		service, id string
	}{
		{"postgres", c.postgres},
		{"redis", c.redis},
	} {
		raw, inspectErr := c.command(10*time.Second, "inspect", "--format", "{{.Image}}", selected.id)
		if inspectErr != nil {
			err = inspectErr
			return
		}
		actual := strings.TrimSpace(string(raw))
		if strings.TrimPrefix(actual, "sha256:") != imageIDs[selected.service] {
			err = fmt.Errorf("running %s container image %q differs from selected local image %q", selected.service, actual, imageIDs[selected.service])
			return
		}
		containerImages[selected.service] = actual
	}
	postgresExecutable, err := c.command(10*time.Second, "exec", c.postgres, "postgres", "--version")
	if err != nil {
		return
	}
	postgresDatabase, err := c.sql("SELECT version()")
	if err != nil {
		return
	}
	redisExecutable, err := c.command(10*time.Second, "exec", c.redis, "redis-server", "--version")
	if err != nil {
		return
	}
	redisDatabase, err := c.command(10*time.Second, "exec", c.redis, "redis-cli", "INFO", "server")
	if err != nil {
		return
	}
	dsnFile = filepath.Join(root, "postgres.dsn")
	redisFile = filepath.Join(root, "redis.url")
	if err = os.WriteFile(dsnFile, []byte("postgres://hoorific:"+password+"@"+pgAddress+"/hoorific?sslmode=disable&connect_timeout=3\n"), 0600); err != nil {
		return
	}
	if err = os.WriteFile(redisFile, []byte("redis://"+redisAddress+"/0\n"), 0600); err != nil {
		return
	}
	evidence, _ := json.MarshalIndent(map[string]any{
		"runtime":             c.runtime,
		"owner":               owner,
		"postgres":            c.postgres,
		"redis":               c.redis,
		"postgres_address":    pgAddress,
		"redis_address":       redisAddress,
		"images":              images,
		"container_image_ids": containerImages,
		"versions": map[string]string{
			"postgres_executable": strings.TrimSpace(string(postgresExecutable)),
			"postgres_database":   strings.TrimSpace(postgresDatabase),
			"redis_executable":    strings.TrimSpace(string(redisExecutable)),
			"redis_database":      strings.TrimSpace(string(redisDatabase)),
		},
	}, "", "  ")
	if err = os.WriteFile(filepath.Join(root, "cluster-infrastructure.json"), evidence, 0600); err != nil {
		return
	}
	verifyClusters.Store(root, c)
	complete = true
	return
}
func (c *verifyCluster) port(id, port string) (string, error) {
	raw, err := c.command(10*time.Second, "port", id, port)
	if err != nil {
		return "", err
	}
	lines := strings.Fields(string(raw))
	if len(lines) != 1 {
		return "", fmt.Errorf("expected one loopback binding, got %q", raw)
	}
	host, p, err := net.SplitHostPort(lines[0])
	if err != nil || host != "127.0.0.1" || p == "" {
		return "", fmt.Errorf("refusing non-loopback port binding %q", raw)
	}
	return lines[0], nil
}

func (e *environment) clusterReady() error {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(30 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		if state, ok := clusterProcesses.Load(e.server); ok {
			select {
			case <-state.(*clusterProcess).done:
				return fmt.Errorf("gateway exited before readiness: %v", state.(*clusterProcess).err)
			default:
			}
		}
		r, err := client.Get("http://" + e.management + "/health/ready")
		if err == nil {
			b, readErr := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			r.Body.Close()
			last = fmt.Sprintf("status=%d body=%s read=%v", r.StatusCode, trim(string(b)), readErr)
			if r.StatusCode == http.StatusOK && readErr == nil {
				return nil
			}
		} else {
			last = err.Error()
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("gateway readiness deadline: %s", last)
}
func (e *environment) clusterLaunch() error {
	if e.binary == "" {
		return errors.New("cluster process requires an already resolved built binary")
	}
	serial := clusterProcessSequence.Add(1)
	logPath := filepath.Join(e.root, fmt.Sprintf("cluster-gateway-%04d.log", serial))
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	cmd := exec.Command(e.binary, "serve", "--config", e.config)
	cmd.Dir = e.root
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err = cmd.Start(); err != nil {
		logFile.Close()
		return err
	}
	e.server = cmd
	state := &clusterProcess{done: make(chan struct{})}
	clusterProcesses.Store(cmd, state)
	go func() { state.err = cmd.Wait(); logFile.Close(); close(state.done) }()
	proof, _ := json.MarshalIndent(map[string]any{"pid": cmd.Process.Pid, "parent_pid": os.Getpid(), "binary": e.binary, "config": e.config, "inference": e.inference, "management": e.management, "log": logPath, "started_at": time.Now().UTC()}, "", "  ")
	if err = os.WriteFile(filepath.Join(e.root, fmt.Sprintf("cluster-gateway-%04d.json", serial)), proof, 0600); err != nil {
		_ = e.clusterStop()
		return err
	}
	if err = e.clusterReady(); err != nil {
		_ = e.clusterStop()
		return fmt.Errorf("%w; see %s", err, logPath)
	}
	return nil
}
func (e *environment) clusterStop() error {
	cmd := e.server
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	var state *clusterProcess
	if value, ok := clusterProcesses.Load(cmd); ok {
		state = value.(*clusterProcess)
	} else {
		state = &clusterProcess{done: make(chan struct{})}
		go func() { state.err = cmd.Wait(); close(state.done) }()
	}
	_ = cmd.Process.Signal(os.Interrupt)
	select {
	case <-state.done:
	case <-time.After(35 * time.Second):
		_ = cmd.Process.Kill()
		select {
		case <-state.done:
		case <-time.After(5 * time.Second):
			return errors.New("gateway could not be reaped after kill")
		}
	}
	clusterProcesses.Delete(cmd)
	e.server = nil
	return nil
}
func (e *environment) clusterRestart() error {
	if err := e.clusterStop(); err != nil {
		return err
	}
	return e.clusterLaunch()
}
func (e *environment) clusterReplica() (*environment, func(), error) {
	if e.mode != "cluster" {
		return nil, func() {}, errors.New("second active gateway requires PostgreSQL cluster mode")
	}
	if _, err := clusterFor(e.root); err != nil {
		return nil, func() {}, err
	}
	raw, err := os.ReadFile(e.config)
	if err != nil {
		return nil, func() {}, err
	}
	config, err := core.DecodeBootstrap(bytes.NewReader(raw))
	if err != nil {
		return nil, func() {}, err
	}
	inf, err := freeAddr()
	if err != nil {
		return nil, func() {}, err
	}
	mgmt, err := freeAddr()
	if err != nil {
		return nil, func() {}, err
	}
	for mgmt == inf || mgmt == e.inference || mgmt == e.management {
		mgmt, err = freeAddr()
		if err != nil {
			return nil, func() {}, err
		}
	}
	if inf == e.inference || inf == e.management {
		return nil, func() {}, errors.New("replica listener collision")
	}
	dir, err := os.MkdirTemp(e.root, "replica-")
	if err != nil {
		return nil, func() {}, err
	}
	config.DataDir = dir
	config.Listeners.Inference = inf
	config.Listeners.Management = mgmt
	config.PublicURLs = map[string]string{"inference": "http://" + inf}
	path := filepath.Join(dir, "config.json")
	raw, err = json.MarshalIndent(config, "", "  ")
	if err != nil {
		return nil, func() {}, err
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		return nil, func() {}, err
	}
	replica := &environment{root: dir, config: path, key: e.key, mode: e.mode, tenantID: e.tenantID, binary: e.binary, inference: inf, management: mgmt, client: &http.Client{Timeout: 15 * time.Second}, fixture: e.fixture, cookie: e.cookie, csrf: e.csrf}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			if err := replica.clusterStop(); err != nil {
				_ = os.WriteFile(filepath.Join(dir, "shutdown-error.log"), []byte(err.Error()), 0600)
			}
		})
	}
	if err = replica.clusterLaunch(); err != nil {
		stop()
		return nil, stop, err
	}
	if e.server == nil || e.server.Process == nil || e.server.Process.Pid == replica.server.Process.Pid {
		stop()
		return nil, stop, errors.New("replica process identity is not independent")
	}
	return replica, stop, nil
}
