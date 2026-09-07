package app

import (
	"context"
	"hoorific/internal/core"
	"hoorific/internal/credential"
)

// oauthClient resolves only an explicitly composed provider registration. It
// never derives client IDs, scopes, or endpoints from connector names.
func (a *Actions) oauthClient(ctx context.Context, c core.Connection) (*credential.OAuthClient, error) {
	if x := a.deps.OAuthClients[c.Connector]; x != nil {
		return x, nil
	}
	if a.deps.OAuthClientFactory == nil {
		return nil, credentialUnavailable()
	}
	x, e := a.deps.OAuthClientFactory(ctx, c)
	if e != nil {
		return nil, credentialActionFailure(e)
	}
	if x == nil || x.Config.Connector != c.Connector || x.Config.Keyring == nil {
		return nil, credentialUnavailable()
	}
	return x, nil
}
func (a *Actions) deviceCoordinator(ctx context.Context, c core.Connection, x *credential.OAuthClient) (*credential.DeviceCoordinator, error) {
	if d := a.deps.DeviceCoordinators[c.Connector]; d != nil {
		return d, nil
	}
	if a.deps.DeviceCoordinatorFactory == nil {
		return nil, credentialUnavailable()
	}
	d, e := a.deps.DeviceCoordinatorFactory(ctx, c, x)
	if e != nil {
		return nil, credentialActionFailure(e)
	}
	if d == nil || d.Client == nil || d.Repo == nil || d.Keys == nil || d.Client.Config.Connector != c.Connector {
		return nil, credentialUnavailable()
	}
	return d, nil
}
