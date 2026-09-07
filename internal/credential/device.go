package credential

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"time"
)

// DeviceState contains no plaintext device code. DeviceCodeEnvelope is sealed
// with the deployment key and AAD-bound to the initiating identity and handle.
type DeviceState struct {
	ID, TenantID, AdminSessionID, ConnectionID, AccountID, Connector string
	ConnectionVersion                                                int64
	DeviceCodeEnvelope                                               Envelope
	CompletionEnvelope                                               Envelope
	IDTokenEnvelope                                                  Envelope
	UserCode, VerificationURI, VerificationURIComplete               string
	ExpiresAt, PollAfter, LeaseUntil                                 time.Time
	PollInterval                                                     time.Duration
	LeaseOwner, Status, LastError                                    string
	Version                                                          int64
}

// DeviceRepository must perform lease acquisition and transitions atomically in
// SQL. A lease owner that loses a poll response must mark the state ambiguous;
// takeover is forbidden until an operator/provider contract resolves it.
type DeviceRepository interface {
	CreateDevice(context.Context, DeviceState) error
	LoadDevice(context.Context, string, string) (DeviceState, error)
	AcquireDeviceLease(context.Context, string, string, string, time.Time, time.Duration) (DeviceState, error)
	UpdateDevice(context.Context, DeviceState, int64) error
	CompleteDevice(context.Context, string, string, string, time.Time) error
	MarkDeviceAmbiguous(context.Context, string, string, string, string) error
}
type DeviceHandle struct {
	ID, UserCode, VerificationURI, VerificationURIComplete string
	ExpiresAt                                              time.Time
}
type DeviceCoordinator struct {
	Client *OAuthClient
	Repo   DeviceRepository
	Keys   Keyring
	Clock  func() time.Time
}

