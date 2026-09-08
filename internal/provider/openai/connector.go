// Package openai describes the official OpenAI generation and media API.
package openai

import (
	"context"
	"encoding/json"
	"strings"

	"hoorific/internal/core"
	"hoorific/internal/provider/endpoint"
)

const DefaultBaseURL = "https://api.openai.com/v1"

// Connector preserves native request schemas and exposes explicit endpoint binding.
// Nested resource IDs must be supplied individually to BindEndpoint; ResourceID
// alone cannot represent both a conversation_id and an item_id.
type Connector struct {
	*endpoint.Connector
}

// New constructs the official API inventory. Discovery requires an injected
// client and credential source via endpoint.WithDiscovery; it does not infer
// model capabilities from names. Explicit endpoint binding selects alternative
// protocols and stream modes; a plain generate ModelCall defaults to Chat.
func New(opts ...endpoint.Option) *Connector {
	options := append([]endpoint.Option{endpoint.WithDiscoveryPath("models")}, opts...)
	return &Connector{Connector: endpoint.New("openai", DefaultBaseURL, routes(), options...)}
}

// route records a documented method/path pair. Actions name inventory choices,
// not additional URL suffixes. binary denotes native multipart or byte wire;
// the native codec must preserve content types and multipart boundaries.
func route(method, path, action string, op core.Operation, protocol core.Protocol, framing core.Framing, location string, stateful bool, resourceID string) endpoint.Route {
	r := endpoint.E(method, path, action, op, protocol, framing, location, stateful)
	// Portable OpenAI codecs are registered with their protocol and empty
	// variant. Native resource routes also remain unqualified because no
	// provider-local native codec is registered by the core registry.
	r.Variant = ""
	r.ResourceIDField = resourceID
	return r
}

