package core

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const CodexExecVersion = "0.153.4"

const codexExecUserAgent = "codex_exec/" + CodexExecVersion + " (Arch Linux Unknown; x86_64) dumb (codex_exec; " + CodexExecVersion + ")"

// CodexInvocation is the request-scoped identity projected by the emulated
// codex-exec profile. InstallationID is connection-scoped; all other IDs are
// newly generated for each invocation.
type CodexInvocation struct {
	InstallationID      string
	SessionID           string
	ThreadID            string
	TurnID              string
	RootTurnID          string
	WindowID            string
	ContextWindowID     string
	TurnStartedAtUnixMS int64
}

// CodexTurnMetadata is the source-supported identity and execution metadata
// carried by x-codex-turn-metadata. It intentionally omits harness context
// such as prompts, tools, workspace paths, and tool implementations.
type CodexTurnMetadata struct {
	InstallationID             string `json:"installation_id"`
	SessionID                  string `json:"session_id"`
	ThreadID                   string `json:"thread_id"`
	AgentName                  string `json:"agent_name"`
	TurnID                     string `json:"turn_id"`
	WindowID                   string `json:"window_id"`
	WindowNumber               int    `json:"window_number"`
	ContextWindowID            string `json:"context_window_id"`
	RequestKind                string `json:"request_kind"`
	RootTurnID                 string `json:"root_turn_id"`
	ThreadSource               string `json:"thread_source"`
	Sandbox                    string `json:"sandbox"`
	SandboxMode                string `json:"sandbox_mode"`
	AutoReviewEnabled          bool   `json:"auto_review_enabled"`
	NodeReplAutoReviewRequired bool   `json:"node_repl_auto_review_required"`
	NodeReplDisabled           bool   `json:"node_repl_disabled"`
	TurnStartedAtUnixMS        int64  `json:"turn_started_at_unix_ms"`
}

// NewCodexInvocation creates fresh UUIDv7 session, turn, and context IDs.
// The configured installation ID is never regenerated or accepted from a
// caller request.
func NewCodexInvocation(installationID string) (CodexInvocation, error) {
	if !IsCanonicalUUIDv4(installationID) {
		return CodexInvocation{}, fmt.Errorf("invalid Codex installation ID")
	}
	sessionID, err := newUUIDv7()
	if err != nil {
		return CodexInvocation{}, err
	}
	turnID, err := newUUIDv7()
	if err != nil {
		return CodexInvocation{}, err
	}
	contextWindowID, err := newUUIDv7()
	if err != nil {
		return CodexInvocation{}, err
	}
	return CodexInvocation{
		InstallationID:      installationID,
		SessionID:           sessionID,
		ThreadID:            sessionID,
		TurnID:              turnID,
		RootTurnID:          turnID,
		WindowID:            sessionID + ":0",
		ContextWindowID:     contextWindowID,
		TurnStartedAtUnixMS: time.Now().UnixMilli(),
	}, nil
}

// TurnMetadata returns deterministic JSON for the generated turn identity.
func (i CodexInvocation) TurnMetadata() (string, error) {
	value := CodexTurnMetadata{
		InstallationID:             i.InstallationID,
		SessionID:                  i.SessionID,
		ThreadID:                   i.ThreadID,
		AgentName:                  "/root",
		TurnID:                     i.TurnID,
		RootTurnID:                 i.RootTurnID,
		WindowID:                   i.WindowID,
		WindowNumber:               0,
		ContextWindowID:            i.ContextWindowID,
		RequestKind:                "turn",
		ThreadSource:               "user",
		Sandbox:                    "seccomp",
		SandboxMode:                "read-only",
		AutoReviewEnabled:          false,
		NodeReplAutoReviewRequired: false,
		NodeReplDisabled:           false,
		TurnStartedAtUnixMS:        i.TurnStartedAtUnixMS,
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// ClientMetadata projects identity fields used by the Responses request
// envelope. The returned map is independent and safe for caller mutation.
func (i CodexInvocation) ClientMetadata() (map[string]string, error) {
	metadata, err := i.TurnMetadata()
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"thread_id":               i.ThreadID,
		"root_turn_id":            i.RootTurnID,
		"x-codex-turn-metadata":   metadata,
		"x-codex-installation-id": i.InstallationID,
		"x-codex-window-id":       i.WindowID,
		"session_id":              i.SessionID,
		"turn_id":                 i.TurnID,
	}, nil
}

// Headers returns the measured Codex exec persona headers. Authorization is
// deliberately absent; the credential lease remains the only authority that
// may add it.
func (i CodexInvocation) Headers() (http.Header, error) {
	metadata, err := i.TurnMetadata()
	if err != nil {
		return nil, err
	}
	return http.Header{
		"X-Codex-Beta-Features": {"remote_compaction_v2"},
		"X-Codex-Window-Id":     {i.WindowID},
		"X-Codex-Turn-Metadata": {metadata},
		"X-Client-Request-Id":   {i.ThreadID},
		"Session-Id":            {i.SessionID},
		"Thread-Id":             {i.ThreadID},
		"Accept":                {"text/event-stream"},
		"Content-Type":          {"application/json"},
		"Originator":            {"codex_exec"},
		"User-Agent":            {codexExecUserAgent},
	}, nil
}

// IsCanonicalUUIDv4 accepts only lowercase, hyphenated UUIDv4 values. The
// strict spelling prevents multiple persisted representations of one ID.
func IsCanonicalUUIDv4(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for i := 0; i < len(value); i++ {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !isLowerHex(value[i]) {
			return false
		}
	}
	return value[14] == '4' && (value[19] == '8' || value[19] == '9' || value[19] == 'a' || value[19] == 'b')
}

func validateCanonicalUUIDv4(field, value string) error {
	if !IsCanonicalUUIDv4(value) {
		return fmt.Errorf("%s must be a canonical UUIDv4", field)
	}
	return nil
}

func isLowerHex(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'f'
}

func newUUIDv7() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	milliseconds := uint64(time.Now().UnixMilli())
	value[0] = byte(milliseconds >> 40)
	value[1] = byte(milliseconds >> 32)
	value[2] = byte(milliseconds >> 24)
	value[3] = byte(milliseconds >> 16)
	value[4] = byte(milliseconds >> 8)
	value[5] = byte(milliseconds)
	value[6] = value[6]&0x0f | 0x70
	value[8] = value[8]&0x3f | 0x80
	var encoded [36]byte
	hex.Encode(encoded[0:8], value[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], value[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], value[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], value[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], value[10:16])
	return string(encoded[:]), nil
}

// NewCodexMessageID creates the Responses message identifier used when a
// caller supplies ordinary string input without an explicit message ID.
func NewCodexMessageID() (string, error) {
	id, err := newUUIDv7()
	if err != nil {
		return "", err
	}
	return "msg_" + id, nil
}
