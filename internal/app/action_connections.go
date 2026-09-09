package app

import (
	"context"
	"encoding/json"
	"fmt"
	"hoorific/internal/core"
	"hoorific/internal/transport"
	"io"
	"net/http"
	"net/url"
	"strings"
)

func (a *Actions) connectionAction(ctx context.Context, p core.Principal, id, action string, version int64, _ json.RawMessage) (json.RawMessage, error) {
	c, r, e := a.connection(ctx, p, id)
	if e != nil {
		return nil, e
	}
	switch action {
	case "disable":
		if version <= 0 || version != r.Version {
			return nil, actionError("version_conflict", 412, "Connection changed; reload before disabling")
		}
		var raw map[string]any
		if json.Unmarshal(r.Data, &raw) != nil {
			return nil, actionError("configuration_stale", 503, "Stored connection data is invalid")
		}
		raw["enabled"] = false
		b, e := json.Marshal(raw)
		if e != nil {
			return nil, e
		}
		out, e := a.deps.Store.Mutate(ctx, p, core.Mutation{Kind: "connections", ID: id, ExpectedVersion: version, Data: b})
		if e != nil {
			return nil, e
		}
		return json.Marshal(out)
	case "discover":
		conn, e := a.connector(c)
		if e != nil {
			return nil, e
		}
		d, ok := conn.(core.Discoverer)
		if !ok {
			return nil, actionError("unsupported_operation", 404, "Connector does not support model discovery")
		}
		models, e := d.Discover(ctx, c)
		if e != nil {
			return nil, e
		}
		// Discovery is advisory: candidates are returned to the operator and are
		// never inserted into the approved catalog by an action.
		type candidate struct {
			ID           string                  `json:"id"`
			UpstreamID   string                  `json:"upstream_id"`
			ConnectionID string                  `json:"connection_id"`
			Provenance   string                  `json:"provenance"`
			Operations   []core.Operation        `json:"operations,omitempty"`
			Features     map[string]core.Support `json:"features,omitempty"`
		}
		out := make([]candidate, 0, len(models))
		for _, m := range models {
			if m.ID == "" || m.ConnectionID != c.ID {
				continue
			}
			if m.Provenance == "" {
				m.Provenance = "provider_discovery"
			}
			catalogID := m.CatalogID
			if catalogID == "" {
				catalogID = m.ID
			}
			out = append(out, candidate{catalogID, m.ID, m.ConnectionID, m.Provenance, m.Operations, m.Features})
		}
		b, e := json.Marshal(struct {
			ConnectionID string `json:"connection_id"`
			Candidates   any    `json:"candidates"`
			Approved     bool   `json:"approved"`
		}{c.ID, out, false})
		if e != nil {
			return nil, e
		}
		if len(b) > operationPayloadLimit {
			return nil, actionError("payload_too_large", 413, "Discovery result exceeds limit")
		}
		return b, nil
	case "test":
		conn, e := a.connector(c)
		if e != nil {
			return nil, e
		}
		inv, ok := conn.(core.EndpointInventory)
		if !ok {
			return nil, actionError("unsupported_operation", 404, "Connector has no test endpoint inventory")
		}
		binder, ok := conn.(core.EndpointBinder)
		if !ok {
			return nil, actionError("unsupported_operation", 404, "Connector cannot bind its test endpoint")
		}
		var ep core.NativeEndpoint
		found := false
		for _, x := range inv.Endpoints() {
			if x.Operation == core.Operation("model.list") && strings.EqualFold(x.Method, http.MethodGet) {
				ep = x
				found = true
				break
			}
		}
		if !found {
			return nil, actionError("unsupported_operation", 404, "Connector has no model-list test action")
		}
		bind, e := binder.BindEndpoint(ctx, c, ep, map[string]string{})
		if e != nil {
			return nil, e
		}
		if a.deps.Client == nil {
			return nil, actionError("configuration_stale", 503, "Connection HTTP client unavailable")
		}
		client, e := a.deps.Client(c)
		if e != nil {
			return nil, e
		}
		if client == nil {
			return nil, actionError("configuration_stale", 503, "Connection HTTP client unavailable")
		}
		target, e := joinConnectionURL(c.BaseURL, bind.Endpoint)
		if e != nil {
			return nil, e
		}
		req, e := http.NewRequestWithContext(ctx, bind.Method, target, nil)
		if e != nil {
			return nil, e
		}
		req.Header.Del("Authorization")
		req.Header.Del("x-api-key")
		req.Header.Del("x-goog-api-key")
		for k, values := range bind.Headers {
			req.Header[k] = append([]string(nil), values...)
		}
		if c.ClientProfile != nil {
			if e = c.ClientProfile.Apply(req.Header); e != nil {
				return nil, e
			}
		}
		transport.SanitizeRequest(req)
		if a.deps.Credentials == nil {
			return nil, actionError("connection_required", 400, "Connection credential source unavailable")
		}
		lease, e := a.deps.Credentials.Lease(ctx, c)
		if e != nil {
			return nil, e
		}
		if lease == nil {
			return nil, actionError("connection_required", 400, "Connection credential lease unavailable")
		}
		authErr := lease.Authorize(ctx, req)
		closeErr := core.CloseCredentialLease(lease)
		if authErr != nil {
			return nil, authErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		resp, e := client.Do(req)
		if e != nil {
			return nil, e
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, core.GatewayError{Code: "upstream_outcome_unknown", HTTPStatus: resp.StatusCode, Message: fmt.Sprintf("Connection test returned HTTP %d", resp.StatusCode), Origin: "upstream", Retryable: resp.StatusCode >= 500}
		}
		return json.Marshal(struct {
			Status            string `json:"status"`
			StatusCode        int    `json:"status_code"`
			ProviderRequestID string `json:"provider_request_id,omitempty"`
		}{"ok", resp.StatusCode, resp.Header.Get("x-request-id")})
	default:
		return nil, actionError("unsupported_operation", 404, "Connection action is not supported")
	}
}

func joinConnectionURL(base, path string) (string, error) {
	if base == "" || path == "" {
		return "", actionError("configuration_stale", 503, "Connection endpoint is not configured")
	}
	u, e := url.Parse(base)
	if e != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") || u.RawQuery != "" || u.Fragment != "" {
		return "", actionError("configuration_stale", 503, "Connection endpoint is invalid")
	}
	if strings.Contains(path, "://") || strings.HasPrefix(path, "//") || strings.Contains(path, "#") {
		return "", actionError("unsupported_operation", 400, "Endpoint path is not relative")
	}
	v, e := url.Parse("./" + strings.TrimPrefix(path, "/"))
	if e != nil || v.Host != "" {
		return "", actionError("unsupported_operation", 400, "Endpoint path is not relative")
	}
	decoded := strings.TrimPrefix(v.Path, "./")
	for _, part := range strings.Split(decoded, "/") {
		if part == ".." || part == "." {
			return "", actionError("unsupported_operation", 400, "Endpoint path is not relative")
		}
	}
	escaped := strings.TrimSuffix(u.EscapedPath(), "/") + "/" + strings.TrimPrefix(strings.TrimPrefix(v.EscapedPath(), "./"), "/")
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + strings.TrimPrefix(decoded, "/")
	u.RawPath = escaped
	u.RawQuery = v.RawQuery
	return u.String(), nil
}