func routes() []endpoint.Route {
	routes := []endpoint.Route{
		// Official methods and paths: https://developers.openai.com/api/reference/resources/chat
		// Stored completions expose retrieval/update/delete, but no cancellation.
		route("POST", "chat/completions", "chat.create", "generate", "openai-chat", "json", "body", false, ""),
		route("POST", "chat/completions", "chat.create.stream", "generate", "openai-chat", "sse-data", "body", false, ""),
		route("GET", "chat/completions", "chat.list", "chat.resource", "native", "json", "none", true, ""),
		route("GET", "chat/completions/{completion_id}", "chat.retrieve", "chat.resource", "native", "json", "none", true, "completion_id"),
		route("POST", "chat/completions/{completion_id}", "chat.update", "chat.resource", "native", "json", "none", true, "completion_id"),
		route("DELETE", "chat/completions/{completion_id}", "chat.delete", "chat.resource", "native", "json", "none", true, "completion_id"),
		route("GET", "chat/completions/{completion_id}/messages", "chat.messages.list", "chat.resource", "native", "json", "none", true, "completion_id"),

		// https://developers.openai.com/api/reference/resources/completions
		route("POST", "completions", "completions.create", "complete", "openai-completion", "json", "body", false, ""),
		route("POST", "completions", "completions.create.stream", "complete", "openai-completion", "sse-data", "body", false, ""),
		// https://developers.openai.com/api/reference/resources/embeddings
		route("POST", "embeddings", "embeddings.create", "embed", "openai-chat", "json", "body", false, ""),
		// https://developers.openai.com/api/reference/resources/moderations
		route("POST", "moderations", "moderations.create", "moderation", "native", "json", "body", false, ""),
		// https://developers.openai.com/api/reference/resources/models
		route("GET", "models", "models.list", "model.list", "native", "json", "none", false, ""),
		route("GET", "models/{model}", "models.retrieve", "model.list", "native", "json", "path", true, "model"),
		route("DELETE", "models/{model}", "models.delete", "model.list", "native", "json", "path", true, "model"),

		// https://developers.openai.com/api/reference/resources/images
		// Edits accept native multipart or JSON; variations use multipart.
		route("POST", "images/generations", "images.generate", "image.generate", "openai-chat", "json", "body", false, ""),
		route("POST", "images/generations", "images.generate.stream", "image.generate", "openai-chat", "sse-named", "body", false, ""),
		route("POST", "images/edits", "images.edit", "image.edit", "openai-chat", "binary", "body", false, ""),
		route("POST", "images/edits", "images.edit.json", "image.edit", "openai-chat", "json", "body", false, ""),
		route("POST", "images/edits", "images.edit.stream", "image.edit", "openai-chat", "sse-named", "body", false, ""),
		route("POST", "images/variations", "images.variation", "image.variation", "openai-chat", "binary", "body", false, ""),

		// https://developers.openai.com/api/reference/resources/audio
		// Speech returns bytes or SSE. Transcription/translation and voice
		// creation/consent creation accept multipart; native output formats vary.
		route("POST", "audio/speech", "audio.speech", "audio.speech", "openai-chat", "binary", "body", false, ""),
		route("POST", "audio/speech", "audio.speech.stream", "audio.speech", "openai-chat", "sse-named", "body", false, ""),
		route("POST", "audio/transcriptions", "audio.transcribe", "audio.transcribe", "openai-chat", "binary", "body", false, ""),
		route("POST", "audio/transcriptions", "audio.transcribe.stream", "audio.transcribe", "openai-chat", "sse-named", "body", false, ""),
		route("POST", "audio/translations", "audio.translate", "audio.translate", "openai-chat", "binary", "body", false, ""),
		route("POST", "audio/voices", "audio.voices.create", "audio.voice", "native", "binary", "none", true, ""),
		route("POST", "audio/voice_consents", "audio.voice_consents.create", "audio.voice_consent", "native", "binary", "none", true, ""),
		route("GET", "audio/voice_consents", "audio.voice_consents.list", "audio.voice_consent", "native", "json", "none", true, ""),
		route("GET", "audio/voice_consents/{consent_id}", "audio.voice_consents.retrieve", "audio.voice_consent", "native", "json", "none", true, "consent_id"),
		route("POST", "audio/voice_consents/{consent_id}", "audio.voice_consents.update", "audio.voice_consent", "native", "json", "none", true, "consent_id"),
		route("DELETE", "audio/voice_consents/{consent_id}", "audio.voice_consents.delete", "audio.voice_consent", "native", "json", "none", true, "consent_id"),

		// https://developers.openai.com/api/reference/resources/files
		// File deletion is not cancellation of work consuming the file.
		route("POST", "files", "files.create", "file", "native", "binary", "none", true, ""),
		route("GET", "files", "files.list", "file", "native", "json", "none", true, ""),
		route("GET", "files/{file_id}", "files.retrieve", "file", "native", "json", "none", true, "file_id"),
		route("DELETE", "files/{file_id}", "files.delete", "file", "native", "json", "none", true, "file_id"),
		route("GET", "files/{file_id}/content", "files.content", "file", "native", "binary", "none", true, "file_id"),
		// https://developers.openai.com/api/reference/resources/uploads
		route("POST", "uploads", "uploads.create", "upload", "native", "json", "none", true, ""),
		route("POST", "uploads/{upload_id}/parts", "uploads.parts.create", "upload", "native", "binary", "none", true, "upload_id"),
		route("POST", "uploads/{upload_id}/complete", "uploads.complete", "upload", "native", "json", "none", true, "upload_id"),
		route("POST", "uploads/{upload_id}/cancel", "uploads.cancel", "upload", "native", "json", "none", true, "upload_id"),
		// https://developers.openai.com/api/reference/resources/batches
		route("POST", "batches", "batches.create", "batch", "native", "json", "none", true, ""),
		route("GET", "batches", "batches.list", "batch", "native", "json", "none", true, ""),
		route("GET", "batches/{batch_id}", "batches.retrieve", "batch", "native", "json", "none", true, "batch_id"),
		route("POST", "batches/{batch_id}/cancel", "batches.cancel", "batch", "native", "json", "none", true, "batch_id"),

		// https://developers.openai.com/api/reference/resources/videos
		// No official video cancel action exists. DELETE removes a video.
		route("POST", "videos", "videos.create", "video", "native", "binary", "body", true, ""),
		route("GET", "videos", "videos.list", "video", "native", "json", "none", true, ""),
		route("GET", "videos/{video_id}", "videos.retrieve", "video", "native", "json", "none", true, "video_id"),
		route("DELETE", "videos/{video_id}", "videos.delete", "video", "native", "json", "none", true, "video_id"),
		route("GET", "videos/{video_id}/content", "videos.content", "video", "native", "binary", "none", true, "video_id"),
		route("POST", "videos/{video_id}/remix", "videos.remix", "video", "native", "json", "none", true, "video_id"),
		route("POST", "videos/edits", "videos.edit", "video", "native", "json", "none", true, ""),
		route("POST", "videos/extensions", "videos.extend", "video.extension", "native", "json", "none", true, ""),
		route("POST", "videos/characters", "videos.characters.create", "video.character", "native", "binary", "none", true, ""),
		route("GET", "videos/characters/{character_id}", "videos.characters.retrieve", "video.character", "native", "json", "none", true, "character_id"),

		// https://developers.openai.com/api/reference/resources/responses
		// Cancellation is only valid for background=true responses. This
		// provider does not turn disconnects into a cancellation request.
		route("POST", "responses", "responses.create", "generate", "openai-responses", "json", "body", true, ""),
		route("POST", "responses", "responses.create.stream", "generate", "openai-responses", "sse-named", "body", true, ""),
		route("GET", "responses/{response_id}", "responses.retrieve", "response.resource", "native", "json", "none", true, "response_id"),
		// GET /responses/{response_id}?stream=true is the documented SSE
		// retrieval form; starting_after resumes the event stream.
		// The query is part of this descriptor because generic binding does
		// not synthesize stream=true; callers may add starting_after.
		route("GET", "responses/{response_id}?stream=true", "responses.retrieve.stream", "response.resource", "native", "sse-named", "none", true, "response_id"),
		route("DELETE", "responses/{response_id}", "responses.delete", "response.resource", "native", "json", "none", true, "response_id"),
		route("POST", "responses/{response_id}/cancel", "responses.cancel", "response.resource", "native", "json", "none", true, "response_id"),
		route("GET", "responses/{response_id}/input_items", "responses.input_items.list", "response.resource", "native", "json", "none", true, "response_id"),
		route("POST", "responses/compact", "responses.compact", "response.resource", "native", "json", "body", false, ""),
		route("POST", "responses/input_tokens", "responses.input_tokens", "count_tokens", "native", "json", "body", false, ""),
		// https://developers.openai.com/api/docs/guides/websocket-mode.md
		// The model is carried in response.create frames, not the handshake.
		route("GET", "responses", "responses.connect", "generate", "openai-responses", "websocket", "none", true, ""),

		// https://developers.openai.com/api/reference/resources/conversations
		route("POST", "conversations", "conversations.create", "conversation.resource", "native", "json", "none", true, ""),
		route("GET", "conversations/{conversation_id}", "conversations.retrieve", "conversation.resource", "native", "json", "none", true, "conversation_id"),
		route("POST", "conversations/{conversation_id}", "conversations.update", "conversation.resource", "native", "json", "none", true, "conversation_id"),
		route("DELETE", "conversations/{conversation_id}", "conversations.delete", "conversation.resource", "native", "json", "none", true, "conversation_id"),
		route("POST", "conversations/{conversation_id}/items", "conversations.items.create", "conversation.resource", "native", "json", "none", true, "conversation_id"),
		route("GET", "conversations/{conversation_id}/items", "conversations.items.list", "conversation.resource", "native", "json", "none", true, "conversation_id"),
		route("GET", "conversations/{conversation_id}/items/{item_id}", "conversations.items.retrieve", "conversation.resource", "native", "json", "none", true, "conversation_id"),
		route("DELETE", "conversations/{conversation_id}/items/{item_id}", "conversations.items.delete", "conversation.resource", "native", "json", "none", true, "conversation_id"),

		// https://developers.openai.com/api/reference/resources/realtime
		// Model fields within session configuration remain native nested fields;
		// they must not be rewritten as top-level model fields.
		route("POST", "realtime/client_secrets", "realtime.client_secrets.create", "realtime", "native", "json", "none", true, ""),
		route("POST", "realtime/sessions", "realtime.sessions.create", "realtime", "native", "json", "body", true, ""),
		route("POST", "realtime/transcription_sessions", "realtime.transcription_sessions.create", "realtime", "native", "json", "none", true, ""),
		route("POST", "realtime/calls", "realtime.calls.create", "realtime", "native", "text/sdp", "none", true, ""),
		route("POST", "realtime/calls/{call_id}/accept", "realtime.calls.accept", "realtime", "native", "json", "body", true, "call_id"),
		route("POST", "realtime/calls/{call_id}/reject", "realtime.calls.reject", "realtime", "native", "json", "none", true, "call_id"),
		route("POST", "realtime/calls/{call_id}/refer", "realtime.calls.refer", "realtime", "native", "json", "none", true, "call_id"),
		route("POST", "realtime/calls/{call_id}/hangup", "realtime.calls.hangup", "realtime", "native", "json", "none", true, "call_id"),
		// https://developers.openai.com/api/docs/guides/realtime-websocket
		route("GET", "realtime", "realtime.connect", "realtime", "native", "websocket", "query", true, ""),
		// https://developers.openai.com/api/docs/guides/realtime-translation
		route("POST", "realtime/translations/client_secrets", "realtime.translations.client_secrets.create", "realtime", "native", "json", "none", true, ""),
		route("POST", "realtime/translations/calls", "realtime.translations.calls.create", "realtime", "native", "text/sdp", "none", true, ""),
		route("GET", "realtime/translations", "realtime.translations.connect", "realtime", "native", "websocket", "query", true, ""),
	}
	for i := range routes {
		routes[i].Response = responsePolicy(routes[i].Action)
	}
	return routes
}

