package credential

import (
	"context"
	"errors"
	"hoorific/internal/core"
	"time"
)

type OAuthClientFactory func(context.Context, core.Connection) (*OAuthClient, error)
type RefreshingSource struct {
	Manager       *Manager
	Store         Repository
	Refresher     *Refresher
	Factory       OAuthClientFactory
	RefreshBefore time.Duration
	Clock         func() time.Time
}

func NewRefreshingSource(m *Manager, store Repository, factory OAuthClientFactory) (core.CredentialSource, error) {
	if m == nil || store == nil || factory == nil {
		return nil, ErrInvalidCredential
	}
	r, err := NewRefresher(m, store)
	if err != nil {
		return nil, err
	}
	return &RefreshingSource{Manager: m, Store: store, Refresher: r, Factory: factory, RefreshBefore: 30 * time.Second, Clock: time.Now}, nil
}
func (s *RefreshingSource) Lease(ctx context.Context, conn core.Connection) (core.CredentialLease, error) {
	if conn.Settings["self_hosted"] == "true" && conn.Settings["keyless_approved"] == "true" {
		return keylessLease{}, nil
	}
	r, err := loadUsableCredential(s.Store, ctx, conn.TenantID, conn.ID)
	if err != nil {
		if !errors.Is(err, ErrReauthRequired) {
			return nil, err
		}
		metadata, metadataErr := s.Store.LoadCredential(ctx, conn.TenantID, conn.ID)
		if metadataErr != nil {
			return nil, err
		}
		if metadata.Identity.TenantID != conn.TenantID || metadata.Identity.ConnectionID != conn.ID || conn.AccountID != "" && metadata.Identity.AccountID != conn.AccountID || conn.Connector != "" && metadata.Identity.Provider != conn.Connector {
			return nil, ErrInvalidCredential
		}
		r, joinErr := s.Refresher.Join(ctx, metadata.Identity)
		if joinErr != nil {
			return nil, joinErr
		}
		return s.Manager.leaseRecord(ctx, r)
	}
	if r.Identity.TenantID != conn.TenantID || r.Identity.ConnectionID != conn.ID || conn.AccountID != "" && r.Identity.AccountID != conn.AccountID || conn.Connector != "" && r.Identity.Provider != conn.Connector {
		return nil, ErrInvalidCredential
	}
	if r.Status != "" && r.Status != "active" {
		return nil, ErrInvalidCredential
	}
	secret, err := Open(s.Manager.Keys, r.Identity, r.Envelope)
	if err != nil {
		return nil, err
	}
	if secret.OAuth != nil && !secret.OAuth.ExpiresAt.After(s.Clock().Add(s.RefreshBefore)) {
		if secret.OAuth.RefreshToken == "" {
			return nil, ErrReauthRequired
		}
		client, err := s.Factory(ctx, conn)
		if err != nil {
			return nil, err
		}
		if client == nil {
			return nil, ErrInvalidCredential
		}
		r, err = s.Refresher.RefreshLoaded(ctx, r.Identity, r, client.Refresh)
		if err != nil {
			return nil, err
		}
		return s.Manager.leaseRecord(ctx, r)
	}
	return s.Manager.leaseSecret(r, secret)
}

var _ core.CredentialSource = (*RefreshingSource)(nil)
