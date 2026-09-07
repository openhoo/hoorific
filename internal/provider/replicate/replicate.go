// Package replicate implements Replicate's native prediction and discovery boundaries.
package replicate

import (
	"context"
	"strings"

	"hoorific/internal/core"
	"hoorific/internal/provider/endpoint"
)

const (
	Protocol         core.Protocol  = "replicate"
	PredictionCreate core.Operation = "prediction.create"
	PredictionGet    core.Operation = "prediction.get"
	PredictionList   core.Operation = "prediction.list"
	PredictionCancel core.Operation = "prediction.cancel"
	ModelGet         core.Operation = "model.get"
	ModelList        core.Operation = "model.list"
	ModelCreate      core.Operation = "model.create"
	ModelVersionGet  core.Operation = "model.version.get"
	ModelVersionList core.Operation = "model.version.list"
	DeploymentGet    core.Operation = "deployment.get"
	DeploymentList   core.Operation = "deployment.list"
	DeploymentCreate core.Operation = "deployment.create"
)

const DefaultBaseURL = "https://api.replicate.com"

type Connector struct{ *endpoint.Connector }

// New accepts no implicit HTTP client or credential source. Callers that need
// discovery must pass endpoint.WithDiscovery with both dependencies explicitly.
func New(opts ...endpoint.Option) *Connector {
	// Put the default first so an explicitly supplied WithDiscoveryPath can
	// intentionally select another documented catalog endpoint.
	all := append([]endpoint.Option{endpoint.WithDiscoveryPath("v1/models")}, opts...)
	return &Connector{endpoint.New("replicate", DefaultBaseURL, inventory(), all...)}
}

// Paths and actions are from https://replicate.com/docs/reference/http.
// Streaming is deliberately absent: it uses urls.stream returned by a prediction,
// not an invented relative predictions/{id}/stream endpoint.
func inventory() []endpoint.Route {
	routes := []endpoint.Route{
		endpoint.E("POST", "v1/predictions", "predictions.create", PredictionCreate, Protocol, "json", "body.version", true),
		endpoint.E("POST", "v1/models/{owner}/{name}/predictions", "models.predictions.create", PredictionCreate, Protocol, "json", "path", true),
		endpoint.E("POST", "v1/deployments/{owner}/{name}/predictions", "deployments.predictions.create", PredictionCreate, Protocol, "json", "none", true),
		endpoint.E("GET", "v1/predictions/{id}", "predictions.get", PredictionGet, Protocol, "json", "none", true),
		endpoint.E("GET", "v1/predictions", "predictions.list", PredictionList, Protocol, "json", "none", true),
		endpoint.E("POST", "v1/predictions/{id}/cancel", "predictions.cancel", PredictionCancel, Protocol, "json", "none", true),
		endpoint.E("POST", "v1/models", "models.create", ModelCreate, Protocol, "json", "body", true),
		endpoint.E("GET", "v1/models", "models.list", ModelList, Protocol, "json", "none", false),
		endpoint.E("GET", "v1/models/{owner}/{name}", "models.get", ModelGet, Protocol, "json", "path", false),
		endpoint.E("GET", "v1/models/{owner}/{name}/versions", "models.versions.list", ModelVersionList, Protocol, "json", "path", false),
		endpoint.E("GET", "v1/models/{owner}/{name}/versions/{version}", "models.versions.get", ModelVersionGet, Protocol, "json", "path", false),
		endpoint.E("POST", "v1/deployments", "deployments.create", DeploymentCreate, Protocol, "json", "body", true),
		endpoint.E("GET", "v1/deployments", "deployments.list", DeploymentList, Protocol, "json", "none", false),
		endpoint.E("GET", "v1/deployments/{owner}/{name}", "deployments.get", DeploymentGet, Protocol, "json", "path", false),
	}
	for i := range routes {
		routes[i].Variant = "native"
		routes[i].Response = responsePolicy(routes[i].Action)
	}
	return routes
}