func responsePolicy(action string) core.NativeResponsePolicy {
	switch action {
	case "batches.create", "batches.retrieve", "batches.cancel":
		return core.NativeResponsePolicy{
			IDField: "id", StatusField: "status", PollEndpoint: "batches/{id}", PollMethod: "GET",
			PollAction: "batches.retrieve", PollOperation: "batch",
			Async: true, TerminalStatuses: []string{"completed", "failed", "expired", "cancelled"},
			FailureStatuses: []string{"failed", "expired", "cancelled"},
		}
	case "videos.create", "videos.retrieve", "videos.remix", "videos.edit", "videos.extend":
		return core.NativeResponsePolicy{
			IDField: "id", StatusField: "status", PollEndpoint: "videos/{id}", PollMethod: "GET",
			PollAction: "videos.retrieve", PollOperation: "video",
			Async: true, TerminalStatuses: []string{"completed", "failed"},
			FailureStatuses: []string{"failed"},
		}
	case "responses.retrieve", "responses.retrieve.stream", "responses.cancel":
		return core.NativeResponsePolicy{IDField: "id", StatusField: "status", Async: true}
	case "responses.create", "responses.create.stream":
		// A create request is synchronous unless its native body sets
		// background=true; static route metadata cannot inspect that body.
		return core.NativeResponsePolicy{IDField: "id", StatusField: "status"}
	case "files.create", "files.retrieve":
		return core.NativeResponsePolicy{IDField: "id", StatusField: "status"}
	case "files.delete":
		return core.NativeResponsePolicy{IDField: "id"}
	case "uploads.create", "uploads.complete", "uploads.cancel":
		return core.NativeResponsePolicy{IDField: "id", StatusField: "status"}
	case "uploads.parts.create":
		return core.NativeResponsePolicy{IDField: "id"}
	default:
		return core.NativeResponsePolicy{}
	}
}

