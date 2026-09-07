package app

import (
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"hoorific/internal/core"
	"hoorific/internal/credential"
	"hoorific/internal/store"
)

// ConfiguredOAuthFactory composes only operator-registered, connection-bound
// OAuth clients. Construction does not perform discovery or other network I/O.
func ConfiguredOAuthFactory(cfg core.BootstrapConfig, keys credential.Keyring, db *store.Store, clients func(core.Connection) (*http.Client, error)) (func(context.Context, core.Connection) (*credential.OAuthClient, error), func(context.Context, core.Connection, *credential.OAuthClient) (*credential.DeviceCoordinator, error)) {
	registrations := maps.Clone(cfg.OAuth)
	for id, registration := range registrations {
		registration.Scopes = append([]string(nil), registration.Scopes...)
		registrations[id] = registration
	}
	oauthFactory := func(ctx context.Context, c core.Connection) (*credential.OAuthClient, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		registration, ok := registrations[c.ID]
		if !ok {
			return nil, actionError("requires_credentials", http.StatusConflict, "An explicit OAuth registration for this connection is required")
		}
		if keys == nil || db == nil || clients == nil {
			return nil, actionError("configuration_stale", http.StatusServiceUnavailable, "OAuth credential storage and safe transport are required")
		}
		if c.ID == "" || c.TenantID == "" || c.Connector == "" || c.AccountID == "" || c.Version <= 0 {
			return nil, actionError("requires_credentials", http.StatusConflict, "OAuth requires a persisted connection with an explicit tenant, connector and account")
		}
		if strings.TrimSpace(registration.ClientID) == "" || registration.AuthorizationURL == "" || registration.TokenURL == "" || registration.RedirectURL == "" {
			return nil, configuredOAuthError("OAuth registration requires a client ID, authorization endpoint, token endpoint and exact redirect URL")
		}
		if _, err := configuredOAuthURL(registration.RedirectURL, c); err != nil {
			return nil, configuredOAuthError("OAuth redirect URL must use HTTPS or explicitly permitted loopback HTTP")
		}
		transport := &configuredOAuthTransport{clients: make(map[string]*http.Client)}
		for _, endpoint := range []string{registration.AuthorizationURL, registration.TokenURL, registration.DeviceURL, registration.Issuer} {
			if endpoint == "" {
				continue
			}
			u, err := configuredOAuthURL(endpoint, c)
			if err != nil {
				return nil, configuredOAuthError("OAuth endpoints must use HTTPS or explicitly permitted loopback HTTP, without credentials or fragments")
			}
			origin := configuredOAuthOrigin(u)
			if transport.clients[origin] != nil {
				continue
			}
			// ClientFactory derives its exact allowed host from BaseURL. Retain
			// the connection's address policy rather than relaxing private access.
			endpointConnection := c
			endpointConnection.BaseURL = origin
			endpointConnection.Settings = maps.Clone(c.Settings)
			if endpointConnection.Settings == nil {
				endpointConnection.Settings = make(map[string]string)
			}
			endpointConnection.Settings["allow_same_origin_redirect"] = "false"
			client, err := clients(endpointConnection)
			if err != nil || client == nil || client.Transport == nil {
				return nil, configuredOAuthError("OAuth safe endpoint transport could not be configured")
			}
			transport.clients[origin] = client
		}
		if registration.Issuer != "" {
			u, _ := url.Parse(registration.Issuer)
			if u.RawQuery != "" || u.ForceQuery {
				return nil, configuredOAuthError("OAuth issuer must not contain a query")
			}
		}
		secret, err := configuredOAuthSecret(registration.ClientSecretFile)
		if err != nil {
			return nil, err
		}
		client := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		validate := func(ctx context.Context, raw, nonce string) (credential.AccountEvidence, error) {
			if registration.Issuer == "" {
				return credential.AccountEvidence{}, errors.New("ID token verification requires an explicitly configured issuer")
			}
			// Discovery and JWKS retrieval use the same restricted transport.
			// A discovered JWKS origin is not automatically trusted.
			ctx = oidc.ClientContext(ctx, client)
			provider, err := oidc.NewProvider(ctx, registration.Issuer)
			if err != nil {
				return credential.AccountEvidence{}, errors.New("OIDC issuer discovery failed")
			}
			token, err := provider.Verifier(&oidc.Config{ClientID: registration.ClientID}).Verify(ctx, raw)
			if err != nil {
				return credential.AccountEvidence{}, errors.New("OIDC ID token verification failed")
			}
			if token.Subject == "" || token.Subject != c.AccountID {
				return credential.AccountEvidence{}, errors.New("OIDC subject does not match the configured account")
			}
			// Device flow has no nonce; code flow always supplies its stored nonce.
			if nonce != "" && token.Nonce != nonce {
				return credential.AccountEvidence{}, errors.New("OIDC nonce mismatch")
			}
			return credential.AccountEvidence{Provider: c.Connector, AccountID: c.AccountID, Issuer: registration.Issuer, Subject: token.Subject, Method: "oidc-id-token"}, nil
		}
		return credential.NewOAuthClient(credential.OAuthConfig{
			Connector: c.Connector, ClientID: registration.ClientID, ClientSecret: secret,
			AuthorizationURL: registration.AuthorizationURL, TokenURL: registration.TokenURL,
			DeviceURL: registration.DeviceURL, RedirectURI: registration.RedirectURL,
			Scopes: append([]string(nil), registration.Scopes...), HTTPClient: client,
			Keyring: keys, ValidateIDToken: validate,
		}, db)
	}
	deviceFactory := func(ctx context.Context, c core.Connection, client *credential.OAuthClient) (*credential.DeviceCoordinator, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		registration, ok := registrations[c.ID]
		if !ok || registration.DeviceURL == "" {
			return nil, actionError("requires_credentials", http.StatusConflict, "An explicit device authorization endpoint for this connection is required")
		}
		if client == nil || client.Config.Connector != c.Connector || client.Config.ClientID != registration.ClientID || client.Config.DeviceURL != registration.DeviceURL || client.Config.TokenURL != registration.TokenURL || client.Config.ValidateIDToken == nil || c.AccountID == "" || c.Version <= 0 || db == nil || keys == nil {
			return nil, configuredOAuthError("Device authorization requires the configured OAuth client and current connection identity")
		}
		// Actions supplies c.Version to Start and checks it against the durable
		// session before Poll. No in-process device session map is necessary.
		return credential.NewDeviceCoordinator(client, db, keys)
	}
	return oauthFactory, deviceFactory
}

