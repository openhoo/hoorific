package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"hoorific/internal/core"
	"io"
	"net/http"
	"strings"
)

// Request/Result retain Antigravity's nested native payload while exposing the
// same independently authenticated wrapper shape as Code Assist.
type Request struct {
	Model        string          `json:"model"`
	Project      string          `json:"project,omitempty"`
	UserPromptID string          `json:"user_prompt_id,omitempty"`
	Request      json.RawMessage `json:"request"`
}
type Result struct {
	Response       json.RawMessage `json:"response"`
	TraceID        string          `json:"traceId,omitempty"`
	CreditMetadata json.RawMessage `json:"creditMetadata,omitempty"`
}

func Wrap(model, project, userPromptID string, request []byte) ([]byte, error) {
	if model == "" || len(bytes.TrimSpace(request)) == 0 || !json.Valid(request) {
		return nil, errors.New("Antigravity requires model and valid request")
	}
	return json.Marshal(Request{Model: model, Project: project, UserPromptID: userPromptID, Request: append(json.RawMessage(nil), request...)})
}
func Unwrap(body []byte) (Result, error) {
	var r Result
	if err := json.Unmarshal(body, &r); err != nil {
		return Result{}, err
	}
	if len(r.Response) == 0 || bytes.Equal(bytes.TrimSpace(r.Response), []byte("null")) {
		return Result{}, errors.New("Antigravity response has no response envelope")
	}
	return r, nil
}
func (c *Connector) AdaptRequest(ctx context.Context, target core.Target, binding core.Binding, body []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if binding.Codec.Variant != "antigravity" {
		return body, nil
	}
	call, ok := target.(core.ModelCall)
	if !ok {
		return body, nil
	}
	return Wrap(call.Model.ID, call.Connection.Project, "", body)
}
func (c *Connector) AdaptResponse(ctx context.Context, binding core.Binding, resp *http.Response) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if resp == nil || resp.Body == nil {
		return errors.New("Antigravity response is missing")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if len(raw) > 1<<20 {
		return errors.New("Antigravity response exceeds adapter limit")
	}
	if binding.Framing == core.Framing("sse-data") {
		raw, err = unwrapSSE(raw)
		if err != nil {
			return err
		}
	} else {
		r, e := Unwrap(raw)
		if e != nil {
			return e
		}
		raw = append([]byte(nil), r.Response...)
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	resp.ContentLength = int64(len(raw))
	return nil
}
func unwrapSSE(raw []byte) ([]byte, error) {
	var out bytes.Buffer
	for _, line := range strings.Split(string(raw), "\n") {
		trim := strings.TrimSuffix(line, "\r")
		if !strings.HasPrefix(trim, "data:") {
			out.WriteString(line)
			out.WriteByte('\n')
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(trim, "data:"))
		if data == "" || data == "[DONE]" {
			out.WriteString(line)
			out.WriteByte('\n')
			continue
		}
		r, e := Unwrap([]byte(data))
		if e != nil {
			return nil, e
		}
		out.WriteString("data: ")
		out.Write(r.Response)
		out.WriteString("\n")
	}
	return out.Bytes(), nil
}

var _ core.WireAdapter = (*Connector)(nil)
