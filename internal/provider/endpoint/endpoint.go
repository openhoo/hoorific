// Package endpoint implements explicit provider endpoint inventories. It never dispatches inference.
package endpoint

import (
	"context"
	"encoding/json"
	"fmt"
	"hoorific/internal/core"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type Route struct {
	core.NativeEndpoint
	Protocol               core.Protocol
	Variant                string
	Response               core.NativeResponsePolicy
	AllowedRequestHeaders  []string
	ValidateRequestHeaders func(http.Header) error
	DefaultBody            string
}
type Option func(*Connector)
type Connector struct {
	id, base, discoveryPath string
	routes                  []Route
	client                  *http.Client
	credentials             core.CredentialSource
}

func E(method, path, action string, op core.Operation, protocol core.Protocol, framing core.Framing, location string, stateful bool) Route {
	return Route{NativeEndpoint: core.NativeEndpoint{Method: method, Path: path, Action: action, Operation: op, ModelLocation: location, Framing: framing, Stateful: stateful}, Protocol: protocol}
}
func New(id, base string, routes []Route, opts ...Option) *Connector {
	c := &Connector{id: id, base: base, routes: append([]Route(nil), routes...)}
	for _, o := range opts {
		o(c)
	}
	return c
}
func WithDiscovery(client *http.Client, credentials core.CredentialSource) Option {
	return func(c *Connector) { c.client = client; c.credentials = credentials }
}
func WithDiscoveryPath(path string) Option                   { return func(c *Connector) { c.discoveryPath = path } }
func (c *Connector) DefaultBaseURL() string                  { return c.base }
func (c *Connector) DiscoveryClient() *http.Client           { return c.client }
func (c *Connector) CredentialSource() core.CredentialSource { return c.credentials }
func (c *Connector) DiscoverAt(ctx context.Context, conn core.Connection, path string) ([]core.Model, error) {
	clone := *c
	clone.discoveryPath = path
	return clone.Discover(ctx, conn)
}
func (c *Connector) Descriptor() core.ConnectorDescriptor {
	d := core.ConnectorDescriptor{ID: c.id}
	ops := map[core.Operation]bool{}
	protocols := map[core.Protocol]bool{}
	for _, r := range c.routes {
		if !ops[r.Operation] {
			d.Operations = append(d.Operations, r.Operation)
			ops[r.Operation] = true
		}
		if !protocols[r.Protocol] {
			d.Protocols = append(d.Protocols, r.Protocol)
			protocols[r.Protocol] = true
		}
	}
	return d
}
func (c *Connector) Endpoints() []core.NativeEndpoint {
	out := make([]core.NativeEndpoint, len(c.routes))
	for i, r := range c.routes {
		out[i] = r.NativeEndpoint
	}
	return out
}
func failure(code, message string) error {
	return core.GatewayError{Code: code, HTTPStatus: 400, Message: message, Origin: "gateway"}
}
func (c *Connector) Bind(ctx context.Context, target core.Target, op core.Operation) (core.Binding, error) {
	var conn core.Connection
	params := map[string]string{}
	action := ""
	switch t := target.(type) {
	case core.ModelCall:
		conn = t.Connection
		allowed := false
		for _, v := range t.Model.Operations {
			if v == op {
				allowed = true
			}
		}
		if !allowed {
			return core.Binding{}, failure("unsupported_operation", "Model manifest does not approve this operation")
		}
		params["model"] = t.Model.ID
	case core.ConnectionResourceCall:
		conn = t.Connection
		action = t.Action
		params["id"] = t.ResourceID
		params["resource_id"] = t.ResourceID
	default:
		return core.Binding{}, failure("unsupported_operation", "Unknown target")
	}
	for _, r := range c.routes {
		if r.Operation != op || (action != "" && r.Action != action) {
			continue
		}
		if action == "" && r.Stateful && r.ModelLocation == "none" {
			continue
		}
		return c.BindEndpoint(ctx, conn, r.NativeEndpoint, params)
	}
	return core.Binding{}, failure("unsupported_operation", "Endpoint action is not supported")
}

// Segment validates one opaque path component before escaping exactly once.
func Segment(s string) (string, error) {
	if s == "" || s == "." || s == ".." || strings.ContainsAny(s, "/\\%?#\x00\r\n") {
		return "", failure("unsupported_operation", "Invalid endpoint path component")
	}
	return url.PathEscape(s), nil
}
func (c *Connector) BindEndpoint(ctx context.Context, conn core.Connection, e core.NativeEndpoint, params map[string]string) (core.Binding, error) {
	if err := ctx.Err(); err != nil {
		return core.Binding{}, err
	}
	var route *Route
	for i := range c.routes {
		if c.routes[i].NativeEndpoint == e {
			route = &c.routes[i]
			break
		}
	}
	if route == nil {
		return core.Binding{}, failure("unsupported_operation", "Endpoint is not in connector inventory")
	}
	if e.ResourceIDField != "" && params[e.ResourceIDField] == "" {
		params[e.ResourceIDField] = params["resource_id"]
	}
	path := e.Path
	for strings.Contains(path, "{") {
		start := strings.IndexByte(path, '{')
		end := strings.IndexByte(path[start:], '}')
		if end < 0 {
			return core.Binding{}, failure("unsupported_operation", "Malformed endpoint descriptor")
		}
		end += start
		key := path[start+1 : end]
		v, err := Segment(params[key])
		if err != nil {
			return core.Binding{}, err
		}
		path = path[:start] + v + path[end+1:]
	}
	if err := ValidateRelative(path); err != nil {
		return core.Binding{}, err
	}
	return core.Binding{Codec: core.CodecKey{Protocol: route.Protocol, Variant: route.Variant, Operation: e.Operation}, Endpoint: path, Method: e.Method, ModelLocation: e.ModelLocation, Framing: e.Framing, ReplaySafe: e.Method == http.MethodGet || e.Method == http.MethodHead, CancellationSupported: e.Action == "cancel" || strings.HasSuffix(e.Action, ".cancel"), AllowedRequestHeaders: append([]string(nil), route.AllowedRequestHeaders...), ValidateRequestHeaders: route.ValidateRequestHeaders, DefaultBody: route.DefaultBody, Response: route.Response}, nil
}

type DiscoveryPage struct {
	Models        []core.Model
	NextPageToken string
	NextPage      string
	LastID        string
	HasMore       bool
}

func (c *Connector) Discover(ctx context.Context, conn core.Connection) ([]core.Model, error) {
	page, err := c.DiscoverPage(ctx, conn, c.discoveryPath)
	if err != nil {
		return nil, err
	}
	return page.Models, nil
}

// DiscoverPage performs one authenticated page request. Providers that expose
// cursors must follow the returned cursor with a descriptor-specific path.
func (c *Connector) DiscoverPage(ctx context.Context, conn core.Connection, path string) (DiscoveryPage, error) {
	if c.client == nil {
		return DiscoveryPage{}, failure("configuration_stale", "Model discovery requires an injected HTTP client")
	}
	if conn.BaseURL == "" {
		return DiscoveryPage{}, failure("configuration_stale", "Model discovery requires an explicit connection base URL")
	}
	if path == "" {
		return DiscoveryPage{}, failure("unsupported_operation", "Connector has no model discovery endpoint")
	}
	u, err := url.Parse(conn.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return DiscoveryPage{}, failure("configuration_stale", "Connector base URL is invalid")
	}
	p, err := url.Parse(path)
	if err != nil || p.IsAbs() || p.Host != "" || strings.HasPrefix(path, "/") {
		return DiscoveryPage{}, failure("configuration_stale", "Discovery path must be relative")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + p.Path
	u.RawQuery = p.RawQuery
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return DiscoveryPage{}, err
	}
	if c.credentials == nil {
		return DiscoveryPage{}, failure("connection_required", "Model discovery requires an injected credential source")
	}
	lease, err := c.credentials.Lease(ctx, conn)
	if err != nil {
		return DiscoveryPage{}, err
	}
	if lease == nil {
		return DiscoveryPage{}, failure("connection_required", "Credential lease is unavailable")
	}
	authErr := lease.Authorize(ctx, req)
	closeErr := core.CloseCredentialLease(lease)
	if authErr != nil {
		return DiscoveryPage{}, authErr
	}
	if closeErr != nil {
		return DiscoveryPage{}, closeErr
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return DiscoveryPage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return DiscoveryPage{}, core.GatewayError{Code: "upstream_outcome_unknown", HTTPStatus: resp.StatusCode, Message: "Model discovery failed", Origin: "upstream", Retryable: resp.StatusCode >= 500}
	}
	var raw struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
		Models []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"models"`
		Results []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"results"`
		NextPageToken string `json:"nextPageToken"`
		NextPage      string `json:"next_page"`
		LastID        string `json:"last_id"`
		HasMore       *bool  `json:"has_more"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&raw); err != nil {
		return DiscoveryPage{}, failure("upstream_outcome_unknown", "Model discovery returned invalid JSON")
	}
	items := raw.Data
	if len(items) == 0 {
		items = raw.Models
	}
	if len(items) == 0 {
		items = raw.Results
	}
	out := DiscoveryPage{NextPageToken: raw.NextPageToken, NextPage: raw.NextPage, LastID: raw.LastID}
	out.HasMore = raw.NextPageToken != "" || raw.NextPage != "" || raw.LastID != ""
	if raw.HasMore != nil {
		out.HasMore = *raw.HasMore
	}
	out.Models = make([]core.Model, 0, len(items))
	for _, item := range items {
		id := item.ID
		if id == "" {
			id = item.Name
		}
		if id != "" {
			out.Models = append(out.Models, core.Model{ID: id, ConnectionID: conn.ID, Features: map[string]core.Support{}, Provenance: "provider_discovery"})
		}
	}
	return out, nil
}

var _ core.Connector = (*Connector)(nil)
var _ core.Discoverer = (*Connector)(nil)
var _ core.EndpointInventory = (*Connector)(nil)
var _ core.EndpointBinder = (*Connector)(nil)
var _ core.ScopeInspector = (*Connector)(nil)

func ValidateRelative(path string) error {
	u, err := url.Parse(path)
	if err != nil || u.IsAbs() || u.Host != "" || u.Fragment != "" || strings.HasPrefix(path, "/") || strings.Contains(path, "\\") {
		return failure("unsupported_operation", "Endpoint must be a relative allowlisted path")
	}
	for _, part := range strings.Split(u.EscapedPath(), "/") {
		decoded, err := url.PathUnescape(part)
		if err != nil || decoded == "." || decoded == ".." || strings.ContainsAny(decoded, "/\\\x00") {
			return failure("unsupported_operation", "Unsafe endpoint path")
		}
	}
	return nil
}

// Inspect enforces the operator manifest without interpreting or rewriting native schemas.
func (c *Connector) Inspect(ctx context.Context, target core.Target, op core.Operation, body []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t, ok := target.(core.ModelCall)
	if !ok {
		return nil
	}
	approved := false
	for _, v := range t.Model.Operations {
		if v == op {
			approved = true
		}
	}
	if !approved {
		return failure("unsupported_operation", "Model operation is not approved")
	}
	if len(body) == 0 || body[0] != '{' {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return failure("unsupported_feature", "Invalid JSON request")
	}
	for field, feature := range map[string]string{"tools": "custom_tools", "parallel_tool_calls": "parallel_tools", "response_format": "structured_output", "stream": "streaming"} {
		raw, exists := fields[field]
		if !exists || string(raw) == "false" || string(raw) == "null" || string(raw) == "[]" {
			continue
		}
		if t.Model.Features[feature] != core.Supported {
			return core.GatewayError{Code: "unsupported_feature", HTTPStatus: 400, Message: fmt.Sprintf("Model manifest does not approve %s", feature), Param: field, Origin: "gateway"}
		}
	}
	return nil
}
