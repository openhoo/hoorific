package realtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"hoorific/internal/core"
)

type Ticket struct {
	IDHash                                            []byte
	TenantID, ConnectionID, ModelID, KeyID, SessionID string
	Principal                                         core.Principal
	Grants                                            []string
	ExpiresAt                                         time.Time
	Used                                              bool
}

// TicketStore is the sole bearer admission boundary. Implementations must atomically
// validate route scope, expiry, and used state before consuming the hash.
type TicketStore interface {
	PutTicket(context.Context, Ticket) error
	ConsumeTicketBound(context.Context, []byte, time.Time, string, string) (Ticket, error)
}

func IssueBoundTicket(ctx context.Context, s TicketStore, p core.Principal, connection, model string, now time.Time) (string, error) {
	if p.TenantID == "" || (p.KeyID == "" && p.SessionID == "") || !p.Realtime {
		return "", errors.New("realtime principal grant required")
	}
	return issueTicket(ctx, s, Ticket{TenantID: p.TenantID, ConnectionID: connection, ModelID: model, KeyID: p.KeyID, SessionID: p.SessionID, Principal: p, Grants: []string{"realtime"}}, now)
}
func issueTicket(ctx context.Context, s TicketStore, t Ticket, now time.Time) (string, error) {
	if t.TenantID == "" || t.ConnectionID == "" || t.ModelID == "" || (t.KeyID == "" && t.SessionID == "") {
		return "", errors.New("ticket identity required")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	t.IDHash = sum[:]
	t.ExpiresAt = now.Add(30 * time.Second)
	if err := s.PutTicket(ctx, t); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// ConsumeTicket is the canonical bearer-only helper. It never accepts a caller principal or key.
func ConsumeTicket(ctx context.Context, s TicketStore, token string, now time.Time, connection, model string) (Ticket, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != 32 {
		return Ticket{}, errors.New("invalid realtime ticket")
	}
	sum := sha256.Sum256(raw)
	t, err := s.ConsumeTicketBound(ctx, sum[:], now, connection, model)
	if err != nil {
		return Ticket{}, err
	}
	if subtle.ConstantTimeCompare(t.IDHash, sum[:]) != 1 || t.TenantID == "" || t.ConnectionID != connection || t.ModelID != model || t.Used || !t.ExpiresAt.After(now) {
		return Ticket{}, errors.New("invalid or expired realtime ticket")
	}
	if err := restoreTicketIdentity(&t); err != nil {
		return Ticket{}, err
	}
	return t, nil
}
func restoreTicketIdentity(t *Ticket) error {
	if t.Principal.TenantID != "" && t.Principal.TenantID != t.TenantID {
		return errors.New("ticket tenant identity conflict")
	}
	if t.SessionID != "" && t.Principal.SessionID != "" && t.SessionID != t.Principal.SessionID {
		return errors.New("ticket session identity conflict")
	}
	if t.KeyID != "" && t.Principal.KeyID != "" && t.KeyID != t.Principal.KeyID {
		return errors.New("ticket key identity conflict")
	}
	if t.SessionID != "" {
		t.Principal.SessionID = t.SessionID
	}
	if t.KeyID != "" {
		t.Principal.KeyID = t.KeyID
	}
	if t.Principal.TenantID == "" {
		t.Principal.TenantID = t.TenantID
	}
	return nil
}
func HasGrant(t Ticket, grant string) bool {
	for _, g := range t.Grants {
		if g == grant {
			return true
		}
	}
	return false
}
func AuthorizeTicket(t Ticket, tenant, connection, model, grant string) error {
	if t.TenantID != tenant || t.ConnectionID != connection || t.ModelID != model {
		return errors.New("realtime ticket scope mismatch")
	}
	if grant != "" && !HasGrant(t, grant) {
		return errors.New("realtime grant denied")
	}
	return nil
}
func GrantList(raw string) []string {
	var out []string
	for _, v := range strings.Split(raw, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