// Bind requires an explicit action for connection resources. It never interprets
// delete, hangup, or a transport disconnect as an official cancel operation.
func (c *Connector) Bind(ctx context.Context, target core.Target, op core.Operation) (core.Binding, error) {
	if err := ctx.Err(); err != nil {
		return core.Binding{}, err
	}
	if t, ok := target.(core.ConnectionResourceCall); ok {
		if t.Action == "" {
			return core.Binding{}, unsupported("A resource call requires an explicit OpenAI action")
		}
		for _, e := range c.Endpoints() {
			if e.Operation == op && e.Action == t.Action {
				return c.BindEndpoint(ctx, t.Connection, e, map[string]string{"resource_id": t.ResourceID})
			}
		}
		if t.Action == "cancel" || strings.HasSuffix(t.Action, ".cancel") {
			return core.Binding{}, unsupported("OpenAI has no official cancellation action for this resource; only uploads.cancel, batches.cancel and background responses.cancel are supported")
		}
		return core.Binding{}, unsupported("OpenAI endpoint action is not supported for this operation")
	}
	b, err := c.Connector.Bind(ctx, target, op)
	if b.Framing == "websocket" {
		b.ReplaySafe = false
		// Generic ModelCall binding has no action selector; only the
		// explicit realtime.connect resource action receives a policy.
		b.Realtime = openAIRealtimePolicy("", b.Framing)
	}
	return b, err
}

