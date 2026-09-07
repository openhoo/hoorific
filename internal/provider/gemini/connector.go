// Package gemini inventories the official Gemini Developer API, not Vertex AI.
package gemini

import (
	"context"
	"net/url"
	"strings"

	"hoorific/internal/core"
	"hoorific/internal/provider/endpoint"
)

const BaseURL = "https://generativelanguage.googleapis.com"

// Connector preserves native Gemini schemas and binds qualified resource names.
// Discovery paginates models using endpoint.WithDiscovery's injected client and
// credentials; it does not infer capabilities or approve model operations.
type Connector struct {
	*endpoint.Connector
	routes []endpoint.Route
}

// New constructs the Gemini v1beta inventory. Options are shared endpoint options.
func New(opts ...endpoint.Option) *Connector {
	routes := inventory()
	options := append([]endpoint.Option{endpoint.WithDiscoveryPath("v1beta/models")}, opts...)
	return &Connector{Connector: endpoint.New("gemini", BaseURL, routes, options...), routes: routes}
}

func route(method, path, action string, op core.Operation, protocol core.Protocol, framing core.Framing, location string, stateful bool) endpoint.Route {
	r := endpoint.E(method, path, action, op, protocol, framing, location, stateful)
	// The conversation and embedding codecs are registered with an empty
	// variant; native Gemini routes also use the core native codec key.
	r.Variant = ""
	switch action {
	case "models.predictLongRunning", "operations.get":
		r.Response = core.NativeResponsePolicy{
			IDField: "name", StatusField: "done", PollEndpoint: "v1beta/{operation}",
			PollMethod: "GET", PollAction: "operations.get", PollOperation: "video",
			Async: true, TerminalStatuses: []string{"true"},
		}
	case "models.batchGenerateContent", "models.asyncBatchEmbedContent":
		r.Response = core.NativeResponsePolicy{
			IDField: "name", StatusField: "done", PollEndpoint: "v1beta/batches/{id}",
			PollMethod: "GET", PollAction: "batches.get", PollOperation: "batch",
			Async: true, TerminalStatuses: []string{"true"},
		}
	case "batches.get":
		r.Response = core.NativeResponsePolicy{
			IDField: "name", StatusField: "done", PollEndpoint: "v1beta/batches/{id}",
			PollMethod: "GET", PollAction: "batches.get", PollOperation: "batch",
			Async: true, TerminalStatuses: []string{"true"},
		}
	case "files.get":
		r.Response = core.NativeResponsePolicy{IDField: "name"}
	case "media.create":
		r.Response = core.NativeResponsePolicy{IDField: "file.name", StatusField: "file.state"}
	case "media.upload":
		// The resumable-start response is header-only: forcing file metadata
		// extraction here would reject the non-JSON acknowledgement. The
		// finalized File metadata is available through files.get/media.create.
		r.Response = core.NativeResponsePolicy{
			ContinuationHeaders: []string{"X-Goog-Upload-URL"},
			ContinuationOrigins: []string{BaseURL},
			ContinuationMethods: map[string]string{"X-Goog-Upload-URL": "POST"},
			ContinuationActions: map[string]string{"X-Goog-Upload-URL": "upload.continue"},
		}
	}
	return r
}

func resourceRoute(method, path, action string, op core.Operation, protocol core.Protocol, framing core.Framing, location string, stateful bool, field string) endpoint.Route {
	r := route(method, path, action, op, protocol, framing, location, stateful)
	r.ResourceIDField = field
	return r
}

