package anthropic

import (
	"encoding/json"
	"hoorific/internal/core"
)

type resultWire struct {
	ID           string            `json:"id"`
	Type         string            `json:"type"`
	Role         string            `json:"role"`
	Model        string            `json:"model"`
	Content      []json.RawMessage `json:"content"`
	StopReason   *string           `json:"stop_reason"`
	StopSequence *string           `json:"stop_sequence"`
	Usage        *usageWire        `json:"usage,omitempty"`
}

func resultFromWire(a resultWire) (core.ResultPayload, error) {
	if err := required(a.ID, "id"); err != nil {
		return nil, err
	}
	if a.Type != "message" {
		return nil, unsupported("type")
	}
	if a.Role != "assistant" {
		return nil, unsupported("role")
	}
	if err := required(a.Model, "model"); err != nil {
		return nil, err
	}
	if a.Content == nil {
		return nil, unsupported("content")
	}
	g := core.GenerationResult{ID: a.ID, Model: a.Model}
	for i, r := range a.Content {
		b, e := responseBlockFromRaw(r, field("content", i))
		if e != nil {
			return nil, e
		}
		g.Blocks = append(g.Blocks, b)
	}
	reason := "end_turn"
	if a.StopReason != nil {
		reason = *a.StopReason
	}
	canonical := ""
	status := "completed"
	switch reason {
	case "end_turn":
		canonical = "stop"
	case "stop_sequence":
		canonical = "stop_sequence"
	case "tool_use":
		canonical = "tool_calls"
	case "max_tokens":
		canonical = "length"
		status = "incomplete"
	case "pause_turn", "refusal":
		canonical = reason
	case "model_context_window_exceeded":
		canonical = "context_length_exceeded"
		status = "incomplete"
	default:
		return nil, unsupported("stop_reason")
	}
	g.Finish.Status = status
	g.Finish.Reason = canonical
	if a.StopSequence != nil {
		if reason != "stop_sequence" {
			return nil, unsupported("stop_sequence")
		}
		g.Finish.Reason = "stop_sequence"
	}
	if a.Usage != nil {
		u, err := usageFromWire(a.Usage)
		if err != nil {
			return nil, err
		}
		g.Usage = u
	}
	return g, nil
}
func resultToWire(g core.GenerationResult) (resultWire, error) {
	if err := required(g.ID, "id"); err != nil {
		return resultWire{}, err
	}
	if err := required(g.Model, "model"); err != nil {
		return resultWire{}, err
	}
	if g.Finish.Cancellation != "" {
		return resultWire{}, unsupported("finish.cancellation")
	}
	a := resultWire{ID: g.ID, Type: "message", Role: "assistant", Model: g.Model}
	for i, b := range g.Blocks {
		x, e := blockToWire(b, field("content", i))
		if e != nil {
			return a, e
		}
		raw, e := json.Marshal(x)
		if e != nil {
			return a, e
		}
		a.Content = append(a.Content, raw)
	}
	reason := g.Finish.Reason
	if reason == "" {
		if g.Finish.Status == "incomplete" {
			reason = "length"
		} else {
			reason = "stop"
		}
	}
	wireReason := reason
	if g.Finish.Status == "" {
		g.Finish.Status = "completed"
	}
	switch reason {
	case "stop":
		wireReason = "end_turn"
	case "stop_sequence":
		wireReason = "stop_sequence"
	case "tool_calls":
		wireReason = "tool_use"
	case "length":
		wireReason = "max_tokens"
	case "pause_turn", "refusal":
		wireReason = reason
	case "context_length_exceeded":
		wireReason = "model_context_window_exceeded"
	default:
		return a, unsupported("finish.reason")
	}
	if (reason == "length" || reason == "context_length_exceeded") && g.Finish.Status != "incomplete" {
		return a, unsupported("finish.status")
	}
	if reason != "length" && reason != "context_length_exceeded" && g.Finish.Status != "completed" {
		return a, unsupported("finish.status")
	}
	a.StopReason = &wireReason
	if g.Usage != nil {
		u, err := usageToWire(g.Usage)
		if err != nil {
			return a, err
		}
		a.Usage = u
	}
	return a, nil
}

func reasonFromWire(reason string) (string, string, error) {
	switch reason {
	case "end_turn":
		return "stop", "completed", nil
	case "stop_sequence":
		return "stop_sequence", "completed", nil
	case "tool_use":
		return "tool_calls", "completed", nil
	case "max_tokens":
		return "length", "incomplete", nil
	case "pause_turn", "refusal":
		return reason, "completed", nil
	case "model_context_window_exceeded":
		return "context_length_exceeded", "incomplete", nil
	default:
		return "", "", unsupported("stop_reason")
	}
}

func reasonToWire(reason string) (string, string, error) {
	switch reason {
	case "stop":
		return "end_turn", "completed", nil
	case "stop_sequence":
		return "stop_sequence", "completed", nil
	case "tool_calls":
		return "tool_use", "completed", nil
	case "length":
		return "max_tokens", "incomplete", nil
	case "pause_turn", "refusal":
		return reason, "completed", nil
	case "context_length_exceeded":
		return "model_context_window_exceeded", "incomplete", nil
	default:
		return "", "", unsupported("finish.reason")
	}
}
