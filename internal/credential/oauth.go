package credential

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// AccountEvidence is transient proof produced only by a trusted account verifier.
type AccountEvidence struct{ Provider, AccountID, Issuer, Subject, Method string }
type OAuthConfig struct {
	Connector, ClientID, ClientSecret, AuthorizationURL, TokenURL, DeviceURL, RedirectURI string
	Scopes                                                                                []string
	HTTPClient                                                                            *http.Client
	Keyring                                                                               Keyring
	ValidateIDToken                                                                       func(context.Context, string, string) (AccountEvidence, error)
}
type Authorization struct {
	URL, State string
	ExpiresAt  time.Time
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

type OAuthTokenResponse struct {
	AccessToken  string           `json:"access_token"`
	RefreshToken string           `json:"refresh_token"`
	TokenType    string           `json:"token_type"`
	ExpiresIn    int64            `json:"expires_in"`
	Scope        string           `json:"scope"`
	IDToken      string           `json:"id_token,omitempty"`
	Evidence     *AccountEvidence `json:"-"`
}
type DeviceAuthorization struct {
	DeviceCode, UserCode, VerificationURI string
	VerificationURIComplete               string
	ExpiresAt                             time.Time
	Interval                              time.Duration
}
type oauthError struct {
	ErrorCode   string `json:"error"`
	Description string `json:"error_description"`
}

func (e oauthError) Error() string {
	if e.Description != "" {
		return e.ErrorCode + ": " + e.Description
	}
	return e.ErrorCode
}

type OAuthClient struct {
	Config OAuthConfig
	Login  LoginRepository
	Clock  func() time.Time
}

func NewOAuthClient(cfg OAuthConfig, login LoginRepository) (*OAuthClient, error) {
	if login == nil || cfg.Keyring == nil || cfg.Connector == "" || cfg.ClientID == "" || cfg.AuthorizationURL == "" || cfg.TokenURL == "" || cfg.RedirectURI == "" {
		return nil, errors.New("oauth requires keyring, connector, client, authorization/token endpoints and exact redirect URI")
	}
	u, err := url.Parse(cfg.RedirectURI)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Fragment != "" {
		return nil, errors.New("redirect URI must be an absolute URI without fragment")
	}
	h := cfg.HTTPClient
	if h == nil {
		h = http.DefaultClient
	}
	cfg.HTTPClient = h
	return &OAuthClient{Config: cfg, Login: login, Clock: time.Now}, nil
}
func (c *OAuthClient) Begin(ctx context.Context, tenant, adminSession string) (Authorization, error) {
	return c.begin(ctx, tenant, adminSession, "", "", 0)
}
func (c *OAuthClient) BeginForConnection(ctx context.Context, tenant, adminSession, connectionID, accountID string, connectionVersion int64) (Authorization, error) {
	if connectionID == "" || accountID == "" {
		return Authorization{}, errors.New("OAuth login requires connection and account identity")
	}
	return c.begin(ctx, tenant, adminSession, connectionID, accountID, connectionVersion)
}
func (c *OAuthClient) begin(ctx context.Context, tenant, adminSession, connectionID, accountID string, connectionVersion int64) (Authorization, error) {
	if err := ctx.Err(); err != nil {
		return Authorization{}, err
	}
	state, err := randomString(32)
	if err != nil {
		return Authorization{}, err
	}
	nonce, err := randomString(32)
	if err != nil {
		return Authorization{}, err
	}
	verifier, err := randomString(48)
	if err != nil {
		return Authorization{}, err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	exp := c.Clock().Add(10 * time.Minute)
	u, err := url.Parse(c.Config.AuthorizationURL)
	if err != nil {
		return Authorization{}, errors.New("invalid authorization endpoint")
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", c.Config.ClientID)
	q.Set("redirect_uri", c.Config.RedirectURI)
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	if len(c.Config.Scopes) > 0 {
		q.Set("scope", strings.Join(c.Config.Scopes, " "))
	}
	u.RawQuery = q.Encode()
	if err := c.saveLogin(ctx, LoginSession{StateHash: HashVerifier(state), TenantID: tenant, AdminSessionID: adminSession, Connector: c.Config.Connector, ConnectionID: connectionID, AccountID: accountID, Nonce: nonce, Verifier: verifier, RedirectURI: c.Config.RedirectURI, ConnectionVersion: connectionVersion, ExpiresAt: exp}); err != nil {
		return Authorization{}, err
	}
	return Authorization{URL: u.String(), State: state, ExpiresAt: exp}, nil
}
func (c *OAuthClient) sessionIdentity(s LoginSession) Identity {
	return Identity{TenantID: s.TenantID, ConnectionID: s.ConnectionID, CredentialID: "oauth-session:" + s.StateHash, AccountID: s.AccountID, Provider: s.Connector, Version: s.ConnectionVersion}
}
func (c *OAuthClient) saveLogin(ctx context.Context, s LoginSession) error {
	e, err := Seal(c.Config.Keyring, c.sessionIdentity(s), Secret{APIKey: &APIKey{Value: s.Verifier}})
	if err != nil {
		return err
	}
	s.VerifierEnvelope = e
	s.Verifier = ""
	return c.Login.SaveOAuthLogin(ctx, s)
}
func (c *OAuthClient) sessionVerifier(s LoginSession) (string, error) {
	if s.Verifier != "" {
		return s.Verifier, nil
	}
	sec, err := Open(c.Config.Keyring, c.sessionIdentity(s), s.VerifierEnvelope)
	if err != nil || sec.APIKey == nil {
		return "", errors.New("OAuth PKCE verifier unavailable")
	}
	return sec.APIKey.Value, nil
}
func (c *OAuthClient) Callback(ctx context.Context, tenant, adminSession, state, code, redirectURI string) (OAuthTokenResponse, error) {
	if state == "" || code == "" || redirectURI != c.Config.RedirectURI {
		return OAuthTokenResponse{}, errors.New("invalid OAuth callback")
	}
	s, err := c.Login.ConsumeOAuthLogin(ctx, HashVerifier(state), tenant, adminSession, c.Config.Connector, c.Clock())
	if err != nil {
		return OAuthTokenResponse{}, err
	}
	return c.completeCallback(ctx, s, code, redirectURI)
}
func (c *OAuthClient) CallbackForConnection(ctx context.Context, tenant, adminSession, connectionID, accountID, state, code, redirectURI string, connectionVersion int64) (OAuthTokenResponse, error) {
	if connectionID == "" || accountID == "" {
		return OAuthTokenResponse{}, errors.New("OAuth callback requires connection and account identity")
	}
	if state == "" || code == "" || redirectURI != c.Config.RedirectURI {
		return OAuthTokenResponse{}, errors.New("invalid OAuth callback")
	}
	s, err := c.Login.ConsumeOAuthLogin(ctx, HashVerifier(state), tenant, adminSession, c.Config.Connector, c.Clock())
	if err != nil {
		return OAuthTokenResponse{}, err
	}
	if s.ConnectionID != connectionID || s.AccountID != accountID || s.ConnectionVersion != connectionVersion {
		return OAuthTokenResponse{}, errors.New("OAuth session connection binding mismatch")
	}
	return c.completeCallback(ctx, s, code, redirectURI)
}
func (c *OAuthClient) completeCallback(ctx context.Context, s LoginSession, code, redirectURI string) (OAuthTokenResponse, error) {
	if s.RedirectURI != redirectURI || s.Nonce == "" {
		return OAuthTokenResponse{}, errors.New("OAuth redirect or nonce binding missing")
	}
	verifier, err := c.sessionVerifier(s)
	if err != nil {
		return OAuthTokenResponse{}, err
	}
	out, err := c.exchange(ctx, code, verifier, redirectURI)
	if err != nil {
		return OAuthTokenResponse{}, err
	}
	if out.IDToken == "" {
		return OAuthTokenResponse{}, ErrReauthRequired
	}
	if err = c.verifyResponse(ctx, &out, s.Nonce); err != nil {
		return OAuthTokenResponse{}, err
	}
	if out.Evidence != nil && s.AccountID != "" && out.Evidence.AccountID != s.AccountID {
		return OAuthTokenResponse{}, ErrInvalidCredential
	}
	return out, nil
}
func (c *OAuthClient) verifyResponse(ctx context.Context, out *OAuthTokenResponse, nonce string) error {
	out.Evidence = nil
	if out.IDToken == "" {
		return nil
	}
	if c.Config.ValidateIDToken == nil {
		return ErrReauthRequired
	}
	evidence, err := c.Config.ValidateIDToken(ctx, out.IDToken, nonce)
	if err != nil {
		return errors.New("OIDC account/token validation failed")
	}
	if evidence.Provider != c.Config.Connector || evidence.AccountID == "" || evidence.Issuer == "" || evidence.Subject == "" || evidence.Method == "" {
		return ErrInvalidCredential
	}
	out.Evidence = &evidence
	return nil
}
func (c *OAuthClient) exchange(ctx context.Context, code, verifier, redirectURI string) (OAuthTokenResponse, error) {
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirectURI}, "client_id": {c.Config.ClientID}, "code_verifier": {verifier}}
	if c.Config.ClientSecret != "" {
		form.Set("client_secret", c.Config.ClientSecret)
	}
	return c.postToken(ctx, c.Config.TokenURL, form)
}
func (c *OAuthClient) Refresh(ctx context.Context, old OAuthToken) (OAuthToken, error) {
	if old.RefreshToken == "" {
		return OAuthToken{}, ErrReauthRequired
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {old.RefreshToken}, "client_id": {c.Config.ClientID}}
	if c.Config.ClientSecret != "" {
		form.Set("client_secret", c.Config.ClientSecret)
	}
	raw, err := c.postToken(ctx, c.Config.TokenURL, form)
	if err != nil {
		return OAuthToken{}, err
	}
	if err = c.verifyResponse(ctx, &raw, ""); err != nil {
		return OAuthToken{}, err
	}
	if raw.ExpiresIn <= 0 {
		return OAuthToken{}, errors.New("refresh response has invalid expiry")
	}
	next := OAuthFromResponse(raw, c.Clock())
	if next.RefreshToken == "" {
		next.RefreshToken = old.RefreshToken
	}
	return next, nil
}
func (c *OAuthClient) DeviceStart(ctx context.Context) (DeviceAuthorization, error) {
	if c.Config.DeviceURL == "" {
		return DeviceAuthorization{}, errors.New("device flow is not configured")
	}
	form := url.Values{"client_id": {c.Config.ClientID}}
	for _, s := range c.Config.Scopes {
		form.Add("scope", s)
	}
	var raw struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int64  `json:"expires_in"`
		Interval                int64  `json:"interval"`
	}
	if err := c.postForm(ctx, c.Config.DeviceURL, form, &raw); err != nil {
		return DeviceAuthorization{}, err
	}
	if raw.DeviceCode == "" || raw.ExpiresIn <= 0 {
		return DeviceAuthorization{}, errors.New("device endpoint returned incomplete authorization")
	}
	interval := 5 * time.Second
	if raw.Interval > 0 {
		interval = time.Duration(raw.Interval) * time.Second
	}
	return DeviceAuthorization{DeviceCode: raw.DeviceCode, UserCode: raw.UserCode, VerificationURI: raw.VerificationURI, VerificationURIComplete: raw.VerificationURIComplete, ExpiresAt: c.Clock().Add(time.Duration(raw.ExpiresIn) * time.Second), Interval: interval}, nil
}
func (c *OAuthClient) DevicePollOnce(ctx context.Context, deviceCode string) (OAuthTokenResponse, error) {
	if c.Config.DeviceURL == "" || deviceCode == "" {
		return OAuthTokenResponse{}, errors.New("device flow is not configured")
	}
	out, err := c.postToken(ctx, c.Config.TokenURL, url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {deviceCode}, "client_id": {c.Config.ClientID}})
	if err != nil {
		return OAuthTokenResponse{}, err
	}
	if err = c.verifyResponse(ctx, &out, ""); err != nil {
		return OAuthTokenResponse{}, err
	}
	return out, nil
}
func (c *OAuthClient) DevicePoll(ctx context.Context, d DeviceAuthorization) (OAuthTokenResponse, error) {
	if c.Config.DeviceURL == "" {
		return OAuthTokenResponse{}, errors.New("device flow is not configured")
	}
	interval := d.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	for {
		if !c.Clock().Before(d.ExpiresAt) {
			return OAuthTokenResponse{}, errors.New("device authorization expired")
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return OAuthTokenResponse{}, ctx.Err()
		case <-timer.C:
		}
		raw, err := c.DevicePollOnce(ctx, d.DeviceCode)
		if err == nil {
			return raw, nil
		}
		var oe oauthError
		if errors.As(err, &oe) {
			switch oe.ErrorCode {
			case "authorization_pending":
				continue
			case "slow_down":
				interval += 5 * time.Second
				continue
			case "expired_token":
				return OAuthTokenResponse{}, errors.New("device authorization expired")
			case "access_denied":
				return OAuthTokenResponse{}, errors.New("device authorization denied")
			}
		}
		return OAuthTokenResponse{}, err
	}
}
func (c *OAuthClient) postToken(ctx context.Context, endpoint string, form url.Values) (OAuthTokenResponse, error) {
	var out OAuthTokenResponse
	if err := c.postForm(ctx, endpoint, form, &out); err != nil {
		return out, err
	}
	if out.AccessToken == "" {
		return out, errors.New("token endpoint returned no access token")
	}
	return out, nil
}
func (c *OAuthClient) postForm(ctx context.Context, endpoint string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.Config.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if readErr != nil {
		return readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var oe oauthError
		if json.Unmarshal(b, &oe) == nil && oe.ErrorCode != "" {
			return oe
		}
		return fmt.Errorf("oauth endpoint returned HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return err
	}
	return nil
}
func OAuthFromResponse(r OAuthTokenResponse, now time.Time) OAuthToken {
	typ := r.TokenType
	if typ == "" {
		typ = "Bearer"
	}
	return OAuthToken{AccessToken: r.AccessToken, RefreshToken: r.RefreshToken, TokenType: typ, ExpiresAt: now.Add(time.Duration(r.ExpiresIn) * time.Second), Scopes: strings.Fields(r.Scope)}
}
