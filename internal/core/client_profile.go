package core

import (
	"fmt"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	clientProfileMaxHeaderCount  = 32
	clientProfileMaxHeaderValue  = 4096
	clientProfileMaxHeaderBytes  = 16 << 10
	clientProfileMaxVersionBytes = 256

	clientProfilePassthroughMaxHeaderValueBytes = 16 << 10
	clientProfilePassthroughMaxHeaderBytes      = 32 << 10
)

// ClientProfile describes the non-secret identity of an upstream client.
// It is deliberately narrower than a provider protocol implementation: the
// connector remains responsible for request and response semantics.
type ClientProfile struct {
	Preset         string            `json:"preset" enum:"custom,codex-cli,codex-desktop,zcode-desktop,codex-passthrough,codex-exec"`
	Version        string            `json:"version,omitempty"`
	InstallationID string            `json:"installation_id,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
}

var clientProfileHeaderNames = map[string]string{
	"user-agent":                  "User-Agent",
	"originator":                  "Originator",
	"version":                     "Version",
	"http-referer":                "HTTP-Referer",
	"x-title":                     "X-Title",
	"x-zcode-app-version":         "X-Zcode-App-Version",
	"x-zcode-agent":               "X-Zcode-Agent",
	"anthropic-beta":              "Anthropic-Beta",
	"x-stainless-lang":            "X-Stainless-Lang",
	"x-stainless-package-version": "X-Stainless-Package-Version",
	"x-stainless-os":              "X-Stainless-OS",
	"x-stainless-arch":            "X-Stainless-Arch",
	"x-stainless-runtime":         "X-Stainless-Runtime",
	"x-stainless-runtime-version": "X-Stainless-Runtime-Version",
}

var clientProfilePassthroughHeaderNames = map[string]string{
	"originator":            "Originator",
	"user-agent":            "User-Agent",
	"session-id":            "Session-Id",
	"thread-id":             "Thread-Id",
	"x-client-request-id":   "X-Client-Request-Id",
	"x-codex-window-id":     "X-Codex-Window-Id",
	"x-codex-turn-metadata": "X-Codex-Turn-Metadata",
	"x-codex-beta-features": "X-Codex-Beta-Features",
	"accept":                "Accept",
	"content-type":          "Content-Type",
	"accept-encoding":       "Accept-Encoding",
}

var clientProfileConnectors = map[string]map[string]struct{}{
	"custom": {
		"openai":             {},
		"anthropic":          {},
		"compatible":         {},
		"codex-subscription": {},
	},
	"codex-cli": {
		"codex-subscription": {},
		"openai":             {},
		"compatible":         {},
	},
	"codex-desktop": {
		"codex-subscription": {},
		"openai":             {},
		"compatible":         {},
	},
	"zcode-desktop": {
		"compatible": {},
		"anthropic":  {},
		"openai":     {},
	},
	"codex-passthrough": {
		"openai":             {},
		"compatible":         {},
		"codex-subscription": {},
	},
	"codex-exec": {
		"openai":             {},
		"compatible":         {},
		"codex-subscription": {},
	},
}

// Validate checks the profile shape and whether its preset is supported by
// connector. A nil profile is valid and preserves legacy connections.
func (p *ClientProfile) Validate(connector string) error {
	if p == nil {
		return nil
	}
	if err := p.validateShape(); err != nil {
		return err
	}
	connectors, ok := clientProfileConnectors[p.Preset]
	if !ok {
		// validateShape normally catches this; keep the guard local so future
		// callers cannot accidentally make an unknown preset permissive.
		return fmt.Errorf("client_profile.preset is unsupported")
	}
	if _, ok := connectors[connector]; !ok {
		if p.PreservesClientHeaders() {
			return clientProfilePassthroughError("client_profile.preset", "codex-passthrough is incompatible with this connector")
		}
		return fmt.Errorf("client_profile.preset is incompatible with connector")
	}
	return nil
}

func (p *ClientProfile) validateShape() error {
	switch p.Preset {
	case "custom", "codex-cli", "codex-desktop", "zcode-desktop", "codex-passthrough", "codex-exec":
	default:
		if p.Preset == "" {
			return fmt.Errorf("client_profile.preset is required")
		}
		return fmt.Errorf("client_profile.preset is unsupported")
	}

	if p.Preset != "codex-exec" && p.InstallationID != "" {
		return fmt.Errorf("client_profile.installation_id is only valid for codex-exec")
	}
	if p.Preset == "codex-exec" {
		if p.Version != "" && p.Version != CodexExecVersion {
			return fmt.Errorf("client_profile.version must be empty or %s for codex-exec", CodexExecVersion)
		}
		if err := validateCanonicalUUIDv4("client_profile.installation_id", p.InstallationID); err != nil {
			return err
		}
		if len(p.Headers) != 0 {
			return fmt.Errorf("client_profile.headers are not supported for codex-exec")
		}
		return nil

	}
	if p.Preset == "codex-passthrough" {
		if p.Version != "" {
			return clientProfilePassthroughError("client_profile.version", "codex-passthrough requires an empty version")
		}
		if len(p.Headers) != 0 {
			return clientProfilePassthroughError("client_profile.headers", "codex-passthrough requires empty static headers")
		}
		return nil
	}

	if p.Version != "" {
		if err := validateClientProfileText("client_profile.version", p.Version, clientProfileMaxVersionBytes, false); err != nil {
			return err
		}
	}
	if p.Preset == "zcode-desktop" && p.Version == "" {
		return fmt.Errorf("client_profile.version is required for this preset")
	}

	if len(p.Headers) > clientProfileMaxHeaderCount {
		return fmt.Errorf("client_profile.headers contains too many entries")
	}
	seen := make(map[string]struct{}, len(p.Headers))
	totalBytes := 0
	for key, value := range p.Headers {
		folded := strings.ToLower(key)
		if _, ok := clientProfileHeaderNames[folded]; !ok {
			return fmt.Errorf("client_profile.headers contains an unsupported header")
		}
		if _, ok := seen[folded]; ok {
			return fmt.Errorf("client_profile.headers contains duplicate header names")
		}
		seen[folded] = struct{}{}
		if err := validateClientProfileText("client_profile.headers", value, clientProfileMaxHeaderValue, true); err != nil {
			return err
		}
		totalBytes += len(key) + len(value)
		if totalBytes > clientProfileMaxHeaderBytes {
			return fmt.Errorf("client_profile.headers is too large")
		}
	}

	if p.Preset == "codex-cli" && p.Version == "" && !clientProfileHasHeader(p.Headers, "User-Agent") {
		return fmt.Errorf("client_profile.version or client_profile.headers.user_agent is required for this preset")
	}
	if p.Preset == "codex-desktop" {
		if !clientProfileHasHeader(p.Headers, "Originator") {
			return fmt.Errorf("client_profile.headers.originator is required for this preset")
		}
		if !clientProfileHasHeader(p.Headers, "User-Agent") {
			return fmt.Errorf("client_profile.headers.user_agent is required for this preset")
		}
	}
	return nil
}

func validateClientProfileText(field, value string, limit int, required bool) error {
	if value == "" {
		if required {
			return fmt.Errorf("%s is required", field)
		}
		return nil
	}
	if len(value) > limit || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return fmt.Errorf("%s is invalid", field)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s contains a control character", field)
		}
	}
	return nil
}

func clientProfileHasHeader(headers map[string]string, name string) bool {
	for key := range headers {
		if strings.EqualFold(key, name) {
			return true
		}
	}
	return false
}

// Apply resolves the preset defaults and explicit overrides into headers.
// Existing keys are replaced case-insensitively. The profile map is never
// mutated. Connector compatibility must be checked with Validate first,
// because Apply intentionally has no connector argument.
func (p *ClientProfile) Apply(headers http.Header) error {
	if p == nil {
		return nil
	}
	if err := p.validateShape(); err != nil {
		return err
	}

	effective := make(map[string]string, len(p.Headers)+4)
	switch p.Preset {
	case "codex-exec":
		effective["Originator"] = "codex_exec"
		effective["User-Agent"] = codexExecUserAgent
	case "codex-cli":
		effective["Originator"] = "codex_cli_rs"
		if p.Version != "" {
			effective["User-Agent"] = "codex_cli_rs/" + p.Version
		}
	case "zcode-desktop":
		effective["HTTP-Referer"] = "https://zcode.z.ai"
		effective["X-Title"] = "Z Code"
		effective["User-Agent"] = "ZCode/" + p.Version
		effective["X-Zcode-App-Version"] = p.Version
	}
	for key, value := range p.Headers {
		canonical := clientProfileHeaderNames[strings.ToLower(key)]
		effective[canonical] = value
	}
	if len(effective) == 0 {
		return nil
	}
	if headers == nil {
		return fmt.Errorf("client_profile.headers cannot be applied to nil request headers")
	}

	for key, value := range effective {
		for existing := range headers {
			if strings.EqualFold(existing, key) {
				delete(headers, existing)
			}
		}
		headers.Set(key, value)
	}
	return nil
}

// EmulatesCodex reports whether the connection must use the isolated,
// caller-independent Codex native wire persona.
func (p *ClientProfile) EmulatesCodex() bool {
	return p != nil && p.Preset == "codex-exec"
}

// PreservesClientHeaders reports whether this profile carries selected
// per-request headers from the original caller instead of static defaults.
// A nil profile preserves the legacy behavior and returns false.
func (p *ClientProfile) PreservesClientHeaders() bool {
	return p != nil && p.Preset == "codex-passthrough"
}

// ApplyRequestHeaders applies this profile to dst, optionally using selected
// headers from src for the passthrough preset. Static profiles intentionally
// ignore src and retain their existing Apply behavior.
func (p *ClientProfile) ApplyRequestHeaders(dst, src http.Header) error {
	if p == nil || !p.PreservesClientHeaders() {
		return p.Apply(dst)
	}
	if err := p.validateShape(); err != nil {
		return err
	}

	effective, err := collectClientProfilePassthroughHeaders(src)
	if err != nil {
		return err
	}
	if dst == nil {
		return clientProfilePassthroughError("client_profile.headers", "codex-passthrough requires destination headers")
	}

	for existing := range dst {
		if _, selected := clientProfilePassthroughHeaderNames[strings.ToLower(existing)]; selected {
			delete(dst, existing)
		}
	}
	for folded, value := range effective {
		canonical := clientProfilePassthroughHeaderNames[folded]
		// Assign a fresh one-element slice rather than retaining src's
		// value slice. Header values themselves are immutable strings.
		dst[canonical] = []string{value}
	}
	return nil
}

func clientProfilePassthroughError(param, message string) error {
	return GatewayError{
		Code:       "invalid_request",
		HTTPStatus: http.StatusBadRequest,
		Param:      param,
		Message:    message,
		Origin:     "gateway",
	}
}

func clientProfilePassthroughHeaderParam(canonical string) string {
	return "client_profile.headers." + strings.ToLower(canonical)
}

func collectClientProfilePassthroughHeaders(src http.Header) (map[string]string, error) {
	effective := make(map[string]string, len(clientProfilePassthroughHeaderNames))
	totalBytes := 0
	for key, values := range src {
		folded := strings.ToLower(key)
		canonical, selected := clientProfilePassthroughHeaderNames[folded]
		if selected {
			param := clientProfilePassthroughHeaderParam(canonical)
			if _, exists := effective[folded]; exists {
				return nil, clientProfilePassthroughError(param, "Codex passthrough rejects duplicate header names")
			}
			if len(values) != 1 {
				return nil, clientProfilePassthroughError(param, "Codex passthrough requires a single header value")
			}
			value := values[0]
			required := folded == "originator" || folded == "user-agent"
			if err := validateClientProfilePassthroughValue(param, value, required); err != nil {
				return nil, err
			}
			if folded == "accept-encoding" && !strings.EqualFold(value, "identity") {
				return nil, clientProfilePassthroughError(param, "Codex passthrough accepts only identity encoding")
			}
			totalBytes += len(key) + len(value)
			if totalBytes > clientProfilePassthroughMaxHeaderBytes {
				return nil, clientProfilePassthroughError("client_profile.headers", "Codex passthrough headers are too large")
			}
			effective[folded] = value
			continue
		}
		if strings.HasPrefix(folded, "x-codex-") {
			return nil, clientProfilePassthroughError("client_profile.headers.x-codex", "Codex passthrough rejects unknown x-codex metadata")
		}
	}

	if _, ok := effective["originator"]; !ok {
		return nil, clientProfilePassthroughError("client_profile.headers.originator", "Codex passthrough requires Originator")
	}
	if _, ok := effective["user-agent"]; !ok {
		return nil, clientProfilePassthroughError("client_profile.headers.user-agent", "Codex passthrough requires User-Agent")
	}

	for key, values := range src {
		if !strings.EqualFold(key, "Connection") {
			continue
		}
		for _, value := range values {
			for _, token := range strings.Split(value, ",") {
				token = strings.TrimSpace(token)
				if token == "" {
					continue
				}
				folded := strings.ToLower(token)
				if _, selected := clientProfilePassthroughHeaderNames[folded]; !selected {
					continue
				}
				if _, forwarded := effective[folded]; forwarded {
					return nil, clientProfilePassthroughError(clientProfilePassthroughHeaderParam(clientProfilePassthroughHeaderNames[folded]), "Codex passthrough rejects hop-by-hop headers")
				}
			}
		}
	}
	return effective, nil
}

func validateClientProfilePassthroughValue(param, value string, required bool) error {
	if value == "" {
		if required {
			return clientProfilePassthroughError(param, "Codex passthrough requires a nonempty header value")
		}
		return nil
	}
	if len(value) > clientProfilePassthroughMaxHeaderValueBytes {
		return clientProfilePassthroughError(param, "Codex passthrough header value is too large")
	}
	if !utf8.ValidString(value) {
		return clientProfilePassthroughError(param, "Codex passthrough header value is invalid")
	}
	if strings.TrimSpace(value) != value {
		return clientProfilePassthroughError(param, "Codex passthrough header value has edge whitespace")
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return clientProfilePassthroughError(param, "Codex passthrough header value contains a control character")
		}
	}
	return nil
}

// Clone returns an independent profile suitable for an immutable runtime
// snapshot. Nil remains nil, and a nil Headers map remains nil.
func (p *ClientProfile) Clone() *ClientProfile {
	if p == nil {
		return nil
	}
	clone := *p
	if p.Headers != nil {
		clone.Headers = make(map[string]string, len(p.Headers))
		for key, value := range p.Headers {
			clone.Headers[key] = value
		}
	}
	return &clone
}
