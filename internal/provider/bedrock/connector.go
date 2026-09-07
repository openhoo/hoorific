// Package bedrock provides explicit Amazon Bedrock runtime/control-plane bindings.
package bedrock

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"hoorific/internal/core"
)

type Option func(*Connector)

func WithHTTPClient(h *http.Client) Option {
	return func(c *Connector) {
		if h != nil {
			c.client = h
		}
	}
}
func WithCredential(l core.CredentialLease) Option { return func(c *Connector) { c.credential = l } }

type Connector struct {
	id         string
	client     *http.Client
	credential core.CredentialLease
}

func New(opts ...Option) *Connector {
	c := &Connector{id: "bedrock", client: http.DefaultClient}
	for _, o := range opts {
		o(c)
	}
	return c
}
func (c *Connector) Descriptor() core.ConnectorDescriptor {
	return core.ConnectorDescriptor{ID: c.id, Protocols: []core.Protocol{"bedrock-converse", "native"}, Operations: []core.Operation{"generate", "native", "model.list", "prediction", "audio.speech"}}
}
func (c *Connector) Endpoints() []core.NativeEndpoint {
	endpoints := []core.NativeEndpoint{
		{Method: "POST", Path: "/model/{model}/converse", Action: "runtime.converse", Operation: "generate", ModelLocation: "path", Framing: "json"},
		{Method: "POST", Path: "/model/{model}/converse-stream", Action: "runtime.converseStream", Operation: "generate", ModelLocation: "path", Framing: "aws-eventstream"},
		{Method: "POST", Path: "/model/{model}/invoke", Action: "runtime.invokeModel", Operation: "native", ModelLocation: "path", Framing: "json"},
		{Method: "POST", Path: "/model/{model}/invoke-with-response-stream", Action: "runtime.invokeModelWithResponseStream", Operation: "native", ModelLocation: "path", Framing: "aws-eventstream"},
		{Method: "POST", Path: "/model/{model}/invoke-with-bidirectional-stream", Action: "runtime.invokeModelWithBidirectionalStream", Operation: "audio.speech", ModelLocation: "path", Framing: "h2-eventstream-duplex"},
		{Method: "POST", Path: "/async-invoke", Action: "runtime.startAsyncInvoke", Operation: "prediction", ModelLocation: "body", Framing: "json", Stateful: true},
		{Method: "GET", Path: "/async-invoke/{invocation_arn}", Action: "runtime.getAsyncInvoke", Operation: "prediction", Framing: "json", Stateful: true, ResourceIDField: "invocation_arn"},
		{Method: "GET", Path: "/async-invoke", Action: "runtime.listAsyncInvokes", Operation: "prediction", Framing: "json", Stateful: true},
		{Method: "GET", Path: "/foundation-models", Action: "control.listFoundationModels", Operation: "model.list", Framing: "json"},
	}
	return endpoints
}

func bedrockResponse(action string) core.NativeResponsePolicy {
	switch action {
	case "runtime.startAsyncInvoke":
		return core.NativeResponsePolicy{
			IDField: "invocationArn", StatusField: "status",
			PollEndpoint: "/async-invoke/{invocation_arn}", PollMethod: http.MethodGet,
			PollAction: "runtime.getAsyncInvoke", PollOperation: "prediction", Async: true,
			TerminalStatuses: []string{"Completed", "Failed"}, FailureStatuses: []string{"Failed"},
		}
	case "runtime.getAsyncInvoke":
		return core.NativeResponsePolicy{
			IDField: "invocationArn", StatusField: "status",
			TerminalStatuses: []string{"Completed", "Failed"}, FailureStatuses: []string{"Failed"},
		}
	default:
		return core.NativeResponsePolicy{}
	}
}

