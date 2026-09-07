package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Checkpoints survive termination of the runner. Stderr contains only static
// scenario names/statuses; detailed results stay in the private report file.
var verificationLog struct {
	sync.Mutex
	path    string
	report  report
	indices map[string]int
}

func initializeVerificationProgress(path string, base report) error {
	if path == "" {
		dir, err := os.MkdirTemp("", "hoorific-verify-progress-")
		if err != nil {
			return err
		}
		path = filepath.Join(dir, "report.json")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	verificationLog.path = path
	verificationLog.report = base
	verificationLog.indices = make(map[string]int)
	fmt.Fprintf(os.Stderr, "verification checkpoint=%s\n", path)
	return writeVerificationCheckpoint()
}

func verificationProgress(name, status string, durationMS int64) {
	fmt.Fprintf(os.Stderr, "verification %s %s duration_ms=%d at=%s\n", name, status, durationMS, time.Now().UTC().Format(time.RFC3339))
}

func verificationResult(r result) {
	verificationLog.Lock()
	defer verificationLog.Unlock()
	verificationProgress(r.Name, r.Status, r.DurationMS)
	if verificationLog.indices == nil {
		return
	}
	if index, exists := verificationLog.indices[r.Name]; exists {
		verificationLog.report.Results[index] = r
	} else {
		verificationLog.indices[r.Name] = len(verificationLog.report.Results)
		verificationLog.report.Results = append(verificationLog.report.Results, r)
	}
	verificationLog.report.Passed, verificationLog.report.Failed, verificationLog.report.NotRun = 0, 0, 0
	for _, item := range verificationLog.report.Results {
		switch item.Status {
		case "passed":
			verificationLog.report.Passed++
		case "failed":
			verificationLog.report.Failed++
		case "not-run":
			verificationLog.report.NotRun++
		}
	}
	if err := writeVerificationCheckpoint(); err != nil {
		// Do not print error payloads that may contain scenario data.
		fmt.Fprintln(os.Stderr, "verification checkpoint-write failed")
	}
}

func writeVerificationCheckpoint() error {
	payload := struct {
		report
		Incomplete bool `json:"incomplete"`
	}{verificationLog.report, true}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(verificationLog.path), ".verify-checkpoint-")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err = file.Write(raw); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(name, verificationLog.path)
}
