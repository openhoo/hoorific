package core

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCodexExecProfileValidationAndIdentityIsolation(t *testing.T) {
	installationID := "e7ccd59d-93e4-4cf4-bab4-4cc65f55e922"
	profile := &ClientProfile{Preset: "codex-exec", InstallationID: installationID}
	for _, connector := range []string{"openai", "compatible", "codex-subscription"} {
		if err := profile.Validate(connector); err != nil {
			t.Fatalf("Validate(%q) error = %v", connector, err)
		}
	}
	if !profile.EmulatesCodex() {
		t.Fatal("EmulatesCodex() = false for codex-exec")
	}
	first, err := NewCodexInvocation(installationID)
	if err != nil {
		t.Fatalf("NewCodexInvocation(first) error = %v", err)
	}
	second, err := NewCodexInvocation(installationID)
	if err != nil {
		t.Fatalf("NewCodexInvocation(second) error = %v", err)
	}
	if first.SessionID == second.SessionID || first.TurnID == second.TurnID || first.ContextWindowID == second.ContextWindowID {
		t.Fatal("successive invocations reused request identity")
	}
	if first.InstallationID != installationID || second.InstallationID != installationID {
		t.Fatal("configured installation ID was not preserved")
	}
	if first.ThreadID != first.SessionID || first.RootTurnID != first.TurnID || first.WindowID != first.SessionID+":0" {
		t.Fatalf("incoherent first identity: %#v", first)
	}
	if len(first.SessionID) != 36 || first.SessionID[14] != '7' || !strings.ContainsAny(first.SessionID[19:20], "89ab") {
		t.Fatalf("session ID is not UUIDv7: %q", first.SessionID)
	}
	metadata, err := first.TurnMetadata()
	if err != nil {
		t.Fatalf("TurnMetadata() error = %v", err)
	}
	var decoded CodexTurnMetadata
	if err := json.Unmarshal([]byte(metadata), &decoded); err != nil {
		t.Fatalf("TurnMetadata() JSON error = %v", err)
	}
	if decoded.InstallationID != installationID || decoded.SessionID != first.SessionID || decoded.TurnID != first.TurnID || decoded.WindowID != first.WindowID {
		t.Fatalf("metadata identity mismatch: %#v", decoded)
	}
	headers, err := first.Headers()
	if err != nil {
		t.Fatalf("Headers() error = %v", err)
	}
	if headers.Get("Authorization") != "" || headers.Get("Cookie") != "" {
		t.Fatal("identity headers carried credential or cookie state")
	}
	if headers.Get("Originator") != "codex_exec" || headers.Get("Accept") != "text/event-stream" {
		t.Fatalf("unexpected Codex persona headers: %#v", headers)
	}
}

func TestClientProfileInstallationIDRules(t *testing.T) {
	valid := "e7ccd59d-93e4-4cf4-bab4-4cc65f55e922"
	invalid := []string{
		"",
		"E7CCD59D-93E4-4CF4-BAB4-4CC65F55E922",
		"e7ccd59d93e44cf4bab44cc65f55e922",
		"e7ccd59d-93e4-5cf4-bab4-4cc65f55e922",
		"e7ccd59d-93e4-4cf4-cab4-4cc65f55e922",
	}
	for _, value := range invalid {
		if err := (&ClientProfile{Preset: "codex-exec", InstallationID: value}).Validate("openai"); err == nil {
			t.Errorf("invalid installation ID %q was accepted", value)
		}
	}
	if err := (&ClientProfile{Preset: "codex-exec", InstallationID: valid, Version: "0.153.5"}).Validate("openai"); err == nil {
		t.Error("unsupported Codex version was accepted")
	}
	for _, preset := range []string{"custom", "codex-cli", "codex-desktop", "zcode-desktop", "codex-passthrough"} {
		profile := &ClientProfile{Preset: preset, InstallationID: valid}
		if preset == "codex-cli" {
			profile.Version = "1.0"
		}
		if preset == "codex-desktop" {
			profile.Headers = map[string]string{"Originator": "desktop", "User-Agent": "desktop/1"}
		}
		if preset == "zcode-desktop" {
			profile.Version = "1.0"
		}
		if err := profile.Validate("openai"); err == nil {
			t.Errorf("installation ID accepted for non-emulated preset %q", preset)
		}
	}
	var nilProfile *ClientProfile
	if nilProfile.EmulatesCodex() {
		t.Fatal("nil EmulatesCodex() = true")
	}
}
