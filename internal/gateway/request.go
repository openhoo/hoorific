package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"hoorific/internal/core"
	"hoorific/internal/routing"
)

type route struct {
	protocol                core.Protocol
	operation               core.Operation
	connection, path, model string
	native                  bool
	requirements            routing.Requirements
}

func identify(r *http.Request) (route, error) {
	p := r.URL.EscapedPath()
	lower := strings.ToLower(p)
	if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") || strings.Contains(p, "\\") {
		return route{}, failure("unsupported_operation", 404, "encoded separators are forbidden")
	}
	decoded, err := url.PathUnescape(p)
	if err != nil {
		return route{}, failure("unsupported_operation", 404, "invalid path")
	}
	for _, part := range strings.Split(decoded, "/") {
		if part == ".." || part == "." {
			return route{}, failure("unsupported_operation", 404, "path traversal is forbidden")
		}
	}
	x := route{path: decoded}
	if decoded == "/gateway/v1/realtime/tickets" && r.Method == http.MethodPost {
		x.operation = "realtime.ticket"
		x.protocol = "native"
		return x, nil
	}
	if strings.HasPrefix(decoded, "/connect/") {
		parts := strings.SplitN(strings.TrimPrefix(decoded, "/connect/"), "/", 3)
		if len(parts) != 3 || parts[0] == "" {
			return x, failure("unsupported_operation", 404, "invalid connection SDK path")
		}
		x.connection = parts[0]
		x.native = true
		x.path = parts[2]
		x.model = r.URL.Query().Get("model")
		switch parts[1] {
		case "openai":
			x.protocol = "openai-chat"
			x.path = strings.TrimPrefix(x.path, "v1/")
		case "anthropic":
			x.protocol = "anthropic-messages"
			x.path = strings.TrimPrefix(x.path, "v1/")
		case "gemini":
			x.protocol = "gemini-content"
		case "native":
			x.protocol = "native"
		case "continuations":
			x.protocol = "native"
			x.path = "continuations/" + parts[2]
		default:
			return x, failure("unsupported_operation", 404, "unknown SDK base")
		}
		return x, nil
	}
	if (r.Method == http.MethodGet || r.Method == http.MethodOptions) && decoded == "/v1/models" {
		x.operation = "model.list"
		x.protocol = "openai-chat"
		if r.Header.Get("anthropic-version") != "" {
			x.protocol = "anthropic-messages"
		}
		return x, nil
	}
	if (r.Method == http.MethodGet || r.Method == http.MethodOptions) && decoded == "/v1beta/models" {
		x.operation = "model.list"
		x.protocol = "gemini-content"
		return x, nil
	}
	if r.Method != http.MethodPost && r.Method != http.MethodOptions {
		return x, failure("unsupported_operation", 404, "unknown operation")
	}
	switch decoded {
	case "/v1/chat/completions":
		x.protocol = "openai-chat"
		x.operation = "generate"
	case "/v1/completions":
		x.protocol = "openai-completion"
		x.operation = "complete"
	case "/v1/responses":
		x.protocol = "openai-responses"
		x.operation = "generate"
	case "/v1/messages":
		x.protocol = "anthropic-messages"
		x.operation = "generate"
	case "/v1/messages/count_tokens":
		x.protocol = "anthropic-messages"
		x.operation = "count_tokens"
	case "/v1/embeddings":
		x.protocol = "openai-chat"
		x.operation = "embed"
	case "/v1/rerank":
		x.protocol = "cohere-v2"
		x.operation = "rerank"
	case "/v1/images/generations":
		x.protocol = "openai-chat"
		x.operation = "image.generate"
	case "/v1/images/edits":
		x.protocol = "openai-chat"
		x.operation = "image.edit"
	case "/v1/images/variations":
		x.protocol = "openai-chat"
		x.operation = "image.variation"
	case "/v1/audio/speech":
		x.protocol = "openai-chat"
		x.operation = "audio.speech"
	case "/v1/audio/transcriptions":
		x.protocol = "openai-chat"
		x.operation = "audio.transcribe"
	case "/v1/audio/translations":
		x.protocol = "openai-chat"
		x.operation = "audio.translate"
	default:
		if strings.HasPrefix(decoded, "/v1beta/models/") {
			model, action, ok := strings.Cut(strings.TrimPrefix(decoded, "/v1beta/models/"), ":")
			if !ok || model == "" {
				return x, failure("unsupported_operation", 404, "unknown model action")
			}
			x.model = model
			x.protocol = "gemini-content"
			switch action {
			case "generateContent", "streamGenerateContent":
				x.operation = "generate"
			case "embedContent":
				x.operation = "embed"
			case "countTokens":
				x.operation = "count_tokens"
			default:
				return x, failure("unsupported_operation", 404, "unknown model action")
			}
		} else {
			return x, failure("unsupported_operation", 404, "unknown operation")
		}
	}
	return x, nil
}
func strictObject(raw []byte) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := uniqueJSON(d); err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, fmt.Errorf("multiple JSON values")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, fmt.Errorf("expected JSON object")
	}
	return m, nil
}
func uniqueJSON(d *json.Decoder) error {
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
		seen := map[string]bool{}
		for d.More() {
			k, e := d.Token()
			if e != nil {
				return e
			}
			s, ok := k.(string)
			if !ok {
				return fmt.Errorf("invalid object key")
			}
			if seen[s] {
				return fmt.Errorf("duplicate field %s", s)
			}
			seen[s] = true
			if e = uniqueJSON(d); e != nil {
				return e
			}
		}
	case '[':
		for d.More() {
			if err := uniqueJSON(d); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected delimiter")
	}
	_, err = d.Token()
	return err
}
func portableScope(x route, m map[string]json.RawMessage) error {
	if x.protocol == "openai-responses" {
		b, ok := m["store"]
		if !ok || !bytes.Equal(bytes.TrimSpace(b), []byte("false")) {
			return failure("connection_required", 400, "root Responses requires explicit store:false; use a connection SDK base")
		}
	}
	for _, key := range []string{"previous_response_id", "conversation", "file_id", "batch_id", "background"} {
		if value, ok := m[key]; ok && string(value) != "false" && string(value) != "null" {
			e := failure("connection_required", 400, "provider state requires a connection SDK base")
			e.Param = key
			return e
		}
	}
	if b, ok := m["store"]; ok && !bytes.Equal(bytes.TrimSpace(b), []byte("false")) {
		return failure("connection_required", 400, "stored state requires a connection SDK base")
	}
	// The protocol codec owns all other semantic validation, even on native-wire relay.
	return nil
}
func matchEndpoint(pattern, path string) (map[string]string, bool) {
	pattern, _, _ = strings.Cut(pattern, "?")
	a, b := strings.Split(strings.Trim(pattern, "/"), "/"), strings.Split(strings.Trim(path, "/"), "/")
	if len(a) != len(b) {
		return nil, false
	}
	params := map[string]string{}
	for i, p := range a {
		start := strings.IndexByte(p, '{')
		if start < 0 {
			if p != b[i] {
				return nil, false
			}
			continue
		}
		end := strings.IndexByte(p, '}')
		if end <= start {
			return nil, false
		}
		prefix, suffix := p[:start], p[end+1:]
		if !strings.HasPrefix(b[i], prefix) || !strings.HasSuffix(b[i], suffix) || len(b[i]) <= len(prefix)+len(suffix) {
			return nil, false
		}
		value := b[i][len(prefix) : len(b[i])-len(suffix)]
		name := p[start+1 : end]
		if old, exists := params[name]; exists && old != value {
			return nil, false
		}
		params[name] = value
	}
	return params, true
}
func contains[T comparable](xs []T, x T) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
func failure(code string, status int, message string) core.GatewayError {
	return core.GatewayError{Code: code, HTTPStatus: status, Message: message, Origin: "gateway"}
}
