// Package cohere translates the text and function-tool subset of Cohere v2 chat.
package cohere

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"hoorific/internal/core"
)

// Codec is stateless; each stream owns its lifecycle state.
type Codec struct{}

func New() *Codec { return &Codec{} }

var _ core.RequestCodec = (*Codec)(nil)
var _ core.ResultCodec = (*Codec)(nil)
var _ core.StreamCodec = (*Codec)(nil)
var _ core.EventDecoder = (*streamDecoder)(nil)
var _ core.EventEncoder = (*streamEncoder)(nil)

const maxBody = 16 << 20

func invalid(param, message string) error {
	return core.GatewayError{Code: "invalid_request", HTTPStatus: 400, Param: param, Message: "cohere: " + message, Origin: "gateway"}
}
func unsupported(param string) error {
	return core.GatewayError{Code: "unsupported_feature", HTTPStatus: 400, Param: param, Message: "cohere: unsupported semantics in " + param, Origin: "gateway"}
}
func finishStatus(reason string) (string, string, error) {
	switch strings.ToUpper(reason) {
	case "COMPLETE":
		return "completed", "", nil
	case "STOP_SEQUENCE":
		return "stop", "stop_sequence", nil
	case "MAX_TOKENS":
		return "length", "length", nil
	case "TOOL_CALL":
		return "tool_calls", "tool_calls", nil
	case "ERROR", "TIMEOUT":
		return "error", "error", nil
	default:
		return "", "", unsupported("finish_reason")
	}
}
func finishReason(status, reason string) (string, error) {
	if reason != "" {
		switch reason {
		case "stop", "stop_sequence":
			if status != "stop" {
				return "", unsupported("finish.reason")
			}
			return "STOP_SEQUENCE", nil
		case "length":
			if status != "length" {
				return "", unsupported("finish.reason")
			}
			return "MAX_TOKENS", nil
		case "tool_calls":
			if status != "tool_calls" {
				return "", unsupported("finish.reason")
			}
			return "TOOL_CALL", nil
		case "error":
			if status != "error" {
				return "", unsupported("finish.reason")
			}
			return "ERROR", nil
		case "content_filter":
			return "", unsupported("finish.reason")
		default:
			return "", unsupported("finish.reason")
		}
	}
	switch status {
	case "", "completed":
		return "COMPLETE", nil
	case "stop":
		return "STOP_SEQUENCE", nil
	case "length":
		return "MAX_TOKENS", nil
	case "tool_calls":
		return "TOOL_CALL", nil
	case "error":
		return "ERROR", nil
	case "content_filter":
		return "", unsupported("finish.status")
	}
	return "", unsupported("finish.status")
}

// Check tokens before unmarshalling: encoding/json otherwise accepts duplicate keys,
// including escaped aliases and duplicates hidden inside tool schemas/arguments.
func uniqueJSON(raw []byte, param string) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 128 {
			return invalid(param, "JSON nesting exceeds 128")
		}
		t, err := d.Token()
		if err != nil {
			return invalid(param, "invalid JSON")
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return invalid(param, "invalid JSON object")
				}
				s, ok := key.(string)
				if !ok {
					return invalid(param, "invalid JSON key")
				}
				if seen[s] {
					return invalid(param, "duplicate JSON key")
				}
				seen[s] = true
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		default:
			return invalid(param, "invalid JSON delimiter")
		}
		end, err := d.Token()
		if err != nil || (delim == '{' && end != json.Delim('}')) || (delim == '[' && end != json.Delim(']')) {
			return invalid(param, "invalid JSON closure")
		}
		return nil
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return invalid(param, "trailing JSON data")
	}
	return nil
}
func strict(raw []byte, out any, param string) error {
	if err := uniqueJSON(raw, param); err != nil {
		return err
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return invalid(param, "expected a non-null value")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		const prefix = "json: unknown field "
		if strings.HasPrefix(err.Error(), prefix) {
			return unsupported(param + "." + strings.Trim(err.Error()[len(prefix):], "\""))
		}
		return invalid(param, "invalid JSON field type")
	}
	return nil
}
func readJSON(ctx context.Context, r io.Reader, out any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := io.ReadAll(io.LimitReader(r, maxBody+1))
	if err != nil {
		return err
	}
	if len(raw) > maxBody {
		return invalid("body", "body exceeds 16 MiB")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return strict(raw, out, "body")
}
func writeJSON(ctx context.Context, w io.Writer, v any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return json.NewEncoder(w).Encode(v)
}
func objectJSON(raw []byte, param string) error {
	if err := uniqueJSON(raw, param); err != nil {
		return err
	}
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return invalid(param, "expected JSON object")
	}
	return nil
}
func cleanBlock(b core.ContentBlock, kind string) error {
	if b.Kind != kind {
		return unsupported("content." + b.Kind)
	}
	if b.URL != "" || b.MIMEType != "" || len(b.Data) != 0 {
		return unsupported("content.metadata")
	}
	switch kind {
	case "text":
		if b.ID != "" || b.Name != "" || b.Arguments != "" {
			return unsupported("content.text.metadata")
		}
	case "tool_call":
		if b.Text != "" {
			return unsupported("tool_call.text")
		}
		if b.ID == "" || b.Name == "" {
			return invalid("tool_call", "id and name are required")
		}
		return objectJSON([]byte(b.Arguments), "tool_call.arguments")
	case "tool_result":
		if b.ID == "" {
			return invalid("tool_result.id", "call id is required")
		}
		if b.Name != "" || b.Arguments != "" {
			return unsupported("tool_result.metadata")
		}
	}
	return nil
}
func at(field string, i int) string { return fmt.Sprintf("%s[%d]", field, i) }
