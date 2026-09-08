package credential

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"hoorific/internal/core"
)

var ErrInvalidEnvelope = errors.New("invalid credential envelope")
var ErrInvalidCredential = errors.New("invalid credential")

type Keyring interface {
	Current() (string, []byte, error)
	ByID(string) ([]byte, error)
}
type StaticKeyring struct {
	ID       string
	Key      []byte
	Previous map[string][]byte
}

func (k StaticKeyring) Current() (string, []byte, error) {
	if len(k.Key) != 32 || k.ID == "" {
		return "", nil, ErrInvalidCredential
	}
	return k.ID, append([]byte(nil), k.Key...), nil
}
func (k StaticKeyring) ByID(id string) ([]byte, error) {
	if id == k.ID {
		return append([]byte(nil), k.Key...), nil
	}
	if v, ok := k.Previous[id]; ok && len(v) == 32 {
		return append([]byte(nil), v...), nil
	}
	return nil, ErrInvalidEnvelope
}
func NewKeyring(id string, key []byte, previous map[string][]byte) (StaticKeyring, error) {
	if id == "" || len(key) != 32 {
		return StaticKeyring{}, fmt.Errorf("master key must be exactly 32 bytes")
	}
	return StaticKeyring{ID: id, Key: append([]byte(nil), key...), Previous: previous}, nil
}

