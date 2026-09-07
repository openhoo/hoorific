package cohere

import (
	"bytes"
	"context"
	"encoding/json"
	"io"

	"hoorific/internal/core"
)

type wireRequest struct {
	Model     string        `json:"model"`
	Messages  []wireMessage `json:"messages"`
	Tools     []wireTool    `json:"tools,omitempty"`
	MaxTokens *int64        `json:"max_tokens,omitempty"`
	Stop      []string      `json:"stop_sequences,omitempty"`
	Stream    bool          `json:"stream"`
}
type wireMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  []wireCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}
type wireContent struct {
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Content    json.RawMessage `json:"content,omitempty"`
}
type wireCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}
type wireFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}
type wireTool struct {
	Type     string         `json:"type"`
	Function wireDefinition `json:"function"`
}
type wireDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

func (c *Codec) DecodeRequest(ctx context.Context, r io.Reader) (core.RequestPayload, error) {
	var x wireRequest
	if err := readJSON(ctx, r, &x); err != nil {
		return nil, err
	}
	if x.Model == "" {
		return nil, invalid("model", "is required")
	}
	conv := core.Conversation{Model: x.Model, MaxOutputTokens: x.MaxTokens, Stop: append([]string(nil), x.Stop...), Stream: x.Stream}
	for i, m := range x.Messages {
		if m.Role != "assistant" && len(m.ToolCalls) > 0 {
			return nil, unsupported(at("messages", i) + ".tool_calls")
		}
		if m.Role != "tool" && m.ToolCallID != "" {
			return nil, unsupported(at("messages", i) + ".tool_call_id")
		}
		var blocks []core.ContentBlock
		noContent := len(m.Content) == 0 || bytes.Equal(bytes.TrimSpace(m.Content), []byte("null"))
		if !(m.Role == "assistant" && noContent && len(m.ToolCalls) > 0) {
			var err error
			blocks, err = decodeContent(m.Content, at("messages", i)+".content")
			if err != nil {
				return nil, err
			}
		}
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			for j, call := range m.ToolCalls {
				if call.Type != "function" || call.ID == "" || call.Function.Name == "" {
					return nil, invalid(at("messages", i)+".tool_calls", "invalid function call")
				}
				if err := objectJSON([]byte(call.Function.Arguments), at("messages", i)+".tool_calls"); err != nil {
					return nil, err
				}
				blocks = append(blocks, core.ContentBlock{Kind: "tool_call", ID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments})
				_ = j
			}
		}
		if m.Role == "tool" {
			if m.ToolCallID == "" {
				return nil, invalid(at("messages", i)+".tool_call_id", "is required")
			}
			if len(blocks) != 1 || blocks[0].Kind != "text" {
				return nil, unsupported(at("messages", i) + ".content")
			}
			blocks[0].Kind = "tool_result"
			blocks[0].ID = m.ToolCallID
		}
		if m.Role == "system" {
			conv.System = append(conv.System, blocks...)
			continue
		}
		conv.Messages = append(conv.Messages, core.Message{Role: m.Role, Content: blocks})
	}
	for i, t := range x.Tools {
		if t.Type != "function" || t.Function.Name == "" {
			return nil, invalid(at("tools", i), "invalid function definition")
		}
		if err := objectJSON(t.Function.Parameters, at("tools", i)+".function.parameters"); err != nil {
			return nil, err
		}
		conv.Tools = append(conv.Tools, core.Tool{Name: t.Function.Name, Description: t.Function.Description, Schema: append([]byte(nil), t.Function.Parameters...)})
	}
	return conv, nil
}
func decodeContent(raw json.RawMessage, param string) ([]core.ContentBlock, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, invalid(param, "is required")
	}
	var s string
	if raw[0] == '"' {
		if err := strict(raw, &s, param); err != nil {
			return nil, err
		}
		return []core.ContentBlock{{Kind: "text", Text: s}}, nil
	}
	var arr []wireContent
	if err := strict(raw, &arr, param); err != nil {
		return nil, err
	}
	out := make([]core.ContentBlock, 0, len(arr))
	for i, b := range arr {
		switch b.Type {
		case "text":
			out = append(out, core.ContentBlock{Kind: "text", Text: b.Text})
		case "tool_result":
			if b.ToolCallID == "" {
				return nil, invalid(at(param, i)+".tool_call_id", "is required")
			}
			var text string
			bc := bytes.TrimSpace(b.Content)
			if len(bc) == 0 || bc[0] != '"' {
				return nil, unsupported(at(param, i) + ".content")
			}
			if err := strict(bc, &text, param); err != nil {
				return nil, err
			}
			out = append(out, core.ContentBlock{Kind: "tool_result", ID: b.ToolCallID, Text: text})
		default:
			return nil, unsupported(at(param, i) + ".type")
		}
	}
	return out, nil
}
func (c *Codec) EncodeRequest(ctx context.Context, p core.RequestPayload, w io.Writer) error {
	conv, ok := p.(core.Conversation)
	if !ok {
		if cp, yes := p.(*core.Conversation); yes && cp != nil {
			conv = *cp
			ok = true
		}
	}
	if !ok {
		return invalid("request", "expected core.Conversation")
	}
	if conv.StructuredOutput != nil || conv.StructuredOutputName != "" || conv.StructuredOutputDescription != "" || conv.StructuredOutputMode != "" || conv.StructuredOutputStrict != nil {
		return unsupported("structured_output")
	}
	x := wireRequest{Model: conv.Model, MaxTokens: conv.MaxOutputTokens, Stop: append([]string(nil), conv.Stop...), Stream: conv.Stream}
	if x.Model == "" {
		return invalid("model", "is required")
	}
	if len(conv.System) > 0 {
		m, err := encodeMessage("system", conv.System, "")
		if err != nil {
			return err
		}
		x.Messages = append(x.Messages, m)
	}
	for i, m := range conv.Messages {
		wm, err := encodeMessage(m.Role, m.Content, "")
		if err != nil {
			return invalid(at("messages", i)+".content", err.Error())
		}
		x.Messages = append(x.Messages, wm)
	}
	for i, t := range conv.Tools {
		if t.Name == "" {
			return invalid(at("tools", i)+".name", "is required")
		}
		if err := objectJSON(t.Schema, at("tools", i)+".parameters"); err != nil {
			return err
		}
		x.Tools = append(x.Tools, wireTool{Type: "function", Function: wireDefinition{Name: t.Name, Description: t.Description, Parameters: t.Schema}})
	}
	return writeJSON(ctx, w, x)
}
func encodeMessage(role string, blocks []core.ContentBlock, toolID string) (wireMessage, error) {
	if role != "system" && role != "user" && role != "assistant" && role != "tool" {
		return wireMessage{}, invalid("role", "unknown role")
	}
	m := wireMessage{Role: role}
	var texts []wireContent
	for _, b := range blocks {
		if b.Kind == "text" && len(m.ToolCalls) > 0 {
			return m, unsupported("assistant block order")
		}
		if err := cleanBlock(b, b.Kind); err != nil {
			return m, err
		}
		switch b.Kind {
		case "text":
			texts = append(texts, wireContent{Type: "text", Text: b.Text})
		case "tool_call":
			if role != "assistant" {
				return m, unsupported("tool_call.role")
			}
			m.ToolCalls = append(m.ToolCalls, wireCall{ID: b.ID, Type: "function", Function: wireFunction{Name: b.Name, Arguments: b.Arguments}})
		case "tool_result":
			if role != "tool" {
				return m, unsupported("tool_result.role")
			}
			if m.ToolCallID != "" && m.ToolCallID != b.ID {
				return m, invalid("tool_result.id", "multiple ids in one tool message")
			}
			m.ToolCallID = b.ID
			texts = append(texts, wireContent{Type: "text", Text: b.Text})
		default:
			return m, unsupported("content." + b.Kind)
		}
	}
	if role == "tool" {
		if m.ToolCallID == "" {
			return m, invalid("tool_call_id", "is required")
		}
		if len(texts) != 1 {
			return m, unsupported("tool.content")
		}
	}
	if len(texts) == 1 && len(m.ToolCalls) == 0 {
		m.Content, _ = json.Marshal(texts[0].Text)
	} else if len(texts) > 0 {
		m.Content, _ = json.Marshal(texts)
	}
	return m, nil
}
