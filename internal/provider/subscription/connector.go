// Package subscription contains the shared, explicit mechanics for opt-in
// subscription connectors. It does not infer provider wire compatibility.
package subscription

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"hoorific/internal/core"
)

type Route struct {
	Endpoint   core.NativeEndpoint
	Protocol   core.Protocol
	Variant    string
	Headers    http.Header
	HeadersFor func(core.Connection) http.Header
}
type Connector struct {
	id     string
	routes []Route
}

func New(id string, routes []Route) *Connector {
	return &Connector{id: id, routes: append([]Route(nil), routes...)}
}
func (c *Connector) Descriptor() core.ConnectorDescriptor {
	d := core.ConnectorDescriptor{ID: c.id, Subscription: true}
	seenP := map[core.Protocol]bool{}
	seenO := map[core.Operation]bool{}
	for _, r := range c.routes {
		if !seenP[r.Protocol] {
			d.Protocols = append(d.Protocols, r.Protocol)
			seenP[r.Protocol] = true
		}
		if !seenO[r.Endpoint.Operation] {
			d.Operations = append(d.Operations, r.Endpoint.Operation)
			seenO[r.Endpoint.Operation] = true
		}
	}
	return d
}
func (c *Connector) Endpoints() []core.NativeEndpoint {
	out := make([]core.NativeEndpoint, len(c.routes))
	for i, r := range c.routes {
		out[i] = r.Endpoint
	}
	return out
}
func (c *Connector) Bind(ctx context.Context, target core.Target, op core.Operation) (core.Binding, error) {
	if err := ctx.Err(); err != nil {
		return core.Binding{}, err
	}
	switch call := target.(type) {
	case core.ModelCall:
		if err := enabled(call.Connection); err != nil {
			return core.Binding{}, err
		}
		for _, r := range c.routes {
			if r.Endpoint.Operation != op {
				continue
			}
			if len(call.Model.Operations) > 0 && !contains(call.Model.Operations, op) {
				return core.Binding{}, failure("model manifest does not approve this operation")
			}
			return c.BindEndpoint(ctx, call.Connection, r.Endpoint, map[string]string{"model": call.Model.ID})
		}
	case core.ConnectionResourceCall:
		if err := enabled(call.Connection); err != nil {
			return core.Binding{}, err
		}
		for _, r := range c.routes {
			if r.Endpoint.Operation != op || r.Endpoint.Action != call.Action {
				continue
			}
			return c.BindEndpoint(ctx, call.Connection, r.Endpoint, map[string]string{"id": call.ResourceID, "resource_id": call.ResourceID})
		}
	default:
		return core.Binding{}, failure("subscription operation requires a pinned connection")
	}
	return core.Binding{}, failure("subscription operation is not source-verified or enabled")
}

