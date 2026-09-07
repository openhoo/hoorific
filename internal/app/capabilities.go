package app

import (
	"context"
	"encoding/json"

	"hoorific/internal/admin"
	"hoorific/internal/core"
)

// connectionCapabilities inspects the configured connector instance and inventory.
// It may construct a pooled transport, but never discovers models, leases credentials or dials upstream.
func (a *Actions) connectionCapabilities(ctx context.Context, p core.Principal, id string) (json.RawMessage, error) {
	connection, resource, err := a.connection(ctx, p, id)
	if err != nil {
		return nil, err
	}
	connector, err := a.connector(connection)
	if err != nil {
		return nil, err
	}
	descriptor := connector.Descriptor()
	if configured, ok := connector.(core.ConnectionDescriptor); ok {
		descriptor, err = configured.DescriptorFor(connection)
		if err != nil {
			return nil, err
		}
	}
	var endpoints []core.NativeEndpoint
	if inventory, ok := connector.(core.ConnectionInventory); ok {
		endpoints, err = inventory.EndpointsFor(connection)
		if err != nil {
			return nil, err
		}
	} else if inventory, ok := connector.(core.EndpointInventory); ok {
		endpoints = inventory.Endpoints()
	}
	out := admin.ConnectionCapabilitiesData{
		ConnectionID: connection.ID, Version: resource.Version,
		Descriptor: admin.ConnectorDescriptorData{ID: descriptor.ID, Protocols: append([]core.Protocol{}, descriptor.Protocols...), Operations: append([]core.Operation{}, descriptor.Operations...), Subscription: descriptor.Subscription},
		Endpoints:  []admin.NativeEndpointData{},
	}
	for _, endpoint := range endpoints {
		out.Endpoints = append(out.Endpoints, admin.NativeEndpointData{Method: endpoint.Method, Path: endpoint.Path, Action: endpoint.Action, Operation: endpoint.Operation, ModelLocation: endpoint.ModelLocation, Framing: endpoint.Framing, Stateful: endpoint.Stateful, ResourceIDField: endpoint.ResourceIDField})
	}
	return json.Marshal(out)
}
