// Package xaisubscription binds the source-verified, opt-in xAI OAuth
// subscription path. It is distinct from an official API-key connection.
package xaisubscription

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"

	"hoorific/internal/core"
	"hoorific/internal/protocol/framing"
	"hoorific/internal/provider/subscription"
)

const cliBaseURL = "https://cli-chat-proxy.grok.com/v1"

type Connector struct{ *subscription.Connector }

func New() *Connector {
	headersFor := func(core.Connection) http.Header {
		return http.Header{
			"X-XAI-Token-Auth":         []string{"xai-grok-cli"},
			"x-grok-client-version":    []string{"0.2.120"},
			"User-Agent":               []string{"xai-grok-workspace/0.2.120"},
			"x-grok-client-identifier": []string{"grok-shell"},
			"x-authenticateresponse":   []string{"authenticate-response"},
		}
	}
	return &Connector{subscription.New("xai-subscription", []subscription.Route{
		// The executor's unary method also forces stream=true upstream. The
		// wire adapter aggregates its terminal SSE event back to JSON.
		{Endpoint: core.NativeEndpoint{Method: "POST", Path: "responses", Action: "responses.create.stream", Operation: "generate", ModelLocation: "body", Framing: "sse-data"}, Protocol: "openai-responses", Headers: http.Header{"Accept": []string{"text/event-stream"}}, HeadersFor: headersFor},
	})}
}

func (c *Connector) Descriptor() core.ConnectorDescriptor {
	return c.Connector.Descriptor()
}

func (c *Connector) Bind(ctx context.Context, t core.Target, o core.Operation) (core.Binding, error) {
	return c.BindStream(ctx, t, o, false)
}

func (c *Connector) BindStream(ctx context.Context, t core.Target, o core.Operation, stream bool) (core.Binding, error) {
	if err := validateConnection(t); err != nil {
		return core.Binding{}, err
	}
	if !stream {
		call, ok := t.(core.ModelCall)
		if !ok {
			return core.Binding{}, unavailable("xAI unary subscription binding requires a model target")
		}
		if err := c.Connector.Inspect(ctx, t, o, nil); err != nil {
			return core.Binding{}, err
		}
		endpoints := c.Connector.Endpoints()
		if len(endpoints) == 0 {
			return core.Binding{}, unavailable("xAI subscription streaming endpoint is unavailable")
		}
		binding, err := c.Connector.BindEndpoint(ctx, call.Connection, endpoints[0], map[string]string{"model": call.Model.ID})
		if err != nil {
			return core.Binding{}, err
		}
		binding.Framing = "json"
		return binding, nil
	}
	return c.Connector.BindStream(ctx, t, o, true)
}

func (c *Connector) BindEndpoint(ctx context.Context, conn core.Connection, e core.NativeEndpoint, vars map[string]string) (core.Binding, error) {
	if e.Action == "responses.create" {
		return core.Binding{}, unavailable("native xAI subscription operation exposes only the streaming Responses endpoint")
	}
	if err := validateConnection(core.ModelCall{Connection: conn}); err != nil {
		return core.Binding{}, err
	}
	return c.Connector.BindEndpoint(ctx, conn, e, vars)
}

// AdaptRequest preserves the client mode while satisfying xAI Grok CLI's
// source contract: both its unary executor and stream executor send stream=true.
func (c *Connector) AdaptRequest(ctx context.Context, _ core.Target, _ core.Binding, body []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return nil, unavailable("xAI Responses request must be a JSON object")
	}
	fields["stream"] = json.RawMessage("true")
	return json.Marshal(fields)
}