func configuredOAuthError(message string) error {
	return actionError("invalid_configuration", http.StatusBadRequest, message)
}

func configuredOAuthSecret(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	file, err := os.Open(path)
	if err != nil {
		return "", configuredOAuthError("OAuth client secret file could not be opened")
	}
	defer file.Close()
	const limit = 64 << 10
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return "", configuredOAuthError("OAuth client secret must be a regular file no larger than 64 KiB")
	}
	b, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || len(b) > limit {
		return "", configuredOAuthError("OAuth client secret file could not be read within its size limit")
	}
	secret := strings.TrimSpace(string(b))
	if secret == "" || strings.ContainsAny(secret, "\x00\r\n") {
		return "", configuredOAuthError("OAuth client secret file contains an empty or invalid secret")
	}
	return secret, nil
}

func configuredOAuthURL(raw string, c core.Connection) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("invalid OAuth URL")
	}
	if u.Scheme == "https" {
		return u, nil
	}
	ip, err := netip.ParseAddr(u.Hostname())
	if u.Scheme != "http" || err != nil || !ip.IsLoopback() || c.Settings["allow_private"] != "true" {
		return nil, errors.New("OAuth requires HTTPS")
	}
	for _, rawPrefix := range strings.Split(c.Settings["allowed_cidrs"], ",") {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(rawPrefix))
		if err == nil && prefix.Contains(ip) {
			return u, nil
		}
	}
	return nil, errors.New("OAuth loopback address is not explicitly permitted")
}

func configuredOAuthOrigin(u *url.URL) string {
	return u.Scheme + "://" + strings.ToLower(u.Host)
}

// The map contains only immutable transport clients, never login/session state.
// Origins are stricter than a hostname allowlist and cannot expand on discovery.
type configuredOAuthTransport struct {
	clients map[string]*http.Client
}

func (t *configuredOAuthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL == nil || r.URL.User != nil || r.URL.Fragment != "" {
		return nil, errors.New("OAuth request URL denied")
	}
	client := t.clients[configuredOAuthOrigin(r.URL)]
	if client == nil {
		return nil, errors.New("OAuth request origin is not explicitly configured")
	}
	return client.Transport.RoundTrip(r)
}
