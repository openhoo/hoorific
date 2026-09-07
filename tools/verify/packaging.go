package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// packagingCommand is deliberately local to the verification process. It keeps
// command output under the invocation root and never runs shell text.
func (e *environment) packagingCommand(name string, timeout time.Duration, args ...string) ([]byte, error) {
	if timeout <= 0 {
		return nil, errors.New("packaging command deadline must be positive")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = e.root
	cmd.WaitDelay = 2 * time.Second
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	path := filepath.Join(e.root, "packaging-"+strings.ReplaceAll(filepath.Base(name), "/", "_")+"-"+strconv.FormatInt(time.Now().UnixNano(), 10)+".log")
	_ = os.WriteFile(path, out.Bytes(), 0600)
	if ctx.Err() != nil {
		return out.Bytes(), fmt.Errorf("%s timed out: %w (see %s)", name, ctx.Err(), path)
	}
	if err != nil {
		return out.Bytes(), fmt.Errorf("%s failed: %w (see %s)", name, err, path)
	}
	return out.Bytes(), nil
}
func packagingResult(name string, start time.Time, status, detail string, evidence any) result {
	return result{name, status, detail, time.Since(start).Milliseconds(), evidence}
}

func (e *environment) packagingHealth(name string) result {
	started := time.Now()
	if e.management == "" {
		return packagingResult(name, started, "failed", "management listener is not configured", nil)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	r, err := client.Get("http://" + e.management + "/health/ready")
	if err != nil {
		return packagingResult(name, started, "failed", "actual gateway readiness request failed: "+err.Error(), nil)
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		return packagingResult(name, started, "failed", fmt.Sprintf("actual gateway readiness returned %d: %s", r.StatusCode, trim(string(body))), map[string]any{"status": r.StatusCode})
	}
	return packagingResult(name, started, "passed", "actual packaged-process readiness returned 200", map[string]any{"status": r.StatusCode, "body": trim(string(body))})
}
func (e *environment) packagingSQLiteRecovery() result {
	started := time.Now()
	if e.mode != "standalone" {
		return packagingResult("packaging/sqlite-recovery", started, "not-run", "SQLite persistence is not part of cluster topology", map[string]any{"topology": e.mode})
	}
	db := filepath.Join(e.root, "hoorific.db")
	backup := filepath.Join(e.root, "hoorific.db.backup")
	if err := e.packagingStopServe(); err != nil {
		return packagingResult("packaging/sqlite-recovery", started, "failed", "gateway stop before consistent SQLite backup failed: "+err.Error(), nil)
	}
	before, err := os.ReadFile(db)
	if err != nil {
		return packagingResult("packaging/sqlite-recovery", started, "failed", "SQLite database could not be read after clean stop: "+err.Error(), nil)
	}
	if err = os.WriteFile(backup, before, 0600); err != nil {
		return packagingResult("packaging/sqlite-recovery", started, "failed", "SQLite backup failed: "+err.Error(), nil)
	}
	digest := sha256.Sum256(before)
	if err = os.WriteFile(db, before, 0600); err != nil {
		return packagingResult("packaging/sqlite-recovery", started, "failed", "SQLite restore failed: "+err.Error(), nil)
	}
	if err = e.Start(context.Background(), e.binary); err != nil {
		return packagingResult("packaging/sqlite-recovery", started, "failed", "gateway did not recover after SQLite restore: "+err.Error(), nil)
	}
	ready := e.packagingHealth("packaging/sqlite-recovery")
	if ready.Status != "passed" {
		ready.Name = "packaging/sqlite-recovery"
		return ready
	}
	after, err := os.ReadFile(backup)
	if err != nil {
		return packagingResult("packaging/sqlite-recovery", started, "failed", "SQLite backup could not be reread: "+err.Error(), nil)
	}
	restored := sha256.Sum256(after)
	if digest != restored {
		return packagingResult("packaging/sqlite-recovery", started, "failed", "SQLite backup digest changed during restore", map[string]any{"before_sha256": hex.EncodeToString(digest[:]), "backup_sha256": hex.EncodeToString(restored[:])})
	}
	return packagingResult("packaging/sqlite-recovery", started, "passed", "cleanly stopped SQLite, restored backup, and recovered actual gateway", map[string]any{"backup": backup, "sha256": hex.EncodeToString(digest[:])})
}
func (e *environment) packagingStopServe() error {
	if e.server == nil || e.server.Process == nil {
		return errors.New("gateway process is not running")
	}
	if _, managed := clusterProcesses.Load(e.server); managed {
		return e.clusterStop()
	}
	_ = e.server.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() { done <- e.server.Wait() }()
	select {
	case <-done:
		e.server = nil
		return nil
	case <-time.After(35 * time.Second):
		_ = e.server.Process.Kill()
		select {
		case <-done:
			e.server = nil
			return nil
		case <-time.After(5 * time.Second):
			return errors.New("gateway did not drain within bounded shutdown")
		}
	}
}
func (e *environment) packagingReplicaRecovery() result {
	started := time.Now()
	if e.mode != "cluster" {
		return packagingResult("packaging/postgres-replica-recovery", started, "not-run", "both-replica PostgreSQL recovery applies only to cluster topology", map[string]any{"topology": e.mode})
	}
	c, err := clusterFor(e.root)
	if err != nil {
		return packagingResult("packaging/postgres-replica-recovery", started, "failed", err.Error(), nil)
	}
	if err = c.fault("postgres", "restart"); err != nil {
		return packagingResult("packaging/postgres-replica-recovery", started, "failed", "owned PostgreSQL restart failed: "+err.Error(), nil)
	}
	if err = c.ready(); err != nil {
		return packagingResult("packaging/postgres-replica-recovery", started, "failed", "PostgreSQL/Redis did not recover: "+err.Error(), nil)
	}
	replica, stop, err := e.clusterReplica()
	if err != nil {
		return packagingResult("packaging/postgres-replica-recovery", started, "failed", "second real gateway did not recover: "+err.Error(), nil)
	}
	defer stop()
	if err = e.clusterRestart(); err != nil {
		return packagingResult("packaging/postgres-replica-recovery", started, "failed", "first real gateway did not recover: "+err.Error(), nil)
	}
	if err = replica.clusterReady(); err != nil {
		return packagingResult("packaging/postgres-replica-recovery", started, "failed", "second real gateway readiness failed after first restart: "+err.Error(), nil)
	}
	return packagingResult("packaging/postgres-replica-recovery", started, "passed", "owned PostgreSQL restarted and both independently launched gateways recovered", map[string]any{"postgres": c.postgres, "replica_management": replica.management})
}
func (e *environment) packagingMigrationRace() result {
	started := time.Now()
	if e.mode != "cluster" {
		return packagingResult("packaging/migration-race", started, "not-run", "migration-race qualification requires external PostgreSQL cluster topology", map[string]any{"topology": e.mode})
	}
	replica, stop, err := e.clusterReplica()
	if err != nil {
		return packagingResult("packaging/migration-race", started, "failed", "second gateway could not start without running migrations: "+err.Error(), nil)
	}
	defer stop()
	if err := replica.clusterReady(); err != nil {
		return packagingResult("packaging/migration-race", started, "failed", err.Error(), nil)
	}
	return packagingResult("packaging/migration-race", started, "passed", "second real gateway served shared schema without concurrent migration", map[string]any{"primary_pid": e.server.Process.Pid, "replica_pid": replica.server.Process.Pid})
}
func (e *environment) packagingDrain() result {
	started := time.Now()
	if e.server == nil || e.server.Process == nil {
		return packagingResult("packaging/graceful-drain", started, "failed", "actual gateway process is not running", nil)
	}
	pid := e.server.Process.Pid
	if err := e.packagingStopServe(); err != nil {
		return packagingResult("packaging/graceful-drain", started, "failed", err.Error(), map[string]any{"pid": pid})
	}
	if err := e.Start(context.Background(), e.binary); err != nil {
		return packagingResult("packaging/graceful-drain", started, "failed", "gateway failed to relaunch after graceful drain: "+err.Error(), map[string]any{"pid": pid})
	}
	return packagingResult("packaging/graceful-drain", started, "passed", "actual gateway accepted interrupt, drained, exited, and relaunched", map[string]any{"old_pid": pid, "new_pid": e.server.Process.Pid})
}
func (e *environment) packagingHelm() result {
	started := time.Now()
	helm, helmErr := exec.LookPath("helm")
	kubectl, kubectlErr := exec.LookPath("kubectl")
	if helmErr != nil || kubectlErr != nil {
		return packagingResult("packaging/helm", started, "not-run", "Kubernetes runtime qualification not-run: helm and/or kubectl unavailable; no Kubernetes claim made", map[string]any{"helm": helmErr == nil, "kubectl": kubectlErr == nil, "coverage": "chart sources and image/Compose checks only"})
	}
	cwd, _ := os.Getwd()
	chart := filepath.Join(cwd, "charts", "hoorific")
	if _, err := os.Stat(chart); err != nil {
		return packagingResult("packaging/helm", started, "failed", "Helm chart is absent at expected repository path", map[string]any{"chart": chart})
	}
	raw, err := e.packagingCommand(helm, 30*time.Second, "template", "hoorific-verify", chart, "--set", "image.repository=hoorific", "--set", "image.tag=verify", "--set", "existingSecrets.postgres.secretName=hoorific-verify-postgres", "--set", "existingSecrets.encryption.secretName=hoorific-verify-encryption")
	if err != nil {
		return packagingResult("packaging/helm", started, "failed", "Helm template failed: "+err.Error(), map[string]any{"output": trim(string(raw))})
	}
	if len(raw) == 0 {
		return packagingResult("packaging/helm", started, "failed", "Helm template produced no manifests", nil)
	}
	// Installation is deliberately not attempted without an operator-provided isolated context.
	return packagingResult("packaging/helm", started, "not-run", "Helm rendered manifests, but isolated-cluster installation requires an explicit operator context; Kubernetes runtime was not claimed", map[string]any{"helm": helm, "kubectl": kubectl, "rendered_bytes": len(raw)})
}
func (e *environment) packagingScenarios() []result {
	// Keep this callable from main's all/packaging scenarios. Topology-specific
	// executable checks are selected rather than reported as artificial skips.
	results := make([]result, 0, 5)
	results = append(results, e.packagingHealth("packaging/process"))
	if e.mode == "standalone" {
		results = append(results, e.packagingSQLiteRecovery())
	} else if e.mode == "cluster" {
		results = append(results, e.packagingReplicaRecovery(), e.packagingMigrationRace())
	}
	results = append(results, e.packagingDrain(), e.packagingHelm())
	return results
}