func inventory() []endpoint.Route {
	return []endpoint.Route{
		// https://ai.google.dev/api/generate-content ; SSE requires alt=sse.
		route("POST", "v1beta/models/{model}:generateContent", "models.generateContent", "generate", "gemini-content", "json", "path", false),
		route("POST", "v1beta/models/{model}:streamGenerateContent?alt=sse", "models.streamGenerateContent", "generate", "gemini-content", "sse-data", "path", false),
		// https://ai.google.dev/api/embeddings ; synchronous batching is not an LRO.
		route("POST", "v1beta/models/{model}:embedContent", "models.embedContent", "embed", "gemini-content", "json", "path", false),
		route("POST", "v1beta/models/{model}:batchEmbedContents", "models.batchEmbedContents", "embed", "native", "json", "path", false),
		// https://ai.google.dev/api/tokens
		route("POST", "v1beta/models/{model}:countTokens", "models.countTokens", "count_tokens", "gemini-content", "json", "path", false),
		// https://ai.google.dev/api/models ; predict uses native instances/parameters.
		route("GET", "v1beta/models", "models.list", "model.list", "native", "json", "none", false),
		resourceRoute("GET", "v1beta/models/{model}", "models.get", "model.list", "native", "json", "path", false, "model"),
		route("POST", "v1beta/models/{model}:predict", "models.predict", "image.generate", "native", "json", "path", false),
		route("POST", "v1beta/models/{model}:predictLongRunning", "models.predictLongRunning", "video", "native", "json", "path", true),
		// https://ai.google.dev/gemini-api/docs/veo : poll the returned operation name.
		// No video cancel/delete/list RPC is documented here; none is fabricated.
		// Download uses the returned video URI, not a guessed operation subresource.
		resourceRoute("GET", "v1beta/{operation}", "operations.get", "video", "native", "json", "none", true, "operation"),
		// https://ai.google.dev/api/files ; registration and metadata creation differ
		// from resumable media upload. Uploaded files have no cancellation RPC.
		route("GET", "v1beta/files", "files.list", "file", "native", "json", "none", true),
		resourceRoute("GET", "v1beta/files/{id}", "files.get", "file", "native", "json", "none", true, "id"),
		resourceRoute("DELETE", "v1beta/files/{id}", "files.delete", "file", "native", "json", "none", true, "id"),
		route("POST", "v1beta/files:register", "files.register", "file", "native", "json", "none", true),
		route("POST", "v1beta/files", "media.create", "file", "native", "json", "none", true),
		// Resumable upload is a binary wire after the JSON start request.
		route("POST", "upload/v1beta/files", "media.upload", "upload", "native", "binary", "none", true),
		// https://ai.google.dev/api/batch-api ; cancel is best effort and may return
		// UNIMPLEMENTED. Deleting an operation does NOT cancel the underlying work.
		route("POST", "v1beta/models/{model}:batchGenerateContent", "models.batchGenerateContent", "batch", "native", "json", "path", true),
		route("POST", "v1beta/models/{model}:asyncBatchEmbedContent", "models.asyncBatchEmbedContent", "batch", "native", "json", "path", true),
		route("GET", "v1beta/batches", "batches.list", "batch", "native", "json", "none", true),
		resourceRoute("GET", "v1beta/batches/{id}", "batches.get", "batch", "native", "json", "none", true, "id"),
		resourceRoute("POST", "v1beta/batches/{id}:cancel", "batches.cancel", "batch", "native", "json", "none", true, "id"),
		resourceRoute("DELETE", "v1beta/batches/{id}", "batches.delete", "batch", "native", "json", "none", true, "id"),
		resourceRoute("PATCH", "v1beta/batches/{id}:updateGenerateContentBatch", "batches.updateGenerateContentBatch", "batch", "native", "json", "none", true, "id"),
		resourceRoute("PATCH", "v1beta/batches/{id}:updateEmbedContentBatch", "batches.updateEmbedContentBatch", "batch", "native", "json", "none", true, "id"),
		// https://ai.google.dev/api/live : wss, initial setup.model, bidirectional
		// messages and session resumption, NOT an HTTP SSE or replayable GET call.
		route("GET", "ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent", "live.connect", "realtime", "native", "websocket", "body", true),
		// https://ai.google.dev/api/generate-content#method:-auth_tokens.create
		route("POST", "v1beta/auth_tokens", "auth_tokens.create", "realtime", "native", "json", "none", true),
	}
}

