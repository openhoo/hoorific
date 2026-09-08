package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"hoorific/internal/core"
	"hoorific/internal/resource"
	"hoorific/internal/transport"
)

const nativeAckLimit = 1 << 20

type NativeResponseState struct {
	JobPending bool
	ResourceID string
}
type nativeJobMetadata struct {
	AttemptID string
	Policy    core.NativeResponsePolicy
	Binding   core.Binding
}
type nativeReplayBody struct {
	io.Reader
	io.Closer
}

func nativeField(data []byte, path string) json.RawMessage {
	value := json.RawMessage(data)
	for _, part := range strings.Split(path, ".") {
		if part == "" {
			return nil
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(value, &object) != nil {
			return nil
		}
		value = object[part]
	}
	return value
}
func nativeString(data []byte, path string) string {
	var value string
	_ = json.Unmarshal(nativeField(data, path), &value)
	return value
}
func normalizeNativeStatus(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "canceled" {
		return "cancelled"
	}
	return value
}
func nativeStatus(data []byte, path string) string {
	value := nativeField(data, path)
	var text string
	if json.Unmarshal(value, &text) == nil {
		return normalizeNativeStatus(text)
	}
	if bytes.Equal(value, []byte("true")) {
		return "true"
	}
	if bytes.Equal(value, []byte("false")) {
		return "false"
	}
	return ""
}
func nativeTerminal(policy core.NativeResponsePolicy, status string) bool {
	status = normalizeNativeStatus(status)
	for _, candidate := range policy.TerminalStatuses {
		if normalizeNativeStatus(candidate) == status {
			return true
		}
	}
	for _, candidate := range policy.FailureStatuses {
		if normalizeNativeStatus(candidate) == status {
			return true
		}
	}
	return false
}

func nativeCreatesJob(t selected, b core.Binding) bool {
	if !b.Response.Async || b.Method == http.MethodGet || b.Method == http.MethodHead {
		return false
	}
	action := ""
	if t.endpoint != nil {
		action = t.endpoint.Action
	} else if target, ok := t.target.(core.ConnectionResourceCall); ok {
		action = target.Action
	}
	return action != b.Response.PollAction &&
		!strings.HasSuffix(action, ".cancel") && !strings.HasSuffix(action, ".delete")
}

// prepareNativeResponse persists upstream identity before any acknowledgement is exposed.
func (g *Gateway) prepareNativeResponse(ctx context.Context, request *http.Request, response *http.Response, p core.Principal, t selected, b core.Binding, permit core.AttemptPermit, id string) (NativeResponseState, error) {
	var state NativeResponseState
	policy := b.Response
	policy.Async = nativeCreatesJob(t, b)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return state, nil
	}
	if !policy.Async && len(policy.ContinuationHeaders) == 0 && len(policy.ContinuationFields) == 0 {
		return state, nil
	}
	if g.deps.Resources == nil {
		return state, failure("unavailable", 503, "native resource persistence unavailable")
	}
	durable, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := g.rewriteNativeHeaders(durable, response, p, t, b); err != nil {
		return state, err
	}
	original := response.Body
	var data []byte
	if strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") && policy.IDField != "" {
		guard := transport.NewIdleReader(ctx, original, 120*time.Second)
		reader := bufio.NewReaderSize(guard, 32<<10)
		var prefix bytes.Buffer
		var event bytes.Buffer
		for prefix.Len() < nativeAckLimit {
			line, err := reader.ReadSlice('\n')
			prefix.Write(line)
			if prefix.Len() > nativeAckLimit {
				return state, failure("upstream_outcome_unknown", 502, "native acknowledgement exceeds inspection limit")
			}
			trimmed := bytes.TrimRight(line, "\r\n")
			if bytes.HasPrefix(trimmed, []byte("data:")) {
				v := bytes.TrimPrefix(trimmed, []byte("data:"))
				v = bytes.TrimPrefix(v, []byte(" "))
				event.Write(v)
				event.WriteByte('\n')
			}
			if len(trimmed) == 0 {
				candidate := bytes.TrimSpace(event.Bytes())
				if nativeString(candidate, policy.IDField) != "" {
					data = append([]byte(nil), candidate...)
					break
				}
				event.Reset()
			}
			if err != nil {
				return state, failure("upstream_outcome_unknown", 502, "native stream did not acknowledge resource identity")
			}
		}
		if data == nil {
			return state, failure("upstream_outcome_unknown", 502, "native stream did not acknowledge resource identity")
		}
		if len(policy.ContinuationFields) > 0 {
			return state, failure("unsupported_operation", 502, "stream control URL rewriting is not declared as a supported framing")
		}
		response.Body = &nativeReplayBody{Reader: io.MultiReader(bytes.NewReader(prefix.Bytes()), reader), Closer: guard}
	} else if policy.IDField != "" || len(policy.ContinuationFields) > 0 {
		var err error
		data, err = transport.ReadBounded(ctx, original, nativeAckLimit, 120*time.Second)
		if err != nil || !json.Valid(data) {
			return state, failure("upstream_outcome_unknown", 502, "native acknowledgement could not be inspected")
		}
		updated, changed, err := g.rewriteNativeFields(durable, data, p, t, b)
		if err != nil {
			return state, err
		}
		if changed {
			data = updated
			response.ContentLength = int64(len(data))
			response.Header.Set("Content-Length", fmt.Sprint(len(data)))
		}
		response.Body = &nativeReplayBody{Reader: bytes.NewReader(data), Closer: original}
	}
	if policy.IDField == "" {
		return state, nil
	}
	resourceID := nativeString(data, policy.IDField)
	if resourceID == "" {
		if policy.Async {
			return state, failure("upstream_outcome_unknown", 502, "native acknowledgement omitted resource identity")
		}
		return state, nil
	}
	state.ResourceID = resourceID
	if !policy.Async {
		return state, nil
	}
	status := nativeStatus(data, policy.StatusField)
	metadata, _ := json.Marshal(nativeJobMetadata{AttemptID: permit.AttemptID, Policy: policy, Binding: b})
	now := time.Now()
	operation := b.Codec.Operation
	if t.endpoint != nil {
		operation = t.endpoint.Operation
	}
	if status == "" {
		status = "job_pending"
	}
	job := resource.Job{TenantID: p.TenantID, ConnectionID: t.connection.ID, AccountID: t.connection.AccountID, ResourceID: resourceID, Operation: string(operation), Status: status, RequestID: id, CreatedAt: now, UpdatedAt: now, Metadata: metadata, NextPoll: now.Add(2 * time.Second)}
	job.SettlementPending = nativeTerminal(policy, status)
	if job.SettlementPending {
		job.Usage = nativeUsage(data, policy.UsageField)
	}
	if err := g.deps.Resources.PutJob(durable, job); err != nil {
		return NativeResponseState{}, failure("upstream_outcome_unknown", 503, "native acknowledgement persistence failed")
	}
	state.JobPending = true
	if err := g.deps.Admission.FinalizeAttempt(durable, core.AttemptOutcome{TenantID: permit.TenantID, RequestID: permit.RequestID, AttemptID: permit.AttemptID, State: "job_pending"}); err != nil {
		return state, failure("upstream_outcome_unknown", 503, "native ownership transfer failed")
	}
	return state, nil
}

func (g *Gateway) recordUnknownNative(ctx context.Context, p core.Principal, t selected, b core.Binding, permit core.AttemptPermit, id string) {
	if !nativeCreatesJob(t, b) || g.deps.Resources == nil {
		return
	}
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	job := resource.UnknownCreate(p.TenantID, t.connection.ID, t.connection.AccountID, string(b.Codec.Operation), id, time.Now())
	job.Metadata, _ = json.Marshal(nativeJobMetadata{AttemptID: permit.AttemptID, Policy: b.Response, Binding: b})
	_ = g.deps.Resources.PutJob(c, job)
}