// BindStream selects only an inventory route matching the requested response
// mode. Explicit resource actions remain authoritative and must agree with it.
func (c *Connector) BindStream(ctx context.Context, target core.Target, op core.Operation, stream bool) (core.Binding, error) {
	if err := c.Inspect(ctx, target, op, nil); err != nil {
		return core.Binding{}, err
	}
	switch call := target.(type) {
	case core.ModelCall:
		for _, r := range c.routes {
			if r.Endpoint.Operation != op || (r.Endpoint.Framing == "sse-data" || r.Endpoint.Framing == "sse-named") != stream {
				continue
			}
			return c.BindEndpoint(ctx, call.Connection, r.Endpoint, map[string]string{"model": call.Model.ID})
		}
	case core.ConnectionResourceCall:
		for _, r := range c.routes {
			if r.Endpoint.Operation != op || r.Endpoint.Action != call.Action || (r.Endpoint.Framing == "sse-data" || r.Endpoint.Framing == "sse-named") != stream {
				continue
			}
			return c.BindEndpoint(ctx, call.Connection, r.Endpoint, map[string]string{"id": call.ResourceID, "resource_id": call.ResourceID})
		}
	}
	return core.Binding{}, core.GatewayError{Code: "unsupported_operation", HTTPStatus: 400, Message: "subscription inventory does not support the requested response mode", Origin: "gateway"}
}
func (c *Connector) BindEndpoint(ctx context.Context, conn core.Connection, e core.NativeEndpoint, vars map[string]string) (core.Binding, error) {
	if err := ctx.Err(); err != nil {
		return core.Binding{}, err
	}
	if err := enabled(conn); err != nil {
		return core.Binding{}, err
	}
	var route *Route
	for i := range c.routes {
		if c.routes[i].Endpoint == e {
			route = &c.routes[i]
			break
		}
	}
	if route == nil {
		return core.Binding{}, failure("endpoint is not in subscription inventory")
	}
	p := e.Path
	for strings.Contains(p, "{") {
		a := strings.IndexByte(p, '{')
		z := strings.IndexByte(p[a:], '}')
		if z < 0 {
			return core.Binding{}, failure("malformed subscription endpoint")
		}
		z += a
		k := p[a+1 : z]
		v := vars[k]
		if v == "" {
			return core.Binding{}, failure("missing subscription path variable " + k)
		}
		if strings.ContainsAny(v, "/\\?#%") || v == "." || v == ".." {
			return core.Binding{}, failure("invalid subscription path variable " + k)
		}
		p = p[:a] + url.PathEscape(v) + p[z+1:]
	}
	if strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") {
		return core.Binding{}, failure("subscription endpoint must be relative")
	}
	headers := cloneHeaders(route.Headers)
	if route.HeadersFor != nil {
		for key, values := range route.HeadersFor(conn) {
			if headers == nil {
				headers = make(http.Header)
			}
			headers[key] = append([]string(nil), values...)
		}
	}
	// Closing the HTTP transport does not prove upstream execution was cancelled.
	return core.Binding{Codec: core.CodecKey{Protocol: route.Protocol, Variant: route.Variant, Operation: e.Operation}, Endpoint: p, Method: e.Method, ModelLocation: e.ModelLocation, Framing: e.Framing, ReplaySafe: e.Method == http.MethodGet, CancellationSupported: false, Headers: headers}, nil
}
func (c *Connector) Discover(context.Context, core.Connection) ([]core.Model, error) {
	return nil, failure("subscription discovery requires an authenticated account executor")
}
func (c *Connector) Inspect(ctx context.Context, target core.Target, op core.Operation, body []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch t := target.(type) {
	case core.ModelCall:
		if err := enabled(t.Connection); err != nil {
			return err
		}
		if len(t.Model.Operations) > 0 && !contains(t.Model.Operations, op) {
			return failure("model manifest does not approve this operation")
		}
	case core.ConnectionResourceCall:
		if err := enabled(t.Connection); err != nil {
			return err
		}
	default:
		return failure("subscription operation requires a pinned connection")
	}
	_ = body
	return nil
}
func enabled(conn core.Connection) error {
	if conn.BaseURL == "" {
		return failure("subscription connection requires an explicit configured base URL")
	}
	if conn.AccountID == "" {
		return failure("subscription connection requires an authenticated account identity")
	}
	if conn.Settings["subscription_enabled"] != "true" || conn.Settings["consent_ack"] != "true" {
		return failure("subscription connector requires explicit owner consent and opt-in")
	}
	if auth := conn.Settings["subscription_auth"]; auth != "oauth" && auth != "device" {
		return failure("subscription connector requires an explicit OAuth or device authorization")
	}
	if v := conn.Settings["consent_tenant"]; v != "" && v != conn.TenantID {
		return failure("subscription consent tenant does not match connection")
	}
	if v := conn.Settings["consent_account"]; v != "" && v != conn.AccountID {
		return failure("subscription consent account does not match connection")
	}
	if v := conn.Settings["consent_connector"]; v != "" && v != conn.Connector {
		return failure("subscription consent connector does not match connection")
	}
	return nil
}
func cloneHeaders(headers http.Header) http.Header {
	if headers == nil {
		return nil
	}
	return headers.Clone()
}
func contains(xs []core.Operation, x core.Operation) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
func failure(msg string) error {
	return core.GatewayError{Code: "connection_required", HTTPStatus: 400, Message: fmt.Sprintf("%s", msg), Origin: "gateway"}
}

var _ core.Connector = (*Connector)(nil)
var _ core.StreamBinder = (*Connector)(nil)
var _ core.EndpointInventory = (*Connector)(nil)
var _ core.EndpointBinder = (*Connector)(nil)
var _ core.ScopeInspector = (*Connector)(nil)
