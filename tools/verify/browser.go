package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// browserScenarios runs Chromium against the owned real server, never an operator session.
func (e *environment) browserScenarios(output string) []result {
	start := time.Now()
	state, err := e.browserStorageState()
	if err != nil {
		return []result{extResult("browser/setup", start, nil, err)}
	}
	python := os.Getenv("HOORIFIC_VERIFY_PYTHON")
	if python == "" {
		python = "python3"
	}
	evidenceDir := os.Getenv("HOORIFIC_VERIFY_EVIDENCE_DIR")
	if evidenceDir == "" {
		evidenceDir = filepath.Join(".artifacts", "browser-e2e")
		if output != "" {
			evidenceDir = output + ".browser"
		}
	}
	if err := os.MkdirAll(evidenceDir, 0700); err != nil {
		return []result{extResult("browser/setup", start, nil, err)}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, filepath.Join("tools", "verify", "sdk", "browser_runner.py"))
	cmd.Env = append(os.Environ(), "HOORIFIC_VERIFY_CONSOLE=http://"+e.management,
		"HOORIFIC_VERIFY_BROWSER_STORAGE_STATE="+state, "HOORIFIC_VERIFY_EVIDENCE_DIR="+evidenceDir)
	var stdout, stderr extBoundedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	evidence := map[string]any{"directory": evidenceDir, "stderr": e.extRedact(stderr.String())}
	if stdout.overflow || stderr.overflow {
		return []result{extResult("browser/runner", start, evidence, fmt.Errorf("browser output exceeded bounded evidence capture"))}
	}
	var results []result
	if parseErr := json.Unmarshal(stdout.Bytes(), &results); parseErr != nil || len(results) == 0 {
		return []result{extResult("browser/runner", start, evidence, fmt.Errorf("browser runner must emit a nonempty JSON result array (exit: %v; parse: %v)", err, parseErr))}
	}
	for i := range results {
		if results[i].Status != "passed" && results[i].Status != "failed" {
			results[i].Status = "failed"
			results[i].Detail = "browser runner emitted invalid scenario status"
		}
		raw, marshalErr := json.Marshal(results[i])
		if marshalErr == nil {
			_ = json.Unmarshal([]byte(e.extRedact(string(raw))), &results[i])
		}
	}
	if err != nil {
		results = append(results, extResult("browser/process-exit", start, evidence, err))
	}
	return results
}
