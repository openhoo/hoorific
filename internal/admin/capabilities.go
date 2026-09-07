package admin

import (
	"context"
	"encoding/json"

	"github.com/danielgtaylor/huma/v2"
	"hoorific/internal/core"
)

type ConnectorDescriptorData struct {
	ID           string           `json:"id"`
	Protocols    []core.Protocol  `json:"protocols"`
	Operations   []core.Operation `json:"operations"`
	Subscription bool             `json:"subscription"`
}
type NativeEndpointData struct {
	Method          string         `json:"method"`
	Path            string         `json:"path"`
	Action          string         `json:"action"`
	Operation       core.Operation `json:"operation"`
	ModelLocation   string         `json:"model_location"`
	Framing         core.Framing   `json:"framing"`
	Stateful        bool           `json:"stateful"`
	ResourceIDField string         `json:"resource_id_field,omitempty"`
}
type ConnectionCapabilitiesData struct {
	ConnectionID string                  `json:"connection_id"`
	Version      int64                   `json:"version"`
	Descriptor   ConnectorDescriptorData `json:"descriptor"`
	Endpoints    []NativeEndpointData    `json:"endpoints"`
}
type connectionCapabilitiesOutput struct{ Body ConnectionCapabilitiesData }

func (s *Server) registerConnectionCapabilities(api huma.API) {
	huma.Register(api, huma.Operation{OperationID: "connectionCapabilities", Method: "GET", Path: "/admin/api/v1/connections/{id}/capabilities"}, func(ctx context.Context, in *typedResourceGet) (*connectionCapabilitiesOutput, error) {
		p, e := s.runtimePrincipalWith(ctx, "connection:read")
		if e != nil {
			return nil, e
		}
		if s.deps.Actions == nil {
			return nil, huma.NewError(503, "capability inventory unavailable")
		}
		raw, e := s.deps.Actions.Execute(ctx, p, "connections", in.ID, "capabilities", 0, nil)
		if e != nil {
			return nil, runtimeRepositoryError(e)
		}
		var out ConnectionCapabilitiesData
		if e = json.Unmarshal(raw, &out); e != nil {
			return nil, e
		}
		return &connectionCapabilitiesOutput{Body: out}, nil
	})
}
