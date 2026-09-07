package realtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

type Protocol string

const (
	OpenAI Protocol = "openai"
	Gemini Protocol = "gemini"
)

type State string

const (
	StateNew     State = "new"
	StateReady   State = "ready"
	StateRunning State = "running"
	StateClosed  State = "closed"
)

type Policy struct {
	Protocol                                            Protocol
	ModelID                                             string
	AllowTools, AllowResponseCreate, AllowSessionUpdate bool
	MaxResponses                                        int
}
type Machine struct {
	policy    Policy
	state     State
	responses int
}

func NewMachine(p Policy) (*Machine, error) {
	if p.ModelID == "" || (p.Protocol != OpenAI && p.Protocol != Gemini) {
		return nil, errors.New("realtime model and protocol required")
	}
	if p.MaxResponses < 0 {
		return nil, errors.New("invalid response bound")
	}
	return &Machine{policy: p, state: StateNew}, nil
}
func (m *Machine) State() State { return m.state }
func (m *Machine) ValidateControl(raw []byte) error {
	if m.state == StateClosed {
		return errors.New("realtime session closed")
	}
	if err := uniqueJSON(raw); err != nil {
		return fmt.Errorf("invalid realtime control: %w", err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return fmt.Errorf("invalid realtime control: %w", err)
	}
	typ := stringValue(obj, "type")
	if typ == "" {
		typ = stringValue(obj, "event")
	}
	switch m.policy.Protocol {
	case OpenAI:
		delete(obj, "event_id")
		return m.validateOpenAI(typ, obj)
	case Gemini:
		return m.validateGemini(typ, obj)
	default:
		return errors.New("unsupported realtime protocol")
	}
}
func (m *Machine) validateGemini(typ string, obj map[string]json.RawMessage) error {
	_ = typ
	recognized := ""
	for _, key := range []string{"setup", "clientContent", "realtimeInput", "toolResponse"} {
		if _, ok := obj[key]; ok {
			if recognized != "" {
				return errors.New("multiple realtime envelopes")
			}
			recognized = key
		}
	}
	if len(obj) != 1 {
		return errors.New("unknown Gemini realtime control field")
	}
	switch recognized {
	case "setup":
		if m.state != StateNew {
			return errors.New("duplicate realtime setup")
		}
		setup := objectValue(obj, "setup")
		if setup == nil {
			return errors.New("invalid realtime setup")
		}
		if err := rejectUnknown(setup, []string{"model", "generationConfig", "systemInstruction", "tools", "toolConfig", "proactivity", "realtimeInputConfig", "sessionResumption", "contextWindowCompression"}); err != nil {
			return err
		}
		if v, present, err := stringField(setup, "model"); err != nil {
			return err
		} else if present && !modelMatches(v, m.policy.ModelID) {
			return errors.New("realtime model mismatch")
		}
		if _, ok := setup["tools"]; ok && !m.policy.AllowTools {
			return errors.New("realtime tools not allowed")
		}
		m.state = StateReady
		return nil
	case "clientContent", "realtimeInput", "toolResponse":
		if m.state == StateNew {
			return errors.New("realtime setup required")
		}
		nested := objectValue(obj, recognized)
		if nested == nil {
			return errors.New("invalid Gemini realtime envelope")
		}
		if err := rejectUnknown(nested, geminiNestedFields(recognized)); err != nil {
			return err
		}
		m.state = StateRunning
		return nil
	default:
		return errors.New("unsupported realtime control envelope")
	}
}
func (m *Machine) validateOpenAI(typ string, obj map[string]json.RawMessage) error {
	switch typ {
	case "session.update":
		if err := rejectUnknown(obj, []string{"type", "session"}); err != nil {
			return err
		}
		if !m.policy.AllowSessionUpdate {
			return errors.New("session updates not allowed")
		}
		session := objectValue(obj, "session")
		if _, present := obj["session"]; present && session == nil {
			return errors.New("invalid session update envelope")
		}
		if err := rejectUnknown(session, []string{"model", "modalities", "instructions", "voice", "input_audio_format", "output_audio_format", "input_audio_transcription", "turn_detection", "tools", "tool_choice", "temperature", "max_response_output_tokens", "speed", "tracing", "truncation"}); err != nil {
			return err
		}
		if v, present, err := stringField(session, "model"); err != nil {
			return err
		} else if present && v != m.policy.ModelID {
			return errors.New("realtime model mutation denied")
		}
		if _, ok := session["tools"]; ok && !m.policy.AllowTools {
			return errors.New("realtime tools not allowed")
		}
		m.state = StateReady
		return nil
	case "response.create":
		if err := rejectUnknown(obj, []string{"type", "response"}); err != nil {
			return err
		}
		if m.state == StateNew {
			return errors.New("realtime setup required")
		}
		if !m.policy.AllowResponseCreate {
			return errors.New("response creation not allowed")
		}
		response := objectValue(obj, "response")
		if _, present := obj["response"]; present && response == nil {
			return errors.New("invalid response.create envelope")
		}
		if err := rejectUnknown(response, []string{"conversation", "input", "instructions", "max_output_tokens", "metadata", "modalities", "model", "output_audio_format", "temperature", "tool_choice", "tools", "truncation"}); err != nil {
			return err
		}
		if v, present, err := stringField(response, "model"); err != nil {
			return err
		} else if present && v != m.policy.ModelID {
			return errors.New("realtime model mutation denied")
		}
		if _, ok := response["tools"]; ok && !m.policy.AllowTools {
			return errors.New("realtime tools not allowed")
		}
		if m.policy.MaxResponses > 0 && m.responses >= m.policy.MaxResponses {
			return errors.New("realtime response limit exceeded")
		}
		m.responses++
		m.state = StateRunning
		return nil
	case "conversation.item.create":
		if err := rejectUnknown(obj, []string{"type", "item"}); err != nil {
			return err
		}
		if m.state == StateNew {
			return errors.New("realtime setup required")
		}
		item := objectValue(obj, "item")
		if item == nil {
			return errors.New("invalid conversation item envelope")
		}
		if err := rejectUnknown(item, []string{"id", "type", "role", "content", "call_id", "name", "arguments", "output"}); err != nil {
			return err
		}
		if v, present, err := stringField(item, "role"); err != nil {
			return err
		} else if present && v != "user" && v != "assistant" && v != "system" && v != "tool" {
			return errors.New("unsupported conversation item role")
		}
		if _, ok := item["tools"]; ok && !m.policy.AllowTools {
			return errors.New("realtime tools not allowed")
		}
		return nil
	case "input_audio_buffer.append", "input_audio_buffer.commit":
		if err := rejectUnknown(obj, []string{"type", "audio"}); err != nil {
			return err
		}
		if m.state == StateNew {
			return errors.New("realtime setup required")
		}
		return nil
	default:
		return errors.New("unsupported realtime control event")
	}
}
func stringField(obj map[string]json.RawMessage, key string) (string, bool, error) {
	if obj == nil {
		return "", false, nil
	}
	b, ok := obj[key]
	if !ok {
		return "", false, nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return "", true, fmt.Errorf("%s must be a string", key)
	}
	return s, true, nil
}
func stringValue(obj map[string]json.RawMessage, key string) string {
	v, _, _ := stringField(obj, key)
	return v
}
func objectValue(obj map[string]json.RawMessage, key string) map[string]json.RawMessage {
	if b, ok := obj[key]; ok {
		return objectRaw(b)
	}
	return nil
}
func objectRaw(raw json.RawMessage) map[string]json.RawMessage {
	var o map[string]json.RawMessage
	if json.Unmarshal(raw, &o) != nil {
		return nil
	}
	return o
}
func rejectUnknown(obj map[string]json.RawMessage, allowed []string) error {
	if obj == nil {
		return nil
	}
	ok := make(map[string]struct{}, len(allowed))
	for _, k := range allowed {
		ok[k] = struct{}{}
	}
	for key := range obj {
		if _, yes := ok[key]; !yes {
			return fmt.Errorf("unsupported realtime control field: %s", key)
		}
	}
	return nil
}
func geminiNestedFields(kind string) []string {
	switch kind {
	case "clientContent":
		return []string{"turns", "turnComplete", "inactivityTimeout"}
	case "realtimeInput":
		return []string{"mediaChunks", "audio", "video", "text"}
	case "toolResponse":
		return []string{"functionResponses"}
	default:
		return nil
	}
}
func modelMatches(got, want string) bool {
	return got == want || strings.TrimPrefix(got, "models/") == strings.TrimPrefix(want, "models/")
}
func ValidateWebRTC(wholeSessionBound bool, requiresConcurrency bool) error {
	if !wholeSessionBound {
		return errors.New("unsupported_policy: realtime hard spend requires whole-session bound")
	}
	if requiresConcurrency {
		return errors.New("unsupported_policy: direct WebRTC cannot enforce gateway session concurrency")
	}
	return nil
}
func ValidateBinaryControl(raw []byte) error {
	if len(raw) == 0 || bytes.IndexByte(raw, 0) == 0 {
		return errors.New("invalid realtime control payload")
	}
	return nil
}
func uniqueJSON(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	var walk func() error
	walk = func() error {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]struct{}{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				ks, ok := key.(string)
				if !ok {
					return errors.New("object key is not a string")
				}
				if _, ok := seen[ks]; ok {
					return fmt.Errorf("duplicate JSON key %q", ks)
				}
				seen[ks] = struct{}{}
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = d.Token()
			return err
		case '[':
			for d.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = d.Token()
			return err
		}
		return errors.New("invalid JSON delimiter")
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}