func unsupported(message string) error {
	return core.GatewayError{Code: "unsupported_operation", HTTPStatus: 400, Message: message, Origin: "gateway"}
}

// Bind requires explicit resource actions; it never silently chooses a destructive
// lifecycle action. Model calls select the primary operation, while native callers
// select streaming/batch variants through BindEndpoint.
func (c *Connector) Bind(ctx context.Context, target core.Target, op core.Operation) (core.Binding, error) {
	if err := ctx.Err(); err != nil {
		return core.Binding{}, err
	}
	var conn core.Connection
	params := map[string]string{}
	action := ""
	modelCall := false
	switch t := target.(type) {
	case core.ModelCall:
		if err := c.Inspect(ctx, target, op, nil); err != nil {
			return core.Binding{}, err
		}
		conn, params["model"], modelCall = t.Connection, t.Model.ID, true
	case core.ConnectionResourceCall:
		conn, action = t.Connection, t.Action
		if action == "" {
			return core.Binding{}, unsupported("Gemini resource calls require an explicit inventory action")
		}
		params["id"], params["model"], params["operation"] = t.ResourceID, t.ResourceID, t.ResourceID
	default:
		return core.Binding{}, unsupported("Unknown Gemini target")
	}
	for _, r := range c.routes {
		if r.Operation != op || (!modelCall && r.Action != action) {
			continue
		}
		if modelCall && r.ModelLocation == "none" {
			continue
		}
		return c.BindEndpoint(ctx, conn, r.NativeEndpoint, params)
	}
	return core.Binding{}, unsupported("Gemini endpoint action is not supported; no cancellation is available outside batches.cancel")
}

// BindStream selects the exact unary or SSE generation descriptor. The stream
// flag is transport selection only; model-manifest approval remains mandatory.
// It intentionally does not alter Bind, preserving existing non-stream callers.
func (c *Connector) BindStream(ctx context.Context, target core.Target, op core.Operation, stream bool) (core.Binding, error) {
	if err := ctx.Err(); err != nil {
		return core.Binding{}, err
	}
	if !stream {
		return c.Bind(ctx, target, op)
	}
	if op != "generate" {
		return core.Binding{}, unsupported("Gemini streaming selection is only defined for generate")
	}
	t, ok := target.(core.ModelCall)
	if !ok {
		return core.Binding{}, unsupported("Gemini streaming generation requires a model target")
	}
	if err := c.Inspect(ctx, target, op, nil); err != nil {
		return core.Binding{}, err
	}
	action := "models.generateContent"
	if stream {
		action = "models.streamGenerateContent"
	}
	for _, r := range c.routes {
		if r.Operation != op || r.Action != action {
			continue
		}
		return c.BindEndpoint(ctx, t.Connection, r.NativeEndpoint, map[string]string{"model": t.Model.ID})
	}
	return core.Binding{}, unsupported("Gemini generation stream descriptor is not in the inventory")
}

// component accepts an unqualified ID or its exact collection-qualified name.
// Escape each opaque component once, never the resource's structural slashes.
func component(value, collection string) (string, error) {
	value = strings.TrimPrefix(value, collection+"/")
	if strings.ContainsAny(value, ":{}") {
		return "", unsupported("Invalid Gemini resource component")
	}
	return endpoint.Segment(value)
}

func operationName(value string) (string, error) {
	parts := strings.Split(value, "/")
	switch {
	case len(parts) == 2 && parts[0] == "operations":
	case len(parts) == 4 && parts[0] == "models" && parts[2] == "operations":
	default:
		return "", unsupported("Expected operations/id or models/model/operations/id")
	}
	for i, part := range parts {
		escaped, err := component(part, "")
		if err != nil {
			return "", err
		}
		parts[i] = escaped
	}
	return strings.Join(parts, "/"), nil
}

