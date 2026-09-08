// Package cohere inventories the Cohere inference and embedding-job APIs.
// Sources: https://docs.cohere.com/reference/chat (v2 chat),
// https://docs.cohere.com/reference/embed, https://docs.cohere.com/reference/rerank,
// https://docs.cohere.com/reference/list-models and
// https://docs.cohere.com/reference/create-embed-job (including list/get/cancel).
package cohere

import (
	"hoorific/internal/core"
	"hoorific/internal/provider/endpoint"
)

const DefaultBaseURL = "https://api.cohere.com"

func New(opts ...endpoint.Option) *endpoint.Connector {
	routes := []endpoint.Route{
		endpoint.E("POST", "v2/chat", "chat.create", "generate", "cohere-v2", "sse-data", "body", false),
		endpoint.E("POST", "v2/embed", "embed.create", "embed", "cohere-v2", "json", "body", false),
		endpoint.E("POST", "v2/rerank", "rerank.create", "rerank", "cohere-v2", "json", "body", false),
		endpoint.E("GET", "v1/models", "models.list", "model.list", "cohere-v1", "json", "none", false),
		endpoint.E("GET", "v1/models/{id}", "models.retrieve", "model.list", "cohere-v1", "json", "none", false),
		endpoint.E("POST", "v1/embed-jobs", "embed_jobs.create", "batch", "cohere-v1", "json", "body", true),
		endpoint.E("GET", "v1/embed-jobs", "embed_jobs.list", "batch", "cohere-v1", "json", "none", true),
		endpoint.E("GET", "v1/embed-jobs/{id}", "embed_jobs.retrieve", "batch", "cohere-v1", "json", "none", true),
		endpoint.E("POST", "v1/embed-jobs/{id}/cancel", "embed_jobs.cancel", "batch", "cohere-v1", "json", "none", true),
	}
	routes[4].ResourceIDField = "id"
	routes[7].ResourceIDField = "id"
	routes[8].ResourceIDField = "id"
	routes[5].Response = core.NativeResponsePolicy{
		IDField: "job_id", StatusField: "status", Async: true,
		ResultAction: "embed_jobs.retrieve", CancelAction: "embed_jobs.cancel",
		PollEndpoint: "v1/embed-jobs/{id}", PollMethod: "GET", PollAction: "embed_jobs.retrieve", PollOperation: "batch", UsageField: "meta.billed_units",
		TerminalStatuses: []string{"complete", "cancelled"},
		FailureStatuses:  []string{"failed"},
	}
	routes[7].Response = core.NativeResponsePolicy{
		IDField: "job_id", StatusField: "status",
		ResultAction: "embed_jobs.retrieve", CancelAction: "embed_jobs.cancel",
		TerminalStatuses: []string{"complete", "cancelled"},
		FailureStatuses:  []string{"failed"},
	}
	routes[8].DefaultBody = "{}"
	// Cohere's documented cancel response is an empty JSON object. Its
	// descriptor deliberately carries no status field so job actions can treat
	// that object as the cancellation receipt without inventing an ID/usage.
	routes[8].Response = core.NativeResponsePolicy{}
	return endpoint.New("cohere", DefaultBaseURL, routes, append([]endpoint.Option{endpoint.WithDiscoveryPath("v1/models")}, opts...)...)
}
