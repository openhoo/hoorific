package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"hoorific/internal/core"
	"hoorific/internal/resource"
)

func nativePollURL(base, template, id string) (string, error) {
	if template == "" {
		return "", fmt.Errorf("poll endpoint is not declared")
	}
	escaped := urlPathEscape(id)
	var out strings.Builder
	seen := ""
	for pos := 0; pos < len(template); {
		open := strings.IndexByte(template[pos:], '{')
		if open < 0 {
			out.WriteString(template[pos:])
			break
		}
		open += pos
		out.WriteString(template[pos:open])
		close := strings.IndexByte(template[open+1:], '}')
		if close < 0 {
			return "", fmt.Errorf("poll endpoint placeholder is incomplete")
		}
		close += open + 1
		name := template[open+1 : close]
		if name == "" || strings.ContainsAny(name, "{} /?&#") {
			return "", fmt.Errorf("poll endpoint placeholder is invalid")
		}
		if seen != "" {
			return "", fmt.Errorf("poll endpoint requires exactly one resource parameter")
		}
		seen = name
		value := escaped
		switch {
		case name == "operation" && (strings.HasPrefix(id, "operations/") || strings.Contains(id, "/operations/")):
			value = qualifiedPathEscape(id)
			if strings.Contains(template[:open], "/operations/") && strings.HasPrefix(id, "operations/") {
				value = urlPathEscape(strings.TrimPrefix(id, "operations/"))
			}
		case strings.HasPrefix(id, "batches/") && name == "id" && strings.Contains(template[:open], "/batches/"):
			value = urlPathEscape(strings.TrimPrefix(id, "batches/"))
		}
		out.WriteString(value)
		pos = close + 1
	}
	path := out.String()
	if strings.Contains(path, "{") || strings.Contains(path, "}") {
		return "", fmt.Errorf("poll endpoint contains an unknown placeholder")
	}
	return joinEndpoint(base, path)
}
func qualifiedPathEscape(value string) string {
	parts := strings.Split(value, "/")
	for i := range parts {
		parts[i] = urlPathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}
func urlPathEscape(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(value, "%", "%25"), "/", "%2F"), "?", "%3F"), "#", "%23"), "\\", "%5C")
}
func nativeUsage(data []byte, path string) *core.Usage {
	if path == "" {
		return nil
	}
	v := nativeField(data, path)
	if len(v) == 0 {
		return nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(v, &obj) != nil {
		return nil
	}
	read := func(name string) *int64 {
		var n int64
		if json.Unmarshal(obj[name], &n) != nil {
			return nil
		}
		return &n
	}
	in, out, total := read("input"), read("output"), read("total")
	if in == nil {
		in = read("input_tokens")
	}
	if out == nil {
		out = read("output_tokens")
	}
	if total == nil && in != nil && out != nil {
		x := *in + *out
		total = &x
	}
	return &core.Usage{Input: in, Output: out, Total: total, Source: "upstream"}
}

func (g *Gateway) reconcileNativeJob(ctx context.Context, job resource.Job, s core.RuntimeSnapshot, owner string) (bool, error) {
	var meta nativeJobMetadata
	if json.Unmarshal(job.Metadata, &meta) != nil {
		return false, nil
	}
	c, ok := s.Connections[job.ConnectionID]
	if !ok || c.TenantID != job.TenantID || c.AccountID != job.AccountID || c.Settings["disabled"] == "true" {
		return false, nil
	}
	if job.LeaseOwner != owner || job.Fence <= 0 || !job.LeaseUntil.After(time.Now()) {
		return false, fmt.Errorf("native job lease expired")
	}
	if job.SettlementPending {
		return g.settleNativeJob(ctx, job, meta, owner)
	}
	if meta.Policy.PollEndpoint == "" || meta.Policy.PollAction == "" {
		return false, nil
	}
	target := core.ConnectionResourceCall{Connection: c, Action: meta.Policy.PollAction, ResourceID: job.ResourceID}
	connector, ok := g.deps.Connectors[c.Connector]
	if !ok {
		return false, nil
	}
	operation := meta.Policy.PollOperation
	if operation == "" {
		operation = meta.Binding.Codec.Operation
	}
	if operation == "" {
		operation = core.Operation(job.Operation)
	}
	pollBinding, err := connector.Bind(ctx, target, operation)
	if err != nil {
		return false, nil
	}
	endpoint, err := nativePollURL(c.BaseURL, meta.Policy.PollEndpoint, job.ResourceID)
	if err != nil {
		return false, nil
	}
	method := meta.Policy.PollMethod
	if method == "" {
		method = pollBinding.Method
	}
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return false, err
	}
	for name, values := range pollBinding.Headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	req.Header.Set("Accept", "application/json")
	transportSanitize(req)
	lease, err := g.deps.Credentials.Lease(ctx, c)
	if err != nil || lease == nil {
		closeCredentialLease(lease)
		return false, nil
	}
	defer closeCredentialLease(lease)
	if err = lease.Authorize(ctx, req); err != nil {
		return false, nil
	}
	client, err := g.deps.Client(c)
	if err != nil {
		return false, err
	}
	if client == nil {
		return false, fmt.Errorf("native reconciliation HTTP client unavailable")
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, nil
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, nativeAckLimit+1))
	if err != nil || len(data) > nativeAckLimit {
		return false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, nil
	}
	status := nativeStatus(data, meta.Policy.StatusField)
	if status == "" {
		return false, nil
	}
	job.Status = status
	job.Usage = nativeUsage(data, meta.Policy.UsageField)
	job.UpdatedAt = time.Now()
	job.SettlementPending = nativeTerminal(meta.Policy, status)
	if !job.SettlementPending {
		job.NextPoll = time.Now().Add(2 * time.Second)
		job.LeaseOwner = ""
		job.LeaseUntil = time.Time{}
		return false, g.deps.Resources.UpdateJob(ctx, job, owner, job.Fence)
	}
	// Persist terminal evidence while keeping SQL settlement claimable across a crash.
	if err = g.deps.Resources.UpdateJob(ctx, job, owner, job.Fence); err != nil {
		return false, err
	}
	return g.settleNativeJob(ctx, job, meta, owner)
}

