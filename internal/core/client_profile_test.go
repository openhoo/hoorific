package core

import (
	"net/http"
	"strings"
	"testing"
)

func TestClientProfileValidateRejectsUnsafeAndUnsupportedHeaders(t *testing.T) {
	tests := []struct {
		name      string
		profile   *ClientProfile
		connector string
		want      string
		leak      string
	}{
		{
			name:      "missing preset",
			profile:   &ClientProfile{},
			connector: "openai",
			want:      "client_profile.preset",
		},
		{
			name:      "missing CLI identity",
			profile:   &ClientProfile{Preset: "codex-cli"},
			connector: "openai",
			want:      "client_profile.version",
		},
		{
			name:      "unknown preset",
			profile:   &ClientProfile{Preset: "unknown"},
			connector: "openai",
			want:      "client_profile.preset",
		},
		{
			name:      "unsupported connector",
			profile:   &ClientProfile{Preset: "custom"},
			connector: "gemini",
			want:      "client_profile.preset",
		},
		{
			name:      "protected header",
			profile:   &ClientProfile{Preset: "custom", Headers: map[string]string{"Authorization": "Bearer secret"}},
			connector: "openai",
			want:      "client_profile.headers",
			leak:      "Bearer secret",
		},
		{
			name:      "forwarded header",
			profile:   &ClientProfile{Preset: "custom", Headers: map[string]string{"X-Forwarded-For": "127.0.0.1"}},
			connector: "openai",
			want:      "client_profile.headers",
			leak:      "127.0.0.1",
		},
		{
			name:      "injection value",
			profile:   &ClientProfile{Preset: "custom", Headers: map[string]string{"User-Agent": "safe\r\nX-Evil: yes"}},
			connector: "openai",
			want:      "client_profile.headers",
			leak:      "X-Evil",
		},
		{
			name: "case insensitive duplicate",
			profile: &ClientProfile{Preset: "custom", Headers: map[string]string{
				"User-Agent": "one",
				"user-agent": "two",
			}},
			connector: "openai",
			want:      "client_profile.headers",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.profile.Validate(test.connector)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want field %q", err, test.want)
			}
			if test.leak != "" && strings.Contains(err.Error(), test.leak) {
				t.Fatalf("Validate() error echoed unsafe value %q: %v", test.leak, err)
			}
		})
	}
}

func TestClientProfileValidateConnectorMatrix(t *testing.T) {
	valid := []struct {
		preset    string
		connector string
		version   string
		headers   map[string]string
	}{
		{preset: "custom", connector: "openai"},
		{preset: "custom", connector: "anthropic"},
		{preset: "custom", connector: "compatible"},
		{preset: "custom", connector: "codex-subscription"},
		{preset: "codex-cli", connector: "codex-subscription", version: "1.0"},
		{preset: "codex-desktop", connector: "openai", headers: map[string]string{"Originator": "codex_desktop", "User-Agent": "Codex/1"}},
		{preset: "zcode-desktop", connector: "compatible", version: "1.0"},
		{preset: "zcode-desktop", connector: "anthropic", version: "1.0"},
	}
	for _, test := range valid {
		profile := &ClientProfile{Preset: test.preset, Version: test.version, Headers: test.headers}
		if err := profile.Validate(test.connector); err != nil {
			t.Errorf("Validate(%q) for %q = %v", test.connector, test.preset, err)
		}
	}

	invalid := []struct {
		preset    string
		connector string
		version   string
		headers   map[string]string
	}{
		{preset: "custom", connector: "gemini"},
		{preset: "codex-cli", connector: "anthropic", version: "1.0"},
		{preset: "codex-desktop", connector: "anthropic", headers: map[string]string{"Originator": "fixture-desktop", "User-Agent": "fixture-desktop/1"}},
		{preset: "zcode-desktop", connector: "codex-subscription", version: "1.0"},
		{preset: "zcode-desktop", connector: "openai"},
	}
	for _, test := range invalid {
		profile := &ClientProfile{Preset: test.preset, Version: test.version, Headers: test.headers}
		if err := profile.Validate(test.connector); err == nil {
			t.Errorf("Validate(%q) for %q succeeded, want error", test.connector, test.preset)
		}
	}
}

func TestClientProfileApplyDefaultsAndOverridesCaseInsensitively(t *testing.T) {
	profile := &ClientProfile{
		Preset: "codex-cli",
		Headers: map[string]string{
			"USER-AGENT": "Codex/Custom",
			"originator": "operator",
		},
	}
	headers := http.Header{
		"user-agent":  {"caller"},
		"ORIGINATOR":  {"caller"},
		"X-Unrelated": {"preserved"},
	}
	if err := profile.Apply(headers); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if got := headers.Get("User-Agent"); got != "Codex/Custom" {
		t.Fatalf("User-Agent = %q, want explicit override", got)
	}
	if got := headers.Get("Originator"); got != "operator" {
		t.Fatalf("Originator = %q, want explicit override", got)
	}
	if got := headers.Get("X-Unrelated"); got != "preserved" {
		t.Fatalf("unrelated header = %q, want preserved", got)
	}
	for key := range headers {
		if strings.EqualFold(key, "user-agent") && key != "User-Agent" {
			t.Fatalf("case-variant User-Agent key remained: %q", key)
		}
		if strings.EqualFold(key, "originator") && key != "Originator" {
			t.Fatalf("case-variant Originator key remained: %q", key)
		}
	}
}

func TestClientProfileApplyZCodeDefaultsAndClone(t *testing.T) {
	profile := &ClientProfile{Preset: "zcode-desktop", Version: "2.4.1", Headers: map[string]string{"X-Title": "Operator"}}
	before := profile.Clone()
	headers := make(http.Header)
	if err := profile.Apply(headers); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	for key, want := range map[string]string{
		"HTTP-Referer":        "https://zcode.z.ai",
		"X-Title":             "Operator",
		"User-Agent":          "ZCode/2.4.1",
		"X-Zcode-App-Version": "2.4.1",
	} {
		if got := headers.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	profile.Headers["X-Title"] = "mutated"
	if before.Headers["X-Title"] != "Operator" {
		t.Fatal("Clone() aliased Headers map")
	}
}

func TestClientProfileNilCompatibility(t *testing.T) {
	var profile *ClientProfile
	if err := profile.Validate("openai"); err != nil {
		t.Fatalf("nil Validate() error = %v", err)
	}
	if err := profile.Apply(nil); err != nil {
		t.Fatalf("nil Apply() error = %v", err)
	}
	if got := profile.Clone(); got != nil {
		t.Fatalf("nil Clone() = %#v, want nil", got)
	}
}
