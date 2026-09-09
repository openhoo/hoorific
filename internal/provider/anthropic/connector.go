// Package anthropic describes the official Claude Messages, token counting,
// Files, Message Batches, and Models APIs. It does not dispatch requests.
package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"hoorific/internal/core"
	"hoorific/internal/provider/endpoint"
	"hoorific/internal/transport"
)

const (
	DefaultBaseURL = "https://api.anthropic.com/v1"
	APIVersion     = "2023-06-01"
	// FilesBeta is the legacy opt-in. Current official Files endpoint examples
	// no longer require it; do not enable beta features for every request.
	FilesBeta = "files-api-2025-04-14"
)

// Connector preserves native request schemas instead of applying the shared
// inspector's OpenAI-shaped feature-field heuristics.
type Connector struct {
	*endpoint.Connector
}

// Discover follows the official Models cursor contract without treating
// provider availability as operator approval of any model operation.
// Source: https://platform.claude.com/docs/en/api/models/list
func (c *Connector) Discover(ctx context.Context, conn core.Connection) ([]core.Model, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := conn.ClientProfile.Validate(conn.Connector); err != nil {
		return nil, err
	}
	client := c.DiscoveryClient()
	if client == nil {
		return nil, core.GatewayError{Code: "configuration_stale", HTTPStatus: 400, Message: "Anthropic discovery requires an injected HTTP client", Origin: "gateway"}
	}
	if conn.BaseURL == "" {
		return nil, core.GatewayError{Code: "configuration_stale", HTTPStatus: 400, Message: "Anthropic discovery requires an explicit connection base URL", Origin: "gateway"}
	}
	base, err := url.Parse(conn.BaseURL)
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, core.GatewayError{Code: "configuration_stale", HTTPStatus: 400, Message: "Invalid Anthropic discovery base URL", Origin: "gateway"}
	}
	source := c.CredentialSource()
	if source == nil {
		return nil, core.GatewayError{Code: "connection_required", HTTPStatus: 400, Message: "Anthropic discovery requires an injected credential source", Origin: "gateway"}
	}
	u := base.JoinPath("models")
	query := url.Values{"limit": []string{"1000"}}
	seenCursors := make(map[string]bool)
	seenModels := make(map[string]bool)
	models := make([]core.Model, 0)
	invalidPage := func() error {
		return core.GatewayError{Code: "upstream_outcome_unknown", HTTPStatus: 502, Message: "Anthropic discovery returned an invalid model page or cursor", Origin: "upstream"}
	}
	for {
		u.RawQuery = query.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		if conn.ClientProfile != nil {
			if err := conn.ClientProfile.Apply(req.Header); err != nil {
				return nil, err
			}
		}
		transport.SanitizeRequest(req)
		lease, err := source.Lease(ctx, conn)
		if err != nil {
			return nil, err
		}
		if lease == nil {
			return nil, core.GatewayError{Code: "connection_required", HTTPStatus: 400, Message: "Anthropic discovery credential lease is unavailable", Origin: "gateway"}
		}
		authErr := (Authenticator{}).Authorize(ctx, lease, req)
		closeErr := core.CloseCredentialLease(lease)
		if authErr != nil {
			return nil, authErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			return nil, core.GatewayError{Code: "upstream_outcome_unknown", HTTPStatus: resp.StatusCode, Message: "Anthropic model discovery failed", Origin: "upstream", Retryable: resp.StatusCode == 429 || resp.StatusCode >= 500}
		}
		// Bound each page, close it before requesting another, and reject
		// trailing JSON rather than accepting a valid prefix of bad data.
		const maxPageBytes = 8 << 20
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes+1))
		resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		var page struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			HasMore *bool  `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		if len(body) > maxPageBytes || json.Unmarshal(body, &page) != nil || page.Data == nil || page.HasMore == nil {
			return nil, invalidPage()
		}
		for _, item := range page.Data {
			if item.ID == "" {
				return nil, invalidPage()
			}
			if !seenModels[item.ID] {
				seenModels[item.ID] = true
				models = append(models, core.Model{ID: item.ID, ConnectionID: conn.ID, Features: map[string]core.Support{}, Provenance: "provider_discovery"})
			}
		}
		if !*page.HasMore {
			return models, nil
		}
		if len(page.Data) == 0 || page.LastID == "" || page.LastID != page.Data[len(page.Data)-1].ID || seenCursors[page.LastID] {
			return nil, invalidPage()
		}
		seenCursors[page.LastID] = true
		query.Set("after_id", page.LastID)
	}
}

// New builds an allowlisted inventory. Paths omit the facade SDK prefix: the
// engine removes that prefix and resolves these paths against DefaultBaseURL.
//
// Discovery uses endpoint.WithDiscovery(client, credentials), requires an
// explicit Connection.BaseURL, applies the API version header on each request,
// and follows all Models API pages. Availability does not approve operations
// or infer model capabilities. Native query propagation, multipart bodies,
// binary bodies, and executor header integration remain shared transport
// concerns, not capabilities implied by this inventory.
func New(opts ...endpoint.Option) *Connector {
	routes := []endpoint.Route{
		// https://platform.claude.com/docs/en/api/messages/create
		// Streaming is the same endpoint with stream:true, not a remote job.
		endpoint.E("POST", "messages", "messages.create", "generate", "anthropic-messages", "json", "body", false),
		endpoint.E("POST", "messages", "messages.stream", "generate", "anthropic-messages", "sse-named", "body", false),
		// https://platform.claude.com/docs/en/api/messages/count_tokens
		endpoint.E("POST", "messages/count_tokens", "messages.count_tokens", "count_tokens", "anthropic-messages", "json", "body", false),

		// https://platform.claude.com/docs/en/api/files/upload
		// The request is multipart/form-data with a generated boundary. The
		// binary framing records that native body; the response is JSON.
		endpoint.E("POST", "files", "files.upload", "file", "native", "binary", "none", true),
		// https://platform.claude.com/docs/en/api/files/list
		endpoint.E("GET", "files", "files.list", "file", "native", "json", "none", true),
		// https://platform.claude.com/docs/en/api/files/retrieve_metadata
		endpoint.E("GET", "files/{file_id}", "files.retrieve_metadata", "file", "native", "json", "none", true),
		// https://platform.claude.com/docs/en/api/files/delete
		endpoint.E("DELETE", "files/{file_id}", "files.delete", "file", "native", "json", "none", true),
		// https://platform.claude.com/docs/en/api/files/download
		// Only downloadable files can be fetched. Binary framing is explicit;
		// the shared transport still needs a native binary codec to consume it.
		endpoint.E("GET", "files/{file_id}/content", "files.download", "file", "native", "binary", "none", true),

		// https://platform.claude.com/docs/en/api/messages/batches/create
		// Models live inside requests[].params, not a top-level model field.
		endpoint.E("POST", "messages/batches", "batches.create", "batch", "native", "json", "body", true),
		// https://platform.claude.com/docs/en/api/messages/batches/list
		endpoint.E("GET", "messages/batches", "batches.list", "batch", "native", "json", "none", true),
		// https://platform.claude.com/docs/en/api/messages/batches/retrieve
		endpoint.E("GET", "messages/batches/{message_batch_id}", "batches.retrieve", "batch", "native", "json", "none", true),
		// https://platform.claude.com/docs/en/api/messages/batches/cancel
		// Cancellation initiates canceling; it does not guarantee that any
		// in-flight request is interrupted. No Messages cancel route exists.
		endpoint.E("POST", "messages/batches/{message_batch_id}/cancel", "batches.cancel", "batch", "native", "json", "none", true),
		// https://platform.claude.com/docs/en/api/messages/batches/delete
		// Upstream permits deletion only after processing has ended.
		endpoint.E("DELETE", "messages/batches/{message_batch_id}", "batches.delete", "batch", "native", "json", "none", true),
		// https://platform.claude.com/docs/en/api/messages/batches/results
		// Results are JSONL, unordered, and matched by custom_id. Use the
		// allowlisted path rather than trusting an arbitrary results_url.
		endpoint.E("GET", "messages/batches/{message_batch_id}/results", "batches.results", "batch", "native", "ndjson", "none", true),

		// https://platform.claude.com/docs/en/api/models/list
		endpoint.E("GET", "models", "models.list", "model.list", "native", "json", "none", false),
		// https://platform.claude.com/docs/en/api/models/retrieve
		// Retrieval also resolves model aliases; this does not approve use.
		endpoint.E("GET", "models/{model_id}", "models.retrieve", "model.list", "native", "json", "path", false),
	}
	for i := range routes {
		// The shared Anthropic conversation codec is registered under the
		// empty variant. Native endpoint codecs use the explicit provider
		// variant so they cannot collide with conversation payloads.
		if routes[i].Protocol == "anthropic-messages" {
			routes[i].Variant = ""
		} else {
			routes[i].Variant = "anthropic-v1"
		}
		switch {
		case strings.Contains(routes[i].Path, "{file_id}"):
			routes[i].ResourceIDField = "file_id"
		case strings.Contains(routes[i].Path, "{message_batch_id}"):
			routes[i].ResourceIDField = "message_batch_id"
		case strings.Contains(routes[i].Path, "{model_id}"):
			routes[i].ResourceIDField = "model_id"
		}
		switch routes[i].Action {
		case "files.upload", "files.retrieve_metadata", "files.delete":
			routes[i].Response = core.NativeResponsePolicy{IDField: "id"}
		case "batches.create", "batches.retrieve", "batches.cancel":
			routes[i].Response = core.NativeResponsePolicy{
				IDField: "id", StatusField: "processing_status",
				PollEndpoint: "messages/batches/{message_batch_id}", PollMethod: "GET",
				PollAction: "batches.retrieve", PollOperation: "batch",
				Async: true,
				// Aggregate batch failure is represented by request_counts
				// and per-request results, not processing_status.
				TerminalStatuses: []string{"ended"},
			}
		case "batches.delete":
			routes[i].Response = core.NativeResponsePolicy{IDField: "id"}
		}
	}

	options := append([]endpoint.Option{endpoint.WithDiscoveryPath("models")}, opts...)
	return &Connector{Connector: endpoint.New("anthropic", DefaultBaseURL, routes, options...)}
}

func unsupported(message string) error {
	return core.GatewayError{Code: "unsupported_operation", HTTPStatus: http.StatusBadRequest, Message: message, Origin: "gateway"}
}

// Bind requires explicit resource actions, including for non-stateful Models.
// A model call selects JSON Messages by default; callers selecting native SSE
// use the messages.stream descriptor through BindEndpoint with stream:true.
func (c *Connector) Bind(ctx context.Context, target core.Target, op core.Operation) (core.Binding, error) {
	if err := ctx.Err(); err != nil {
		return core.Binding{}, err
	}
	switch call := target.(type) {
	case core.ModelCall:
		if op != "generate" && op != "count_tokens" {
			return core.Binding{}, unsupported("Anthropic model calls support generate or count_tokens")
		}
	case core.ConnectionResourceCall:
		if call.Action == "" {
			return core.Binding{}, unsupported("Anthropic resource calls require an explicit endpoint action")
		}
		if op != "file" && op != "batch" && op != "model.list" && op != "generate" && op != "count_tokens" {
			return core.Binding{}, unsupported("Anthropic resource action is not supported")
		}
	default:
		return core.Binding{}, unsupported("Unknown Anthropic target")
	}
	b, err := c.Connector.Bind(ctx, target, op)
	if err != nil {
		return core.Binding{}, err
	}
	if b.Headers == nil {
		b.Headers = make(http.Header)
	}
	b.Headers.Set("anthropic-version", APIVersion)
	b.AllowedRequestHeaders = []string{"anthropic-version", "anthropic-beta", "anthropic-workspace-id", "content-type"}
	return b, nil
}

// BindEndpoint mirrors Bind's required-header metadata for callers that
// select a concrete descriptor, including the Messages streaming descriptor.
// endpoint.Connector cannot add provider headers because its Binding contract
// is provider-neutral; this override keeps direct descriptor binding explicit.
func (c *Connector) BindEndpoint(ctx context.Context, conn core.Connection, e core.NativeEndpoint, params map[string]string) (core.Binding, error) {
	b, err := c.Connector.BindEndpoint(ctx, conn, e, params)
	if err != nil {
		return core.Binding{}, err
	}
	if b.Headers == nil {
		b.Headers = make(http.Header)
	}
	b.Headers.Set("anthropic-version", APIVersion)
	b.AllowedRequestHeaders = []string{"anthropic-version", "anthropic-beta", "anthropic-workspace-id", "content-type"}
	return b, nil
}

// Inspect enforces manifest operation approval without rewriting or guessing
// native tool, thinking, document, beta, or structured-output schemas. Native
// per-feature authorization needs a provider-aware shared policy contract.
func (c *Connector) Inspect(ctx context.Context, target core.Target, op core.Operation, _ []byte) error {
	_, err := c.Bind(ctx, target, op)
	return err
}

// Authenticator supplies the required version header using the frozen core
// authentication extension. Bind also exposes the required version header
// through core.Binding.Headers; the executor must preserve and send it.
// Beta is an explicit comma-separated opt-in, not inferred from model names.
// Credential leases remain responsible for x-api-key or Bearer authorization
// and any required anthropic-workspace-id. Content-Type is left to the body
// encoder so multipart boundaries are preserved.
// Sources:
// https://platform.claude.com/docs/en/api/overview
// https://platform.claude.com/docs/en/api/beta-headers
// https://platform.claude.com/docs/en/build-with-claude/files
// https://platform.claude.com/docs/en/api/models/list
// (the latter lists the legacy files-api-2025-04-14 beta).
type Authenticator struct {
	Beta string
}

func (a Authenticator) Authorize(ctx context.Context, lease core.CredentialLease, req *http.Request) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if lease == nil || req == nil {
		return core.GatewayError{Code: "connection_required", HTTPStatus: http.StatusUnauthorized, Message: "Anthropic authorization requires a credential lease and request", Origin: "gateway"}
	}
	if strings.ContainsAny(a.Beta, "\r\n\x00") {
		return core.GatewayError{Code: "configuration_stale", HTTPStatus: http.StatusBadRequest, Message: "Invalid Anthropic beta header", Origin: "gateway"}
	}
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	if err := lease.Authorize(ctx, req); err != nil {
		return err
	}
	req.Header.Set("anthropic-version", APIVersion)
	if a.Beta != "" {
		if existing := req.Header.Get("anthropic-beta"); existing != "" {
			req.Header.Set("anthropic-beta", existing+","+a.Beta)
		} else {
			req.Header.Set("anthropic-beta", a.Beta)
		}
	}
	return nil
}

var _ core.Connector = (*Connector)(nil)
var _ core.Discoverer = (*Connector)(nil)
var _ core.EndpointInventory = (*Connector)(nil)
var _ core.EndpointBinder = (*Connector)(nil)
var _ core.ScopeInspector = (*Connector)(nil)
var _ core.Authenticator = Authenticator{}
