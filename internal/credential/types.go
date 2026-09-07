// Package credential owns write-only upstream secrets and their durable lifecycle.
package credential

import (
	"context"
	"errors"
	"time"
)

var ErrReauthRequired = errors.New("credential requires reauthentication")
var ErrConflict = errors.New("credential version or refresh fence changed")

type Identity struct {
	TenantID, ConnectionID, CredentialID, AccountID, Provider string
	Version                                                   int64
}
type APIKey struct {
	Value  string `json:"value"`
	Header string `json:"header"`
	Prefix string `json:"prefix"`
}
type OAuthToken struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type"`
	ExpiresAt    time.Time `json:"expires_at"`
	Scopes       []string  `json:"scopes,omitempty"`
}
type Secret struct {
	APIKey *APIKey     `json:"api_key,omitempty"`
	OAuth  *OAuthToken `json:"oauth,omitempty"`
}
type Envelope struct {
	KeyID      string `json:"key_id"`
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
}
type Metadata struct {
	CredentialID, Provider, AccountID, Status string
	Version                                   int64
	RotatedAt                                 time.Time
	ExpiresAt                                 *time.Time
}
type Record struct {
	Identity  Identity
	Envelope  Envelope
	Status    string
	RotatedAt time.Time
}
type RefreshIntent struct {
	Identity         Identity
	AttemptID, Fence string
	LeaseUntil       time.Time
}

// Repository implementations MUST use one SQL transaction for each mutation.
// BeginRefresh persists intent before returning; an unresolved prior intent is
// ErrReauthRequired even after lease expiry, never permission to reuse its token.
// CommitRefresh compares original version AND fence and atomically completes the
// attempt and installs version+1. All lookups are tenant/connection/account scoped.
type Repository interface {
	LoadCredential(context.Context, string, string) (Record, error)
	PutCredential(context.Context, Record, int64) error
	BeginRefresh(context.Context, RefreshIntent) error
	CommitRefresh(context.Context, RefreshIntent, Record) error
	FailRefresh(context.Context, RefreshIntent, string) error
}

// RewrapCredential must atomically CAS the envelope and reject unresolved
// refresh intents; it MUST NOT clear a pending or ambiguous refresh fence.
type RewrapRepository interface {
	RewrapCredential(context.Context, Record, int64) error
}
type LoginSession struct {
	StateHash, TenantID, AdminSessionID, Connector, ConnectionID, AccountID, Nonce, RedirectURI string
	Verifier                                                                                    string   `json:"-"`
	VerifierEnvelope                                                                            Envelope `json:"verifier_envelope"`
	ConnectionVersion                                                                           int64
	ExpiresAt                                                                                   time.Time
}

// ConsumeOAuthLogin atomically deletes/marks consumed only if every binding and expiry
// matches; state is stored as a hash, the PKCE verifier must be encrypted at rest.
type LoginRepository interface {
	SaveOAuthLogin(context.Context, LoginSession) error
	ConsumeOAuthLogin(context.Context, string, string, string, string, time.Time) (LoginSession, error)
}