func (g *Gateway) settleNativeJob(ctx context.Context, job resource.Job, meta nativeJobMetadata, owner string) (bool, error) {
	if !job.LeaseUntil.After(time.Now()) {
		return false, fmt.Errorf("native job lease expired")
	}
	state := "settled"
	cancellation := ""
	switch strings.ToLower(strings.TrimSpace(job.Status)) {
	case "canceled", "cancelled":
		cancellation = "confirmed"
	}
	outcome := core.AttemptOutcome{TenantID: job.TenantID, RequestID: job.RequestID, AttemptID: meta.AttemptID, State: state, Cancellation: cancellation, Usage: job.Usage}
	if err := g.deps.Admission.FinalizeAttempt(ctx, outcome); err != nil {
		return false, err
	}
	job.SettlementPending = false
	job.NextPoll = time.Time{}
	job.LeaseOwner = ""
	job.LeaseUntil = time.Time{}
	job.UpdatedAt = time.Now()
	return true, g.deps.Resources.UpdateJob(ctx, job, owner, job.Fence)
}
func transportSanitize(r *http.Request) {
	for _, name := range []string{"Authorization", "Proxy-Authorization", "Cookie", "Host", "Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Connection", "Keep-Alive", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		if name != "Host" {
			r.Header.Del(name)
		}
	}
}

// RunResourceReconciler claims provider jobs using SQL fencing and settles only
// declared terminal statuses. Creation is never retried by this loop.
func (g *Gateway) RunResourceReconciler(ctx context.Context) error {
	if g.deps.Resources == nil {
		return fmt.Errorf("resource store unavailable")
	}
	owner := requestID()
	delay := 250 * time.Millisecond
	ticker := time.NewTicker(delay)
	defer ticker.Stop()
	for {
		passCtx, cancelPass := context.WithTimeout(ctx, 10*time.Second)
		jobs, err := g.deps.Resources.ClaimJobs(passCtx, owner, time.Now(), 30*time.Second, 1)
		cancelPass()
		if err == nil {
			for _, job := range jobs {
				jobCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				scoped := core.WithPrincipal(jobCtx, core.Principal{TenantID: job.TenantID})
				s, e := g.deps.Snapshots.Snapshot(scoped)
				if e == nil {
					_, e = g.reconcileNativeJob(scoped, job, s, owner)
				}
				cancel()
				if e != nil {
					err = e
					break
				}
			}
		}
		if err != nil && ctx.Err() == nil {
			slog.Warn("native reconciliation pass deferred", "error_type", fmt.Sprintf("%T", err))
			delay = min(delay*2, 5*time.Second)
		} else {
			delay = 250 * time.Millisecond
		}
		ticker.Reset(delay)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