func (c *Connector) BindEndpoint(ctx context.Context, conn core.Connection, e core.NativeEndpoint, params map[string]string) (core.Binding, error) {
	if err := ctx.Err(); err != nil {
		return core.Binding{}, err
	}
	var selected *endpoint.Route
	for i := range c.routes {
		if c.routes[i].NativeEndpoint == e {
			selected = &c.routes[i]
			break
		}
	}
	if selected == nil {
		return core.Binding{}, unsupported("Endpoint is not in the Gemini inventory")
	}
	// Live uses the official WSS endpoint and websocket framing. The core now
	// carries the transport framing through Binding; setup and resumption JSON
	// remain native codec payloads rather than being rewritten here.
	path := e.Path
	for _, key := range []string{"model", "id", "operation"} {
		marker := "{" + key + "}"
		if !strings.Contains(path, marker) {
			continue
		}
		var value string
		var err error
		param := params[key]
		// ResourceIDField is an alias for the generic resource_id supplied by
		// core callers. Resolve it locally so caller-owned maps are unchanged.
		if param == "" && e.ResourceIDField == key {
			param = params["resource_id"]
		}
		switch key {
		case "model":
			value, err = component(param, "models")
		case "id":
			collection := "files"
			if strings.HasPrefix(e.Action, "batches.") {
				collection = "batches"
			}
			value, err = component(param, collection)
		case "operation":
			value, err = operationName(param)
		}
		if err != nil {
			return core.Binding{}, err
		}
		path = strings.ReplaceAll(path, marker, value)
	}
	if err := endpoint.ValidateRelative(path); err != nil {
		return core.Binding{}, err
	}
	b := core.Binding{
		Codec:      core.CodecKey{Protocol: selected.Protocol, Variant: selected.Variant, Operation: e.Operation},
		ReplaySafe: e.Method == "GET" && e.Framing != "websocket", CancellationSupported: e.Action == "batches.cancel",
		Endpoint: path, Method: e.Method, ModelLocation: e.ModelLocation, Framing: e.Framing,
		Response: selected.Response,
	}
	if e.Action == "live.connect" {
		b.Realtime = &core.RealtimePolicy{
			Protocol: "gemini", WholeSessionBound: false,
			RequirePayloadEnforcement: true,
		}
	}
	if e.Action == "media.upload" {
		// Gemini's resumable protocol uses a start request followed by binary
		// chunks addressed by the returned session URL and explicit offsets.
		b.AllowedRequestHeaders = []string{
			"X-Goog-Upload-Protocol", "X-Goog-Upload-Command",
			"X-Goog-Upload-Header-Content-Length", "X-Goog-Upload-Header-Content-Type",
			"X-Goog-Upload-Offset", "Content-Length", "Content-Type",
		}
		if conn.BaseURL != "" {
			base, err := url.Parse(conn.BaseURL)
			if err != nil || base.Host == "" || base.User != nil || (base.Scheme != "https" && base.Scheme != "http") || base.RawQuery != "" || base.Fragment != "" {
				return core.Binding{}, unsupported("Invalid configured upload origin")
			}
			// The configured endpoint may serve its own resumable session URL.
			// Copy before extending: never mutate the shared inventory descriptor.
			b.Response.ContinuationOrigins = append(append([]string(nil), b.Response.ContinuationOrigins...), base.Scheme+"://"+base.Host)
		}
	}
	return b, nil
}

// Inspect checks the approved operation without interpreting native Gemini tools
// as OpenAI custom_tools, or assuming capabilities from a model's name. Native
// schema/feature interpretation belongs to the Gemini codec and admission layer.
func (c *Connector) Inspect(ctx context.Context, target core.Target, op core.Operation, _ []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if t, ok := target.(core.ModelCall); ok {
		for _, approved := range t.Model.Operations {
			if approved == op {
				return nil
			}
		}
		return unsupported("Model manifest does not approve this Gemini operation")
	}
	return nil
}

var _ core.Connector = (*Connector)(nil)
var _ core.EndpointBinder = (*Connector)(nil)
var _ core.EndpointInventory = (*Connector)(nil)
var _ core.ScopeInspector = (*Connector)(nil)
var _ core.Discoverer = (*Connector)(nil)
var _ core.StreamBinder = (*Connector)(nil)