// BindEndpoint accepts named path parameters, never an arbitrary upstream path.
// It copies parameters because the shared helper fills ResourceIDField in-place.
func (c *Connector) BindEndpoint(ctx context.Context, conn core.Connection, e core.NativeEndpoint, params map[string]string) (core.Binding, error) {
	copied := make(map[string]string, len(params)+1)
	for key, value := range params {
		copied[key] = value
	}
	b, err := c.Connector.BindEndpoint(ctx, conn, e, copied)
	if b.Framing == "websocket" || b.Framing == "text/sdp" {
		b.ReplaySafe = false
		b.Realtime = openAIRealtimePolicy(e.Action, b.Framing)
	}
	return b, err
}

func openAIRealtimePolicy(action string, framing core.Framing) *core.RealtimePolicy {
	switch {
	case framing == "websocket" && action == "realtime.connect":
		// Standard Realtime WebSocket events use JSON envelopes with
		// base64-encoded audio, not opaque binary frames.
		return &core.RealtimePolicy{
			Protocol:                  "openai",
			AllowBinary:               false,
			RequirePayloadEnforcement: true,
		}
	case framing == "text/sdp" &&
		(action == "realtime.calls.create" || action == "realtime.translations.calls.create"):
		return &core.RealtimePolicy{
			Protocol:     "openai",
			DirectWebRTC: true,
		}
	default:
		// Responses WebSocket mode and translation WebSocket sessions have
		// distinct event validators and are not accepted by realtime.Machine.
		return nil
	}
}

// Inspect enforces the operator's operation manifest and rejects cache
// directives that belong to OpenRouter/Anthropic wire contracts before a
// direct OpenAI request can reach the paid upstream.
func (c *Connector) Inspect(ctx context.Context, target core.Target, op core.Operation, body []byte) error {
	if err := c.Connector.Inspect(ctx, target, op, nil); err != nil {
		return err
	}
	return rejectForeignCacheDirectives(body)
}

func rejectForeignCacheDirectives(body []byte) error {
	var root map[string]json.RawMessage
	if len(body) == 0 || json.Unmarshal(body, &root) != nil || root == nil {
		return nil
	}
	if _, ok := root["session_id"]; ok {
		return unsupportedCacheDirective("session_id")
	}
	if _, ok := root["cache_control"]; ok {
		return unsupportedCacheDirective("cache_control")
	}
	for _, key := range []string{"messages", "input"} {
		var items []json.RawMessage
		if json.Unmarshal(root[key], &items) != nil {
			continue
		}
		for _, item := range items {
			var object map[string]json.RawMessage
			if json.Unmarshal(item, &object) != nil {
				continue
			}
			if err := rejectContentCacheDirectives(object["content"]); err != nil {
				return err
			}
		}
	}
	var tools []json.RawMessage
	if json.Unmarshal(root["tools"], &tools) == nil {
		for _, tool := range tools {
			var object map[string]json.RawMessage
			if json.Unmarshal(tool, &object) != nil {
				continue
			}
			if _, ok := object["cache_control"]; ok {
				return unsupportedCacheDirective("cache_control")
			}
		}
	}
	return nil
}

func rejectContentCacheDirectives(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) == nil && object != nil {
		if _, ok := object["cache_control"]; ok {
			return unsupportedCacheDirective("cache_control")
		}
		return nil
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return nil
	}
	for _, part := range parts {
		if err := rejectContentCacheDirectives(part); err != nil {
			return err
		}
	}
	return nil
}

func unsupportedCacheDirective(param string) error {
	return core.GatewayError{
		Code:       "unsupported_feature",
		HTTPStatus: 400,
		Param:      param,
		Message:    "cache directive is not supported by the direct OpenAI provider: " + param,
		Origin:     "gateway",
	}
}

func unsupported(message string) error {
	return core.GatewayError{Code: "unsupported_operation", HTTPStatus: 400, Message: message, Origin: "gateway"}
}

var _ core.Connector = (*Connector)(nil)
var _ core.EndpointInventory = (*Connector)(nil)
var _ core.EndpointBinder = (*Connector)(nil)
var _ core.Discoverer = (*Connector)(nil)
var _ core.ScopeInspector = (*Connector)(nil)
