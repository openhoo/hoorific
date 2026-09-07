package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"hoorific/internal/core"
	"hoorific/internal/credential"
)

type credentialActionInput struct {
	Provider          string    `json:"provider,omitempty"`
	AccountID         string    `json:"account_id,omitempty"`
	CredentialVersion int64     `json:"credential_version,omitempty"`
	Kind              string    `json:"kind,omitempty"`
	Secret            string    `json:"secret,omitempty"`
	AccessToken       string    `json:"access_token,omitempty"`
	RefreshToken      string    `json:"refresh_token,omitempty"`
	TokenType         string    `json:"token_type,omitempty"`
	ExpiresAt         time.Time `json:"expires_at,omitempty"`
	Scopes            []string  `json:"scopes,omitempty"`
	State             string    `json:"state,omitempty"`
	Code              string    `json:"code,omitempty"`
	RedirectURI       string    `json:"redirect_uri,omitempty"`
	FlowID            string    `json:"flow_id,omitempty"`
}

func (a *Actions) credentialActions(ctx context.Context, p core.Principal, id, action string, version int64, data json.RawMessage) (json.RawMessage, error) {
	switch action {
	case "import", "status", "revoke-credential", "oauth-start", "oauth-callback", "device-start", "device-poll":
	default:
		return nil, actionError("unsupported_operation", 404, "Credential action is not supported")
	}
	c, _, err := a.connection(ctx, p, id)
	if err != nil {
		return nil, err
	}
	var x core.Connector
	m := a.deps.CredentialManager
	if m == nil {
		return nil, credentialUnavailable()
	}
	if action == "status" {
		meta, err := m.Metadata(ctx, c.TenantID, c.ID)
		if errors.Is(err, sql.ErrNoRows) {
			return json.Marshal(map[string]any{"status": "requires_credentials", "provider": c.Connector, "account_id": c.AccountID})
		}
		if err != nil {
			return nil, credentialActionFailure(err)
		}
		if meta.Provider != c.Connector || meta.AccountID != c.AccountID {
			return nil, actionError("credential_identity_mismatch", 409, "Stored credential does not match the connection")
		}
		return credentialStatus(meta)
	}
	if version != c.Version {
		return nil, actionError("version_conflict", 409, "Connection revision changed")
	}
	var in credentialActionInput
	if len(data) == 0 {
		data = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) || decoder.Decode(&in) != nil {
		return nil, actionError("invalid_request", 400, "Invalid credential action data")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || in.CredentialVersion < 0 {
		return nil, actionError("invalid_request", 400, "Invalid credential action data")
	}
	if in.Provider == "" || in.AccountID == "" || in.Provider != c.Connector || in.AccountID != c.AccountID {
		return nil, actionError("credential_identity_mismatch", 400, "Provider and account must match the connection")
	}
	identity := credential.Identity{TenantID: c.TenantID, ConnectionID: c.ID, CredentialID: c.ID, AccountID: c.AccountID, Provider: c.Connector, Version: in.CredentialVersion}
	if action == "revoke-credential" {
		if in.CredentialVersion < 1 || in.Secret != "" || in.AccessToken != "" || in.RefreshToken != "" {
			return nil, actionError("invalid_request", 400, "Revocation requires the current credential version, not secret material")
		}
		record, err := m.Store.LoadCredential(ctx, c.TenantID, c.ID)
		if err != nil {
			return nil, credentialActionFailure(err)
		}
		if record.Identity.Provider != c.Connector || record.Identity.AccountID != c.AccountID || record.Identity.Version != in.CredentialVersion {
			return nil, actionError("version_conflict", 409, "Credential changed before revocation")
		}
		if record.Identity.Version == int64(^uint64(0)>>1) {
			return nil, actionError("version_conflict", 409, "Credential version exhausted")
		}
		secret, err := credential.Open(m.Keys, record.Identity, record.Envelope)
		if err != nil {
			return nil, credentialActionFailure(err)
		}
		record.Identity.Version++
		record.Envelope, err = credential.Seal(m.Keys, record.Identity, secret)
		if err != nil {
			return nil, credentialActionFailure(err)
		}
		record.Status = "revoked"
		if err := m.Store.PutCredential(ctx, record, in.CredentialVersion); err != nil {
			return nil, credentialActionFailure(err)
		}
		// New leases fail closed. Already acquired outbound leases may finish.
		return credentialStatus(credential.Masked(record))
	}
	x, err = a.connector(c)
	if err != nil {
		return nil, err
	}
	if action == "import" {
		var secret credential.Secret
		switch in.Kind {
		case "api_key":
			mode := c.Settings["auth_mode"]
			if (mode != "" && mode != "api_key") || (c.Settings["self_hosted"] == "true" && c.Settings["keyless_approved"] == "true") || strings.TrimSpace(in.Secret) == "" || in.AccessToken != "" || in.RefreshToken != "" || !in.ExpiresAt.IsZero() {
				return nil, actionError("invalid_credential", 400, "API key import is not valid for the configured authentication mode")
			}
			source, ok := x.(core.APIKeyPolicyProvider)
			if !ok {
				return nil, actionError("invalid_credential", 400, "Connector has no declared API key policy")
			}
			policy, policyErr := source.APIKeyPolicyFor(c)
			if policyErr != nil || policy.Header == "" {
				return nil, actionError("invalid_credential", 400, "Configured credential header or authentication mode is incompatible with the connector")
			}
			secret.APIKey = &credential.APIKey{Value: in.Secret, Header: policy.Header, Prefix: policy.Prefix}
		case "oauth":
			_, clientErr := a.oauthClient(ctx, c)
			if clientErr != nil || a.deps.VerifyCredentialAccount == nil {
				return nil, credentialUnavailable()
			}
			if in.Secret != "" {
				return nil, actionError("invalid_credential", 400, "OAuth import requires typed token fields")
			}
			secret.OAuth = &credential.OAuthToken{AccessToken: in.AccessToken, RefreshToken: in.RefreshToken, TokenType: in.TokenType, ExpiresAt: in.ExpiresAt, Scopes: in.Scopes}
		default:
			return nil, actionError("invalid_credential", 400, "Credential kind must be api_key or oauth")
		}
		return a.persistCredential(ctx, p, c, identity, secret, nil)
	}
	client, clientErr := a.oauthClient(ctx, c)
	if clientErr != nil || p.SessionID == "" || c.AccountID == "" {
		return nil, credentialUnavailable()
	}
	if client.Login == nil || client.Clock == nil || client.Config.HTTPClient == nil {
		return nil, credentialUnavailable()
	}
	if strings.HasPrefix(action, "device-") && client.Config.DeviceURL == "" {
		return nil, credentialUnavailable()
	}
	if x.Descriptor().Subscription {
		mode := "oauth"
		if strings.HasPrefix(action, "device-") {
			mode = "device"
		}
		if c.Settings["subscription_auth"] != mode {
			return nil, credentialUnavailable()
		}
	}
	switch action {
	case "oauth-start":
		auth, err := client.BeginForConnection(ctx, c.TenantID, p.SessionID, c.ID, c.AccountID, c.Version)
		if err != nil {
			return nil, credentialActionFailure(err)
		}
		return json.Marshal(map[string]any{"status": "authorization_required", "authorization_url": auth.URL, "expires_at": auth.ExpiresAt})
	case "oauth-callback":
		if in.RedirectURI != client.Config.RedirectURI || in.State == "" || in.Code == "" {
			return nil, actionError("invalid_request", 400, "State, code and exact registered redirect URI are required")
		}
		token, err := client.CallbackForConnection(ctx, c.TenantID, p.SessionID, c.ID, c.AccountID, in.State, in.Code, in.RedirectURI, c.Version)
		if err != nil {
			return nil, credentialActionFailure(err)
		}
		return a.persistCredentialResponse(ctx, p, c, identity, token)
	case "device-start":
		coordinator, coordErr := a.deviceCoordinator(ctx, c, client)
		if coordErr != nil || p.SessionID == "" {
			return nil, credentialUnavailable()
		}
		handle, err := coordinator.Start(ctx, c.TenantID, p.SessionID, c.ID, c.AccountID, c.Version)
		if err != nil {
			return nil, credentialActionFailure(err)
		}
		return json.Marshal(map[string]any{"status": "authorization_required", "flow_id": handle.ID, "user_code": handle.UserCode, "authorization_url": handle.VerificationURI, "authorization_url_complete": handle.VerificationURIComplete, "expires_at": handle.ExpiresAt})
	case "device-poll":
		if in.FlowID == "" {
			return nil, actionError("invalid_authorization", 400, "Device flow ID is required")
		}
		coordinator, coordErr := a.deviceCoordinator(ctx, c, client)
		if coordErr != nil || p.SessionID == "" {
			return nil, credentialUnavailable()
		}
		state, loadErr := coordinator.Repo.LoadDevice(ctx, c.TenantID, in.FlowID)
		if loadErr != nil || state.AdminSessionID != p.SessionID || state.ConnectionID != c.ID || state.AccountID != c.AccountID || state.Connector != c.Connector || state.ConnectionVersion != c.Version {
			return nil, actionError("invalid_authorization", 400, "Device authorization is invalid or expired")
		}
		token, err := coordinator.Poll(ctx, c.TenantID, in.FlowID, "admin:"+p.SubjectID)
		if err != nil {
			return nil, credentialActionFailure(err)
		}
		identity.Version = in.CredentialVersion
		return a.persistCredentialResponse(ctx, p, c, identity, *token)
	}
	return nil, actionError("unsupported_operation", 404, "Credential action is not supported")
}

func (a *Actions) persistCredentialResponse(ctx context.Context, p core.Principal, c core.Connection, identity credential.Identity, response credential.OAuthTokenResponse) (json.RawMessage, error) {
	if response.ExpiresIn <= 0 || response.ExpiresIn > int64((1<<63-1)/time.Second) {
		return nil, actionError("invalid_credential", 502, "Provider returned invalid token expiry")
	}
	token := credential.OAuthFromResponse(response, time.Now())
	return a.persistCredential(ctx, p, c, identity, credential.Secret{OAuth: &token}, response.Evidence)
}

func (a *Actions) persistCredential(ctx context.Context, p core.Principal, c core.Connection, identity credential.Identity, secret credential.Secret, evidence *credential.AccountEvidence) (json.RawMessage, error) {
	if secret.OAuth != nil {
		token := secret.OAuth
		if strings.TrimSpace(token.AccessToken) == "" || !token.ExpiresAt.After(time.Now()) || (token.TokenType != "" && !strings.EqualFold(token.TokenType, "Bearer")) {
			return nil, actionError("invalid_credential", 400, "OAuth token must be unexpired and use Bearer authentication")
		}
		if c.AccountID == "" {
			return nil, credentialUnavailable()
		}
		if evidence != nil {
			if evidence.Provider != c.Connector || evidence.AccountID != c.AccountID || evidence.Issuer == "" || evidence.Subject == "" || evidence.Method == "" {
				return nil, actionError("credential_identity_mismatch", 400, "Credential account verification failed")
			}
		} else {
			if a.deps.VerifyCredentialAccount == nil {
				return nil, credentialUnavailable()
			}
			if err := a.deps.VerifyCredentialAccount(ctx, c, secret); err != nil {
				return nil, actionError("credential_identity_mismatch", 400, "Credential account verification failed")
			}
		}
		if token.TokenType == "" {
			token.TokenType = "Bearer"
		}
	}
	current, _, err := a.connection(ctx, p, c.ID)
	if err != nil {
		return nil, err
	}
	if current.Version != c.Version || current.Connector != c.Connector || current.AccountID != c.AccountID {
		return nil, actionError("version_conflict", 409, "Connection changed during credential authorization")
	}
	if evidence != nil && (evidence.Provider != current.Connector || evidence.AccountID != current.AccountID) {
		return nil, actionError("credential_identity_mismatch", 409, "Connection identity changed during credential authorization")
	}
	if _, err := a.connector(current); err != nil {
		return nil, err
	}
	meta, err := a.deps.CredentialManager.Put(ctx, identity, secret)
	if err != nil {
		return nil, credentialActionFailure(err)
	}
	return credentialStatus(meta)
}

func credentialStatus(meta credential.Metadata) (json.RawMessage, error) {
	return json.Marshal(map[string]any{"credential_id": meta.CredentialID, "provider": meta.Provider, "account_id": meta.AccountID, "status": meta.Status, "version": meta.Version, "rotated_at": meta.RotatedAt})
}

func credentialUnavailable() error {
	return actionError("requires_credentials", 409, "Credentials or a permitted provider registration and account verifier are required")
}
func credentialActionFailure(err error) error {
	if errors.Is(err, credential.ErrConflict) {
		return actionError("version_conflict", 409, "Credential revision changed")
	}
	if errors.Is(err, credential.ErrReauthRequired) {
		return credentialUnavailable()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return actionError("authorization_interrupted", 408, "Credential authorization interrupted; begin a new authorization")
	}
	var ge core.GatewayError
	if errors.As(err, &ge) && ge.Code != "" {
		return ge
	}
	var gep *core.GatewayError
	if errors.As(err, &gep) && gep != nil && gep.Code != "" {
		return *gep
	}
	return actionError("credential_action_failed", 502, "Credential operation failed")
}
