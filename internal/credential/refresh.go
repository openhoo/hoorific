package credential

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

type TokenRefresher func(context.Context, OAuthToken) (OAuthToken, error)
type RefreshError struct {
	Err      error
	Executed bool
}

func (e RefreshError) Error() string { return e.Err.Error() }
func (e RefreshError) Unwrap() error { return e.Err }

type refreshCall struct {
	done   chan struct{}
	record Record
	err    error
}
type Refresher struct {
	Manager *Manager
	Store   Repository
	mu      sync.Mutex
	calls   map[string]*refreshCall
	now     func() time.Time
}

func NewRefresher(m *Manager, store Repository) (*Refresher, error) {
	if m == nil || store == nil {
		return nil, ErrInvalidCredential
	}
	return &Refresher{Manager: m, Store: store, calls: make(map[string]*refreshCall), now: time.Now}, nil
}
func refreshKey(i Identity) string {
	return i.TenantID + "\x00" + i.ConnectionID + "\x00" + i.CredentialID + "\x00" + i.AccountID
}
func newFence() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func (r *Refresher) Refresh(ctx context.Context, id Identity, fn TokenRefresher) (Record, error) {
	return r.refreshSingleflight(ctx, id, fn)
}
func (r *Refresher) RefreshLoaded(ctx context.Context, id Identity, old Record, fn TokenRefresher) (Record, error) {
	if old.Identity != id {
		return Record{}, ErrConflict
	}
	return r.refreshSingleflight(ctx, id, fn)
}

// Join waits for an already-running refresh without ever starting one or
// opening the stale credential. It is used after a durable refresh fence was
// observed by a concurrent request.
func (r *Refresher) Join(ctx context.Context, id Identity) (Record, error) {
	key := refreshKey(id)
	r.mu.Lock()
	call := r.calls[key]
	r.mu.Unlock()
	if call != nil {
		return waitRefreshCall(ctx, call)
	}
	current, err := loadUsableCredential(r.Store, ctx, id.TenantID, id.ConnectionID)
	if err == nil {
		if sameCredential(current.Identity, id) && current.Identity.Version > id.Version {
			return current, nil
		}
		return Record{}, ErrReauthRequired
	}
	if !errors.Is(err, ErrReauthRequired) {
		return Record{}, err
	}
	r.mu.Lock()
	call = r.calls[key]
	r.mu.Unlock()
	if call == nil {
		return Record{}, err
	}
	return waitRefreshCall(ctx, call)
}
func waitRefreshCall(ctx context.Context, call *refreshCall) (Record, error) {
	select {
	case <-call.done:
		return call.record, call.err
	case <-ctx.Done():
		return Record{}, ctx.Err()
	}
}
func (r *Refresher) refreshSingleflight(ctx context.Context, id Identity, fn TokenRefresher) (Record, error) {
	if fn == nil {
		return Record{}, ErrInvalidCredential
	}
	key := refreshKey(id)
	r.mu.Lock()
	if c := r.calls[key]; c != nil {
		r.mu.Unlock()
		select {
		case <-c.done:
			return c.record, c.err
		case <-ctx.Done():
			return Record{}, ctx.Err()
		}
	}
	c := &refreshCall{done: make(chan struct{})}
	r.calls[key] = c
	r.mu.Unlock()
	old, err := loadUsableCredential(r.Store, ctx, id.TenantID, id.ConnectionID)
	if err == nil && old.Identity != id {
		if sameCredential(old.Identity, id) && old.Identity.Version > id.Version {
			c.record = old
		} else {
			c.err = ErrConflict
		}
	} else if err != nil {
		c.err = err
	} else {
		c.record, c.err = r.refreshRecord(ctx, id, old, fn)
	}
	close(c.done)
	r.mu.Lock()
	delete(r.calls, key)
	r.mu.Unlock()
	return c.record, c.err
}
func sameCredential(a, b Identity) bool {
	return a.TenantID == b.TenantID && a.ConnectionID == b.ConnectionID && a.CredentialID == b.CredentialID && a.AccountID == b.AccountID && a.Provider == b.Provider
}
func (r *Refresher) refreshRecord(ctx context.Context, id Identity, old Record, fn TokenRefresher) (Record, error) {
	if old.Identity.AccountID != id.AccountID || old.Identity.Version != id.Version {
		return Record{}, ErrConflict
	}
	secret, err := Open(r.Manager.Keys, id, old.Envelope)
	if err != nil {
		return Record{}, err
	}
	if secret.OAuth == nil || secret.OAuth.RefreshToken == "" {
		return Record{}, ErrReauthRequired
	}
	fence, err := newFence()
	if err != nil {
		return Record{}, err
	}
	intent := RefreshIntent{Identity: id, AttemptID: newAttemptID(), Fence: fence, LeaseUntil: r.now().Add(2 * time.Minute)}
	if err = r.Store.BeginRefresh(ctx, intent); err != nil {
		return Record{}, err
	}
	next, callErr := fn(ctx, *secret.OAuth)
	if callErr != nil {
		var classified RefreshError
		if errors.As(callErr, &classified) && !classified.Executed {
			_ = r.Store.FailRefresh(context.Background(), intent, "refresh rejected before dispatch")
			return Record{}, callErr
		}
		// A rotating-token request may have reached the provider. Preserve ambiguity.
		_ = r.Store.FailRefresh(context.Background(), intent, "refresh outcome unknown: "+callErr.Error())
		return Record{}, fmt.Errorf("%w: %v", ErrReauthRequired, callErr)
	}
	if next.RefreshToken == "" {
		next.RefreshToken = secret.OAuth.RefreshToken
	}
	if err := validateOAuth(next, r.now()); err != nil {
		_ = r.Store.FailRefresh(context.Background(), intent, "provider returned invalid token")
		return Record{}, err
	}
	if next.TokenType == "" {
		next.TokenType = "Bearer"
	}
	nextIdentity := id
	nextIdentity.Version++
	env, err := Seal(r.Manager.Keys, nextIdentity, Secret{OAuth: &next})
	if err != nil {
		_ = r.Store.FailRefresh(context.Background(), intent, "envelope encryption failed")
		return Record{}, err
	}
	out := Record{Identity: nextIdentity, Envelope: env, Status: "active", RotatedAt: r.now().UTC()}
	if err := r.Store.CommitRefresh(ctx, intent, out); err != nil {
		return Record{}, err
	}
	return out, nil
}
func newAttemptID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("refresh-%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func validateOAuth(t OAuthToken, now time.Time) error {
	if t.AccessToken == "" {
		return errors.New("provider returned empty access token")
	}
	if t.TokenType != "" && !strings.EqualFold(t.TokenType, "Bearer") {
		return errors.New("provider returned unsupported token type")
	}
	if t.ExpiresAt.IsZero() || !t.ExpiresAt.After(now) {
		return errors.New("provider returned expired token")
	}
	return nil
}
