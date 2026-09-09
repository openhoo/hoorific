package app

import (
	"context"
	"encoding/json"
	"hoorific/internal/core"
	"hoorific/internal/credential"
	"hoorific/internal/store"
	"net/http"
)

// ActionDependencies supplies the same trusted transports and identity sources as inference.
type ActionDependencies struct {
	Store                    *store.Store
	Connectors               map[string]core.Connector
	Credentials              core.CredentialSource
	CredentialManager        *credential.Manager
	Client                   func(core.Connection) (*http.Client, error)
	SubscriptionEnabled      bool
	OAuthClients             map[string]*credential.OAuthClient
	OAuthClientFactory       func(context.Context, core.Connection) (*credential.OAuthClient, error)
	DeviceCoordinators       map[string]*credential.DeviceCoordinator
	DeviceCoordinatorFactory func(context.Context, core.Connection, *credential.OAuthClient) (*credential.DeviceCoordinator, error)
	VerifyCredentialAccount  func(context.Context, core.Connection, credential.Secret) error
}
type Actions struct{ deps ActionDependencies }

func NewActions(d ActionDependencies) *Actions { return &Actions{deps: d} }
func actionError(code string, status int, message string) error {
	return core.GatewayError{Code: code, HTTPStatus: status, Message: message, Origin: "gateway"}
}
func (a *Actions) Execute(ctx context.Context, p core.Principal, kind, id, action string, version int64, data json.RawMessage) (json.RawMessage, error) {
	if a == nil || a.deps.Store == nil {
		return nil, actionError("configuration_stale", 503, "Administrative storage unavailable")
	}
	if p.TenantID == "" || p.SubjectID == "" {
		return nil, actionError("unauthorized", 401, "Administrative principal required")
	}
	switch kind {
	case "connections":
		switch action {
		case "capabilities":
			return a.connectionCapabilities(ctx, p, id)
		case "test", "discover", "disable":
			return a.connectionAction(ctx, p, id, action, version, data)
		case "import", "status", "revoke-credential", "oauth-start", "oauth-callback", "device-start", "device-poll":
			return a.credentialActions(ctx, p, id, action, version, data)
		}
	case "credentials":
		return a.credentialActions(ctx, p, id, action, version, data)
	case "route_policies":
		if action == "dry-run" {
			return a.routeAction(ctx, p, id, data)
		}
	case "upstream_operations":
		return a.jobAction(ctx, p, id, action, version, data)
	case "config":
		return a.configAction(ctx, p, action, version, data)
	case "catalog":
		if action == "list" {
			snap, e := a.deps.Store.Snapshot(core.WithPrincipal(ctx, p))
			if e != nil {
				return nil, e
			}
			return json.Marshal(snap)
		}
	case "api_keys":
		if action == "issue" || action == "rotate" || action == "revoke" {
			return a.deps.Store.Execute(ctx, p, kind, id, action, version, data)
		}
	}
	return nil, actionError("unsupported_operation", 404, "Administrative action is not supported")
}
func (a *Actions) Reconcile(ctx context.Context, p core.Principal, id string, version int64, r core.Reconciliation) (core.Resource, error) {
	if a == nil || a.deps.Store == nil {
		return core.Resource{}, actionError("configuration_stale", 503, "Administrative storage unavailable")
	}
	return a.deps.Store.Reconcile(ctx, p, id, version, r)
}
func (a *Actions) connection(ctx context.Context, p core.Principal, id string) (core.Connection, core.Resource, error) {
	r, e := a.deps.Store.Get(ctx, p, "connections", id)
	if e != nil {
		return core.Connection{}, r, e
	}
	snap, e := a.deps.Store.Snapshot(core.WithPrincipal(ctx, p))
	if e != nil {
		return core.Connection{}, r, e
	}
	c, ok := snap.Connections[id]
	if !ok || c.ID != id || c.TenantID != p.TenantID {
		return core.Connection{}, r, actionError("configuration_stale", 503, "Stored connection identity is invalid")
	}
	c.Version = r.Version
	return c, r, nil
}
func (a *Actions) connector(c core.Connection) (core.Connector, error) {
	x := a.deps.Connectors[c.Connector]
	if x == nil {
		return nil, actionError("unsupported_operation", 400, "Connector is not registered")
	}
	if c.Settings["disabled"] == "true" {
		return nil, actionError("connection_disabled", 409, "Connection is disabled")
	}
	if x.Descriptor().Subscription {
		if !a.deps.SubscriptionEnabled {
			return nil, actionError("subscription_disabled", 403, "Subscription integrations are disabled")
		}
		if c.AccountID == "" || c.Settings["consent_ack"] != "true" || c.Settings["consent_tenant"] != c.TenantID || c.Settings["consent_account"] != c.AccountID || c.Settings["consent_provider"] != c.Connector {
			return nil, actionError("consent_required", 403, "Owner consent for this tenant, provider and account is required")
		}
	}
	if c.ClientProfile != nil {
		if err := c.ClientProfile.Validate(c.Connector); err != nil {
			return nil, err
		}
	}
	return x, nil
}