func (c *Connector) EndpointsFor(conn core.Connection) ([]core.NativeEndpoint, error) {
	control := strings.TrimSpace(conn.Settings["control_plane"])
	if control != "" && control != "true" && control != "false" {
		return nil, fmt.Errorf("invalid bedrock control_plane setting %q", control)
	}
	controlPlane := control == "true"
	var endpoints []core.NativeEndpoint
	for _, e := range c.Endpoints() {
		if strings.HasPrefix(e.Action, "control.") == controlPlane {
			endpoints = append(endpoints, e)
		}
	}
	return endpoints, nil
}

func (c *Connector) DescriptorFor(conn core.Connection) (core.ConnectorDescriptor, error) {
	endpoints, err := c.EndpointsFor(conn)
	if err != nil {
		return core.ConnectorDescriptor{}, err
	}
	d := c.Descriptor()
	d.Protocols = nil
	d.Operations = nil
	seen := map[core.Operation]bool{}
	hasNative := false
	for _, e := range endpoints {
		if !seen[e.Operation] {
			d.Operations = append(d.Operations, e.Operation)
			seen[e.Operation] = true
		}
		hasNative = hasNative || e.Operation == "native"
	}
	if len(endpoints) > 0 {
		d.Protocols = append(d.Protocols, "bedrock-converse")
	}
	if hasNative {
		d.Protocols = append(d.Protocols, "native")
	}
	return d, nil
}
func (c *Connector) Bind(ctx context.Context, target core.Target, op core.Operation) (core.Binding, error) {
	var conn core.Connection
	var action string
	vars := map[string]string{}
	switch call := target.(type) {
	case core.ModelCall:
		conn = call.Connection
		switch op {
		case "generate":
			action = "runtime.converse"
		case "native":
			action = "runtime.invokeModel"
		case "audio.speech":
			action = "runtime.invokeModelWithBidirectionalStream"
		case "prediction":
			action = "runtime.startAsyncInvoke"
		case "model.list":
			action = "control.listFoundationModels"
		default:
			return core.Binding{}, fmt.Errorf("unsupported bedrock operation %q", op)
		}
		if op != "model.list" && call.Model.ID == "" {
			return core.Binding{}, fmt.Errorf("bedrock model is required")
		}
		if op != "model.list" {
			vars["model"] = call.Model.ID
		}
	case core.ConnectionResourceCall:
		conn = call.Connection
		action = call.Action
		switch action {
		case "runtime.getAsyncInvoke":
			if call.ResourceID == "" {
				return core.Binding{}, fmt.Errorf("bedrock invocation ARN is required")
			}
			vars["invocation_arn"] = call.ResourceID
		case "runtime.listAsyncInvokes", "control.listFoundationModels":
			if call.ResourceID != "" {
				return core.Binding{}, fmt.Errorf("bedrock action %q does not accept a resource ID", action)
			}
		default:
			return core.Binding{}, fmt.Errorf("unsupported bedrock resource action %q", action)
		}
	default:
		return core.Binding{}, fmt.Errorf("unsupported bedrock target")
	}
	endpoints, err := c.EndpointsFor(conn)
	if err != nil {
		return core.Binding{}, err
	}
	for _, e := range endpoints {
		if e.Operation == op && e.Action == action {
			return c.BindEndpoint(ctx, conn, e, vars)
		}
	}
	return core.Binding{}, fmt.Errorf("unsupported bedrock operation %q for action %q", op, action)
}