func NewDeviceCoordinator(c *OAuthClient, r DeviceRepository, k Keyring) (*DeviceCoordinator, error) {
	if c == nil || r == nil || k == nil {
		return nil, ErrInvalidCredential
	}
	return &DeviceCoordinator{Client: c, Repo: r, Keys: k, Clock: time.Now}, nil
}
func (d *DeviceCoordinator) markAmbiguous(ctx context.Context, tenant, id, owner, reason string) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = d.Repo.MarkDeviceAmbiguous(cleanup, tenant, id, owner, reason)
}
func deviceID() string {
	b := make([]byte, 18)
	if _, e := rand.Read(b); e != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func (d *DeviceCoordinator) Start(ctx context.Context, tenant, adminSession, connectionID, accountID string, version int64) (DeviceHandle, error) {
	if tenant == "" || adminSession == "" || connectionID == "" || accountID == "" {
		return DeviceHandle{}, errors.New("device authorization requires tenant, session, connection and account")
	}
	raw, err := d.Client.DeviceStart(ctx)
	if err != nil {
		return DeviceHandle{}, err
	}
	id := deviceID()
	if id == "" {
		return DeviceHandle{}, errors.New("device handle generation failed")
	}
	interval := raw.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	s := DeviceState{ID: id, TenantID: tenant, AdminSessionID: adminSession, ConnectionID: connectionID, AccountID: accountID, Connector: d.Client.Config.Connector, ConnectionVersion: version, UserCode: raw.UserCode, VerificationURI: raw.VerificationURI, VerificationURIComplete: raw.VerificationURIComplete, ExpiresAt: raw.ExpiresAt, PollInterval: interval, PollAfter: d.Clock().Add(interval), Status: "pending", Version: 1}
	s.DeviceCodeEnvelope, err = Seal(d.Keys, deviceIdentity(s), Secret{APIKey: &APIKey{Value: raw.DeviceCode}})
	if err != nil {
		return DeviceHandle{}, err
	}
	if err = d.Repo.CreateDevice(ctx, s); err != nil {
		return DeviceHandle{}, err
	}
	return DeviceHandle{ID: id, UserCode: s.UserCode, VerificationURI: s.VerificationURI, VerificationURIComplete: s.VerificationURIComplete, ExpiresAt: s.ExpiresAt}, nil
}
func deviceIdentity(s DeviceState) Identity {
	return Identity{TenantID: s.TenantID, ConnectionID: s.ConnectionID, CredentialID: "device:" + s.ID, AccountID: s.AccountID, Provider: s.Connector, Version: s.ConnectionVersion}
}
func (d *DeviceCoordinator) Poll(ctx context.Context, tenant, id, owner string) (*OAuthTokenResponse, error) {
	if owner == "" {
		return nil, errors.New("device polling owner is required")
	}
	now := d.Clock()
	completed, loadErr := d.Repo.LoadDevice(ctx, tenant, id)
	if loadErr == nil && completed.Status == "authorized" {
		return d.replayCompleted(ctx, completed)
	}
	fence, err := newFence()
	if err != nil {
		return nil, err
	}
	s, err := d.Repo.AcquireDeviceLease(ctx, tenant, id, fence, now, 30*time.Second)
	if err != nil {
		return nil, err
	}
	if s.Status != "pending" || !s.ExpiresAt.After(now) {
		d.markAmbiguous(ctx, tenant, id, fence, "device authorization is expired or no longer pending")
		return nil, errors.New("device authorization is expired or no longer pending")
	}
	sec, err := Open(d.Keys, deviceIdentity(s), s.DeviceCodeEnvelope)
	if err != nil || sec.APIKey == nil {
		d.markAmbiguous(ctx, tenant, id, fence, "device code envelope unavailable")
		return nil, ErrInvalidEnvelope
	}
	out, err := d.Client.DevicePollOnce(ctx, sec.APIKey.Value)
	if err == nil {
		if out.IDToken == "" || d.Client.Config.ValidateIDToken == nil {
			d.markAmbiguous(ctx, tenant, id, fence, "device ID token proof is required")
			return nil, errors.New("device ID token proof is required")
		}
		evidence, verifyErr := d.Client.Config.ValidateIDToken(ctx, out.IDToken, "")
		if verifyErr != nil || evidence.Provider != d.Client.Config.Connector || evidence.AccountID != s.AccountID || evidence.Issuer == "" || evidence.Subject == "" || evidence.Method == "" {
			d.markAmbiguous(ctx, tenant, id, fence, "device ID token validation failed")
			return nil, errors.New("device ID token validation failed")
		}
		if strings.TrimSpace(out.AccessToken) == "" || (out.TokenType != "" && !strings.EqualFold(out.TokenType, "Bearer")) || out.ExpiresIn <= 0 {
			d.markAmbiguous(ctx, tenant, id, fence, "device token response is invalid")
			return nil, errors.New("device token response is invalid")
		}
		token := OAuthFromResponse(out, d.Clock())
		completion, sealErr := Seal(d.Keys, deviceIdentity(s), Secret{OAuth: &token})
		if sealErr != nil {
			d.markAmbiguous(ctx, tenant, id, fence, "device completion encryption failed")
			return nil, sealErr
		}
		proof, sealErr := Seal(d.Keys, deviceIdentity(s), Secret{APIKey: &APIKey{Value: out.IDToken}})
		if sealErr != nil {
			d.markAmbiguous(ctx, tenant, id, fence, "device identity proof encryption failed")
			return nil, sealErr
		}
		s.CompletionEnvelope = completion
		s.IDTokenEnvelope = proof
		s.Status = "authorized"
		s.LeaseOwner = ""
		s.LeaseUntil = time.Time{}
		s.LastError = ""
		if e := d.Repo.UpdateDevice(ctx, s, s.Version); e != nil {
			d.markAmbiguous(ctx, tenant, id, fence, "device completion persistence failed")
			return nil, e
		}
		out.Evidence = &evidence
		return &out, nil
	}
	var oe oauthError
	if errors.As(err, &oe) {
		switch oe.ErrorCode {
		case "authorization_pending", "slow_down":
			if s.PollInterval <= 0 {
				s.PollInterval = 5 * time.Second
			}
			if oe.ErrorCode == "slow_down" {
				s.PollInterval += 5 * time.Second
			}
			s.PollAfter = d.Clock().Add(s.PollInterval)
			s.LeaseOwner = ""
			s.LeaseUntil = time.Time{}
			if e := d.Repo.UpdateDevice(ctx, s, s.Version); e != nil {
				d.markAmbiguous(ctx, tenant, id, fence, "device pending-state persistence failed")
				return nil, e
			}
			return nil, err
		case "expired_token", "access_denied":
			s.Status = oe.ErrorCode
			s.LastError = oe.ErrorCode
			s.LeaseOwner = ""
			s.LeaseUntil = time.Time{}
			if e := d.Repo.UpdateDevice(ctx, s, s.Version); e != nil {
				d.markAmbiguous(ctx, tenant, id, fence, "device terminal-state persistence failed")
				return nil, e
			}
			return nil, err
		}
	}
	d.markAmbiguous(ctx, tenant, id, fence, "device poll outcome unknown")
	return nil, ErrReauthRequired
}
func (d *DeviceCoordinator) replayCompleted(ctx context.Context, s DeviceState) (*OAuthTokenResponse, error) {
	if len(s.CompletionEnvelope.Ciphertext) == 0 || len(s.IDTokenEnvelope.Ciphertext) == 0 || d.Client.Config.ValidateIDToken == nil {
		return nil, errors.New("completed device authorization proof unavailable")
	}
	proof, err := Open(d.Keys, deviceIdentity(s), s.IDTokenEnvelope)
	if err != nil || proof.APIKey == nil {
		return nil, ErrInvalidEnvelope
	}
	evidence, err := d.Client.Config.ValidateIDToken(ctx, proof.APIKey.Value, "")
	if err != nil || evidence.Provider != s.Connector || evidence.AccountID != s.AccountID || evidence.Issuer == "" || evidence.Subject == "" || evidence.Method == "" {
		return nil, errors.New("completed device authorization identity validation failed")
	}
	sec, err := Open(d.Keys, deviceIdentity(s), s.CompletionEnvelope)
	if err != nil || sec.OAuth == nil {
		return nil, ErrInvalidEnvelope
	}
	if err := validateOAuth(*sec.OAuth, d.Clock()); err != nil {
		return nil, ErrReauthRequired
	}
	out := OAuthTokenResponse{AccessToken: sec.OAuth.AccessToken, RefreshToken: sec.OAuth.RefreshToken, TokenType: sec.OAuth.TokenType, Scope: strings.Join(sec.OAuth.Scopes, " "), ExpiresIn: int64(sec.OAuth.ExpiresAt.Sub(d.Clock()) / time.Second)}
	out.Evidence = &evidence
	return &out, nil
}