// LoadKeyring reads the deployment key file format:
// {"current":"key-id","keys":{"key-id":"base64-raw-or-standard-32-byte-key"}}.
// Keys are decoded once and retained only in the process keyring; the file is
// never returned through an administrative API.
func LoadKeyring(path string) (StaticKeyring, error) {
	if path == "" {
		return StaticKeyring{}, ErrInvalidCredential
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return StaticKeyring{}, err
	}
	var raw struct {
		Current string            `json:"current"`
		Keys    map[string]string `json:"keys"`
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if err := d.Decode(&raw); err != nil {
		return StaticKeyring{}, fmt.Errorf("invalid encryption key file: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return StaticKeyring{}, ErrInvalidCredential
	}
	if raw.Current == "" || len(raw.Keys) == 0 {
		return StaticKeyring{}, ErrInvalidCredential
	}
	decoded := map[string][]byte{}
	for id, encoded := range raw.Keys {
		if id == "" {
			return StaticKeyring{}, ErrInvalidCredential
		}
		v, e := base64.RawStdEncoding.DecodeString(encoded)
		if e != nil || len(v) != 32 {
			v, e = base64.StdEncoding.DecodeString(encoded)
		}
		if e != nil || len(v) != 32 {
			return StaticKeyring{}, ErrInvalidCredential
		}
		decoded[id] = v
	}
	cur, ok := decoded[raw.Current]
	if !ok {
		return StaticKeyring{}, ErrInvalidCredential
	}
	delete(decoded, raw.Current)
	return NewKeyring(raw.Current, cur, decoded)
}

func identityAAD(i Identity) []byte {
	b, _ := json.Marshal(struct {
		Tenant, Connection, Credential, Account, Provider string
		Version                                           int64
	}{i.TenantID, i.ConnectionID, i.CredentialID, i.AccountID, i.Provider, i.Version})
	return b
}
func Seal(keyring Keyring, i Identity, secret Secret) (Envelope, error) {
	id, key, err := keyring.Current()
	if err != nil {
		return Envelope{}, err
	}
	if err := validateSecret(secret); err != nil {
		return Envelope{}, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return Envelope{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return Envelope{}, err
	}
	plain, err := json.Marshal(secret)
	if err != nil {
		return Envelope{}, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return Envelope{}, err
	}
	return Envelope{KeyID: id, Nonce: nonce, Ciphertext: aead.Seal(nil, nonce, plain, identityAAD(i))}, nil
}
func Open(keyring Keyring, i Identity, e Envelope) (Secret, error) {
	if e.KeyID == "" {
		return Secret{}, ErrInvalidEnvelope
	}
	key, err := keyring.ByID(e.KeyID)
	if err != nil {
		return Secret{}, ErrInvalidEnvelope
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return Secret{}, ErrInvalidEnvelope
	}
	aead, err := cipher.NewGCM(block)
	if err != nil || len(e.Nonce) != aead.NonceSize() {
		return Secret{}, ErrInvalidEnvelope
	}
	plain, err := aead.Open(nil, e.Nonce, e.Ciphertext, identityAAD(i))
	if err != nil {
		return Secret{}, ErrInvalidEnvelope
	}
	var s Secret
	if json.Unmarshal(plain, &s) != nil || validateSecret(s) != nil {
		return Secret{}, ErrInvalidEnvelope
	}
	return s, nil
}
func validateSecret(s Secret) error {
	if (s.APIKey == nil) == (s.OAuth == nil) {
		return ErrInvalidCredential
	}
	if s.APIKey != nil && strings.TrimSpace(s.APIKey.Value) == "" {
		return ErrInvalidCredential
	}
	if s.OAuth != nil && strings.TrimSpace(s.OAuth.AccessToken) == "" {
		return ErrInvalidCredential
	}
	return nil
}
func Masked(r Record) Metadata {
	return Metadata{CredentialID: r.Identity.CredentialID, Provider: r.Identity.Provider, AccountID: r.Identity.AccountID, Status: r.Status, Version: r.Identity.Version, RotatedAt: r.RotatedAt}
}
func maskedSecret(r Record, s Secret) Metadata {
	m := Masked(r)
	if s.OAuth != nil && !s.OAuth.ExpiresAt.IsZero() {
		v := s.OAuth.ExpiresAt
		m.ExpiresAt = &v
	}
	return m
}

type usableCredentialLoader interface {
	LoadUsableCredential(context.Context, string, string) (Record, error)
}

func loadUsableCredential(store Repository, ctx context.Context, tenant, connection string) (Record, error) {
	if loader, ok := store.(usableCredentialLoader); ok {
		return loader.LoadUsableCredential(ctx, tenant, connection)
	}
	return store.LoadCredential(ctx, tenant, connection)
}

// Manager decrypts only for an outbound request; callers cannot retrieve Secret
// or raw envelope through this API. The resulting lease is request-scoped.
type Manager struct {
	Keys  Keyring
	Store Repository
}

func NewManager(keys Keyring, store Repository) (*Manager, error) {
	if keys == nil || store == nil {
		return nil, ErrInvalidCredential
	}
	return &Manager{Keys: keys, Store: store}, nil
}
func (m *Manager) Put(ctx context.Context, i Identity, s Secret) (Metadata, error) {
	if err := validateSecret(s); err != nil {
		return Metadata{}, err
	}
	next := i
	next.Version++
	e, err := Seal(m.Keys, next, s)
	if err != nil {
		return Metadata{}, err
	}
	r := Record{Identity: next, Envelope: e, Status: "active", RotatedAt: time.Now().UTC()}
	if err := m.Store.PutCredential(ctx, r, i.Version); err != nil {
		return Metadata{}, err
	}
	return maskedSecret(r, s), nil
}
func (m *Manager) Metadata(ctx context.Context, tenant, connection string) (Metadata, error) {
	r, err := m.Store.LoadCredential(ctx, tenant, connection)
	if err != nil {
		return Metadata{}, err
	}
	s, err := Open(m.Keys, r.Identity, r.Envelope)
	if err != nil {
		return Metadata{}, err
	}
	return maskedSecret(r, s), nil
}

// Rewrap rotates the master-key envelope and credential version while
// preserving the token/account identity. It never returns decrypted data.
func (m *Manager) Rewrap(ctx context.Context, tenant, connection string) (Metadata, error) {
	store, ok := m.Store.(RewrapRepository)
	if !ok {
		return Metadata{}, errors.New("atomic credential rewrap is unavailable")
	}
	r, err := m.Store.LoadCredential(ctx, tenant, connection)
	if err != nil {
		return Metadata{}, err
	}
	s, err := Open(m.Keys, r.Identity, r.Envelope)
	if err != nil {
		return Metadata{}, err
	}
	next := r.Identity
	next.Version++
	e, err := Seal(m.Keys, next, s)
	if err != nil {
		return Metadata{}, err
	}
	r.Identity = next
	r.Envelope = e
	r.RotatedAt = time.Now().UTC()
	if err := store.RewrapCredential(ctx, r, next.Version-1); err != nil {
		return Metadata{}, err
	}
	return maskedSecret(r, s), nil
}

type source struct{ manager *Manager }

func (s source) Lease(ctx context.Context, conn core.Connection) (core.CredentialLease, error) {
	if conn.Settings["self_hosted"] == "true" && conn.Settings["keyless_approved"] == "true" {
		return keylessLease{}, nil
	}
	r, err := loadUsableCredential(s.manager.Store, ctx, conn.TenantID, conn.ID)
	if err != nil {
		return nil, err
	}
	if r.Identity.TenantID != conn.TenantID || r.Identity.ConnectionID != conn.ID {
		return nil, ErrInvalidCredential
	}
	if conn.AccountID != "" && r.Identity.AccountID != conn.AccountID {
		return nil, ErrInvalidCredential
	}
	if conn.Connector != "" && r.Identity.Provider != conn.Connector {
		return nil, ErrInvalidCredential
	}
	if r.Status != "" && r.Status != "active" {
		return nil, ErrInvalidCredential
	}
	// Credential rotation changes token version only; connection version is a
	// configuration revision and is intentionally not compared to credential version.
	return s.manager.leaseRecord(ctx, r)
}
func (m *Manager) Source() core.CredentialSource { return source{manager: m} }

type keylessLease struct{}

func (keylessLease) Authorize(_ context.Context, r *http.Request) error {
	if r == nil {
		return ErrInvalidCredential
	}
	r.Header.Del("Authorization")
	r.Header.Del("x-api-key")
	r.Header.Del("x-goog-api-key")
	return nil
}

var _ core.CredentialSource = source{}
var _ core.CredentialLease = keylessLease{}

const (
	claudeSubscriptionProvider = "claude-subscription"
	// Source: https://github.com/anthropics/anthropic-sdk-go/commit/d2f6543e
	claudeSubscriptionOAuthBeta = "oauth-2025-04-20"
)

func mergeClaudeSubscriptionBeta(header http.Header) error {
	values := header.Values("anthropic-beta")
	tokens := make([]string, 0, len(values)+1)
	seen := make(map[string]struct{}, len(values)+1)
	for _, value := range values {
		if value == "" || strings.ContainsAny(value, "\r\n\x00") {
			return invalidClaudeSubscriptionHeader("Claude subscription anthropic-beta must contain capability values")
		}
		for _, raw := range strings.Split(value, ",") {
			token := strings.TrimSpace(raw)
			if !validClaudeSubscriptionToken(token) {
				return invalidClaudeSubscriptionHeader("Claude subscription anthropic-beta contains an invalid capability value")
			}
			if _, ok := seen[token]; ok {
				continue
			}
			seen[token] = struct{}{}
			tokens = append(tokens, token)
		}
	}
	if _, ok := seen[claudeSubscriptionOAuthBeta]; !ok {
		tokens = append(tokens, claudeSubscriptionOAuthBeta)
	}
	header.Set("anthropic-beta", strings.Join(tokens, ","))
	return nil
}

func validClaudeSubscriptionToken(value string) bool {
	if value == "" {
		return false
	}
	for i := range len(value) {
		c := value[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			continue
		}
		switch c {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		default:
			return false
		}
	}
	return true
}

func invalidClaudeSubscriptionHeader(message string) error {
	return core.GatewayError{Code: "invalid_request", HTTPStatus: http.StatusBadRequest, Message: message, Param: "anthropic-beta", Origin: "gateway"}
}

type lease struct {
	manager *Manager
	record  Record
	secret  Secret
	closed  bool
}

func (l *lease) Authorize(_ context.Context, r *http.Request) error {
	if l.closed || r == nil {
		return ErrInvalidCredential
	}
	if r.Header == nil {
		r.Header = make(http.Header)
	}
	s := l.secret
	var claudeBeta string
	if l.record.Identity.Provider == claudeSubscriptionProvider && s.OAuth != nil {
		if err := mergeClaudeSubscriptionBeta(r.Header); err != nil {
			return err
		}
		claudeBeta = r.Header.Get("anthropic-beta")
	}
	if s.APIKey != nil {
		h := s.APIKey.Header
		if h == "" {
			h = "Authorization"
		}
		if strings.EqualFold(h, "authorization") {
			r.Header.Del("Authorization")
			r.Header.Set("Authorization", strings.TrimSpace(s.APIKey.Prefix+" "+s.APIKey.Value))
		} else {
			r.Header.Del(h)
			r.Header.Set(h, s.APIKey.Prefix+s.APIKey.Value)
		}
		return nil
	}
	r.Header.Del("Authorization")
	r.Header.Del("x-api-key")
	r.Header.Del("x-goog-api-key")
	typ := s.OAuth.TokenType
	if typ == "" {
		typ = "Bearer"
	}
	r.Header.Set("Authorization", typ+" "+s.OAuth.AccessToken)
	if claudeBeta != "" {
		r.Header.Set("anthropic-beta", claudeBeta)
	}
	return nil
}
func (l *lease) Close() { l.closed = true; l.secret = Secret{} }
func (m *Manager) leaseSecret(r Record, s Secret) (*lease, error) {
	if err := validateSecret(s); err != nil {
		return nil, err
	}
	return &lease{manager: m, record: r, secret: s}, nil
}
func (m *Manager) leaseRecord(ctx context.Context, r Record) (*lease, error) {
	s, err := Open(m.Keys, r.Identity, r.Envelope)
	if err != nil {
		return nil, err
	}
	return m.leaseSecret(r, s)
}
func (m *Manager) Lease(ctx context.Context, c Identity) (*lease, error) {
	r, err := loadUsableCredential(m.Store, ctx, c.TenantID, c.ConnectionID)
	if err != nil {
		return nil, err
	}
	if r.Identity.TenantID != c.TenantID || r.Identity.ConnectionID != c.ConnectionID || r.Identity.AccountID != c.AccountID || r.Identity.Provider != c.Provider || r.Identity.Version != c.Version {
		return nil, ErrInvalidCredential
	}
	if r.Status != "" && r.Status != "active" {
		return nil, ErrInvalidCredential
	}
	return m.leaseRecord(ctx, r)
}
func ConstantTimeEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare(a, b) == 1
}
func HashVerifier(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return base64.RawURLEncoding.EncodeToString(h[:])
}