func (c *Connector) BindEndpoint(_ context.Context, conn core.Connection, e core.NativeEndpoint, vars map[string]string) (core.Binding, error) {
	endpoints, err := c.EndpointsFor(conn)
	if err != nil {
		return core.Binding{}, err
	}
	registered := false
	for _, candidate := range endpoints {
		if candidate == e {
			registered = true
			break
		}
	}
	if !registered {
		return core.Binding{}, fmt.Errorf("bedrock endpoint is not available for this connection")
	}
	if e.ResourceIDField == "" && (strings.TrimSpace(vars["invocation_arn"]) != "" || strings.TrimSpace(vars["resource_id"]) != "") {
		return core.Binding{}, fmt.Errorf("bedrock %s does not accept a resource ID", e.Action)
	}
	p := e.Path
	for k, v := range vars {
		if v == "" || v == "." || v == ".." || strings.ContainsAny(v, "\\%?#\x00\r\n") || (k != "invocation_arn" && k != "model" && strings.Contains(v, "/")) {
			return core.Binding{}, fmt.Errorf("invalid bedrock %s", k)
		}
		p = strings.ReplaceAll(p, "{"+k+"}", url.PathEscape(v))
	}
	if strings.Contains(p, "{") {
		return core.Binding{}, fmt.Errorf("unbound bedrock path variable")
	}
	binding := core.Binding{Codec: core.CodecKey{Protocol: core.Protocol("bedrock-converse"), Variant: "", Operation: e.Operation}, Endpoint: p, Method: e.Method, ModelLocation: e.ModelLocation, Framing: e.Framing, ReplaySafe: e.Method == "GET", CancellationSupported: true, Response: bedrockResponse(e.Action)}
	if e.Action == "runtime.startAsyncInvoke" {
		binding.ModelField = "modelId"
	}
	return binding, nil
}
func (c *Connector) Discover(ctx context.Context, conn core.Connection) ([]core.Model, error) {
	if conn.BaseURL == "" {
		return nil, fmt.Errorf("bedrock base URL is required")
	}
	if conn.Settings["control_plane"] != "true" {
		return nil, fmt.Errorf("bedrock catalog requires control-plane connection")
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(conn.BaseURL, "/")+"/foundation-models", nil)
	if e != nil {
		return nil, fmt.Errorf("bedrock discovery request: %w", e)
	}
	if c.credential != nil {
		if e = c.credential.Authorize(ctx, req); e != nil {
			return nil, e
		}
	}
	resp, e := c.client.Do(req)
	if e != nil {
		return nil, fmt.Errorf("bedrock discovery failed: %w", e)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("bedrock discovery returned status %d", resp.StatusCode)
	}
	var body struct {
		Summaries []struct {
			ModelID   string   `json:"modelId"`
			Input     []string `json:"inputModalities"`
			Output    []string `json:"outputModalities"`
			Streaming bool     `json:"responseStreamingSupported"`
		} `json:"modelSummaries"`
	}
	if e = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&body); e != nil {
		return nil, fmt.Errorf("bedrock discovery response: %w", e)
	}
	out := make([]core.Model, 0, len(body.Summaries))
	for _, m := range body.Summaries {
		if m.ModelID == "" {
			continue
		}
		f := map[string]core.Support{"streaming": core.Unsupported}
		if m.Streaming {
			f["streaming"] = core.Supported
		}
		out = append(out, core.Model{ID: m.ModelID, ConnectionID: conn.ID, InputModalities: m.Input, OutputModalities: m.Output, Features: f})
	}
	return out, nil
}
func (c *Connector) Inspect(context.Context, core.Target, core.Operation, []byte) error { return nil }
func (c *Connector) BindStream(ctx context.Context, target core.Target, op core.Operation, stream bool) (core.Binding, error) {
	if !stream {
		return c.Bind(ctx, target, op)
	}
	call, ok := target.(core.ModelCall)
	if !ok {
		return core.Binding{}, fmt.Errorf("bedrock stream operation requires model call")
	}
	endpoints, err := c.EndpointsFor(call.Connection)
	if err != nil {
		return core.Binding{}, err
	}
	action := "runtime.converseStream"
	if op == "native" {
		action = "runtime.invokeModelWithResponseStream"
	}
	for _, e := range endpoints {
		if e.Action == action && e.Operation == op {
			return c.BindEndpoint(ctx, call.Connection, e, map[string]string{"model": call.Model.ID})
		}
	}
	return core.Binding{}, fmt.Errorf("bedrock streaming is unsupported for operation %q", op)
}