// AdaptResponse aggregates the terminal response object from the forced SSE
// upstream for a client-unary binding. Stream bindings remain untouched.
func (c *Connector) AdaptResponse(ctx context.Context, binding core.Binding, response *http.Response) error {
	if binding.Framing != "json" {
		return nil
	}
	if response == nil || response.Body == nil {
		return wireFailure("xAI Responses response body is unavailable")
	}
	defer response.Body.Close()
	decoder := framing.NewSSEDecoder(response.Body, 32<<20)
	const maxAggregate = 32 << 20
	total := 0
	outputs := make(map[int]json.RawMessage)
	var fallback []json.RawMessage
	var completed json.RawMessage
	for {
		event, err := decoder.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return wireFailure("xAI Responses stream read failed: " + err.Error())
		}
		total += len(event.Data)
		if total > maxAggregate {
			return wireFailure("xAI Responses stream exceeded the aggregation limit")
		}
		var payload struct {
			Type        string          `json:"type"`
			Response    json.RawMessage `json:"response"`
			Item        json.RawMessage `json:"item"`
			OutputIndex *int            `json:"output_index"`
		}
		if json.Unmarshal([]byte(event.Data), &payload) != nil {
			continue
		}
		if payload.Type == "response.output_item.done" && json.Valid(payload.Item) {
			if payload.OutputIndex != nil {
				outputs[*payload.OutputIndex] = append(json.RawMessage(nil), payload.Item...)
			} else {
				fallback = append(fallback, append(json.RawMessage(nil), payload.Item...))
			}
			continue
		}
		if (payload.Type == "response.completed" || payload.Type == "response.incomplete") && json.Valid(payload.Response) {
			completed = patchCompletedResponse(payload.Response, outputs, fallback)
			break
		}
	}
	if len(completed) == 0 {
		return wireFailure("xAI Responses stream ended without a terminal response")
	}
	response.Body = io.NopCloser(bytes.NewReader(completed))
	response.ContentLength = int64(len(completed))
	response.Header.Set("Content-Type", "application/json")
	return nil
}

func patchCompletedResponse(raw json.RawMessage, outputs map[int]json.RawMessage, fallback []json.RawMessage) json.RawMessage {
	var response map[string]json.RawMessage
	if json.Unmarshal(raw, &response) != nil {
		return raw
	}
	if existing, ok := response["output"]; ok {
		var items []json.RawMessage
		if json.Unmarshal(existing, &items) == nil && len(items) > 0 {
			return raw
		}
	}
	if len(outputs) == 0 && len(fallback) == 0 {
		return raw
	}
	indexes := make([]int, 0, len(outputs))
	for index := range outputs {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	items := make([]json.RawMessage, 0, len(outputs)+len(fallback))
	for _, index := range indexes {
		items = append(items, outputs[index])
	}
	items = append(items, fallback...)
	encoded, err := json.Marshal(items)
	if err != nil {
		return raw
	}
	response["output"] = encoded
	patched, err := json.Marshal(response)
	if err != nil {
		return raw
	}
	return patched
}

func validateConnection(target core.Target) error {
	var conn core.Connection
	switch call := target.(type) {
	case core.ModelCall:
		conn = call.Connection
	case core.ConnectionResourceCall:
		conn = call.Connection
	default:
		return unavailable("xAI subscription operation requires a pinned connection")
	}
	if conn.Settings["using_api"] != "false" {
		return unavailable("xAI subscription requires using_api=false; use the official xAI API connector for using_api=true")
	}
	base := strings.TrimRight(strings.TrimSpace(conn.BaseURL), "/")
	if base != cliBaseURL {
		return unavailable("xAI subscription requires the source-defined cli-chat-proxy.grok.com/v1 base URL")
	}
	return nil
}

func unavailable(message string) error {
	return core.GatewayError{Code: "connection_required", HTTPStatus: 400, Message: message, Origin: "gateway"}
}

func wireFailure(message string) error {
	return core.GatewayError{Code: "upstream_outcome_unknown", HTTPStatus: 502, Message: message, Origin: "gateway"}
}

var _ core.Connector = (*Connector)(nil)
var _ core.StreamBinder = (*Connector)(nil)
var _ core.EndpointBinder = (*Connector)(nil)
var _ core.WireAdapter = (*Connector)(nil)