func responsePolicy(action string) core.NativeResponsePolicy {
	switch action {
	case "predictions.create", "models.predictions.create", "deployments.predictions.create":
		return core.NativeResponsePolicy{
			IDField: "id", StatusField: "status",
			PollAction: "predictions.get", PollOperation: PredictionGet,
			PollEndpoint: "v1/predictions/{id}", PollMethod: "GET",
			Async:               true,
			ContinuationFields:  []string{"urls.get", "urls.cancel", "urls.stream"},
			ContinuationMethods: map[string]string{"urls.get": "GET", "urls.cancel": "POST", "urls.stream": "GET"},
			ContinuationActions: map[string]string{"urls.get": "predictions.get", "urls.cancel": "predictions.cancel", "urls.stream": "stream.open"},
			ContinuationOrigins: []string{"https://api.replicate.com", "https://stream.replicate.com", "https://streaming.api.replicate.com"},
			TerminalStatuses:    []string{"succeeded", "failed", "canceled"},
			FailureStatuses:     []string{"failed", "canceled"},
		}
	case "predictions.get":
		return core.NativeResponsePolicy{
			IDField: "id", StatusField: "status",
			ContinuationFields:  []string{"urls.get", "urls.cancel", "urls.stream"},
			ContinuationMethods: map[string]string{"urls.get": "GET", "urls.cancel": "POST", "urls.stream": "GET"},
			ContinuationActions: map[string]string{"urls.get": "predictions.get", "urls.cancel": "predictions.cancel", "urls.stream": "stream.open"},
			ContinuationOrigins: []string{"https://api.replicate.com", "https://stream.replicate.com", "https://streaming.api.replicate.com"},
			TerminalStatuses:    []string{"succeeded", "failed", "canceled"},
			FailureStatuses:     []string{"failed", "canceled"},
		}
	case "predictions.cancel":
		return core.NativeResponsePolicy{IDField: "id", StatusField: "status"}
	default:
		return core.NativeResponsePolicy{}
	}
}

func invalid(message string) error {
	return core.GatewayError{Code: "unsupported_operation", HTTPStatus: 400, Message: message, Origin: "gateway"}
}

func modelParts(id string) (owner, name, version string, err error) {
	model := id
	if i := strings.IndexByte(id, ':'); i >= 0 {
		model, version = id[:i], id[i+1:]
		if !validVersion(version) {
			return "", "", "", invalid("Invalid Replicate version ID")
		}
	}
	parts := strings.Split(model, "/")
	if len(parts) != 2 {
		return "", "", "", invalid("Replicate model must be owner/name[:version]")
	}
	for _, part := range parts {
		if part == "" {
			return "", "", "", invalid("Empty Replicate identifier")
		}
		if _, e := endpoint.Segment(part); e != nil {
			return "", "", "", e
		}
	}
	return parts[0], parts[1], version, nil
}

func validVersion(v string) bool {
	if len(v) != 64 {
		return false
	}
	for _, r := range v {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
func approved(t core.ModelCall, op core.Operation) bool {
	for _, candidate := range t.Model.Operations {
		if candidate == op {
			return true
		}
	}
	return false
}

func (c *Connector) Bind(ctx context.Context, target core.Target, op core.Operation) (core.Binding, error) {
	var conn core.Connection
	params := map[string]string{}
	var action string
	switch t := target.(type) {
	case core.ModelCall:
		conn = t.Connection
		if op != PredictionCreate || !approved(t, op) {
			return core.Binding{}, invalid("Model does not approve Replicate prediction creation")
		}
		if validVersion(t.Model.ID) {
			action = "predictions.create"
		} else {
			owner, name, version, err := modelParts(t.Model.ID)
			if err != nil {
				return core.Binding{}, err
			}
			params["owner"], params["name"] = owner, name
			action = "models.predictions.create"
			if version != "" {
				action = "predictions.create"
			}
		}
	case core.ConnectionResourceCall:
		conn, action = t.Connection, t.Action
		params["id"] = t.ResourceID
		switch action {
		case "models.predictions.create", "deployments.predictions.create", "models.get", "models.versions.list", "models.versions.get", "deployments.get":
			owner, name, version, err := modelParts(t.ResourceID)
			if err != nil {
				return core.Binding{}, err
			}
			if action == "models.versions.get" && version == "" {
				return core.Binding{}, invalid("Version is required")
			}
			if action != "models.versions.get" && version != "" {
				return core.Binding{}, invalid("This endpoint does not select a version")
			}
			params["owner"], params["name"], params["version"] = owner, name, version
		}
	default:
		return core.Binding{}, invalid("Unknown Replicate target")
	}
	for _, e := range c.Endpoints() {
		if e.Action == action && e.Operation == op {
			return c.BindEndpoint(ctx, conn, e, params)
		}
	}
	return core.Binding{}, invalid("Unknown Replicate operation or action")
}

var _ core.Connector = (*Connector)(nil)
var _ core.ScopeInspector = (*Connector)(nil)
var _ core.Discoverer = (*Connector)(nil)
