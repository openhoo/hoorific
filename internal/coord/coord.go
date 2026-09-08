// Package coord contains disposable coordination hints, never admission state.
package coord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hoorific/internal/core"
	"strconv"
	"sync"
	"time"
)

var ErrNotFound = errors.New("coordination hint not found")
var ErrScopeRequired = errors.New("coordination hint scope is required")

const (
	MinHintTTL           = time.Second
	MaxHintTTL           = 5 * time.Minute
	DefaultRemoteTimeout = 5 * time.Millisecond
)

func boundTTL(ttl time.Duration) (time.Duration, error) {
	if ttl <= 0 {
		return 0, fmt.Errorf("hint TTL must be positive")
	}
	if ttl < MinHintTTL {
		ttl = MinHintTTL
	}
	if ttl > MaxHintTTL {
		ttl = MaxHintTTL
	}
	return ttl, nil
}

type hint struct {
	healthy bool
	expires time.Time
}
type affinityHint struct {
	target  core.RouteTarget
	expires time.Time
}
type Local struct {
	mu       sync.Mutex
	hints    map[string]hint
	affinity map[string]affinityHint
}

func NewLocal() *Local {
	return &Local{hints: make(map[string]hint), affinity: make(map[string]affinityHint)}
}

func validTarget(t core.RouteTarget) bool {
	return t.ConnectionID != "" && t.ModelID != ""
}
func scopedHealthKey(scope core.HealthScope, target core.RouteTarget) string {
	return scope.TenantID + "\x00" + strconv.FormatInt(scope.Revision, 10) + "\x00" + key(target)
}
func scopedAffinityKey(scope core.HealthScope, affinity string) string {
	return scope.TenantID + "\x00" + strconv.FormatInt(scope.Revision, 10) + "\x00" + affinity
}

func (l *Local) SetScoped(ctx context.Context, scope core.HealthScope, target core.RouteTarget, healthy bool, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !scope.Valid() {
		return ErrScopeRequired
	}
	if !validTarget(target) {
		return fmt.Errorf("target identity is required")
	}
	ttl, err := boundTTL(ttl)
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.hints[scopedHealthKey(scope, target)] = hint{healthy: healthy, expires: time.Now().Add(ttl)}
	l.mu.Unlock()
	return nil
}
func (l *Local) HealthyScoped(ctx context.Context, scope core.HealthScope, target core.RouteTarget) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !scope.Valid() || !validTarget(target) {
		return true, nil
	}
	now := time.Now()
	k := scopedHealthKey(scope, target)
	l.mu.Lock()
	h, ok := l.hints[k]
	if ok && !now.Before(h.expires) {
		delete(l.hints, k)
		ok = false
	}
	l.mu.Unlock()
	if !ok {
		return true, nil
	}
	return h.healthy, nil
}
func (l *Local) GetAffinityScoped(ctx context.Context, scope core.HealthScope, affinity string) (core.RouteTarget, bool, error) {
	if err := ctx.Err(); err != nil {
		return core.RouteTarget{}, false, err
	}
	if !scope.Valid() || affinity == "" {
		return core.RouteTarget{}, false, nil
	}
	now := time.Now()
	k := scopedAffinityKey(scope, affinity)
	l.mu.Lock()
	v, ok := l.affinity[k]
	if ok && !now.Before(v.expires) {
		delete(l.affinity, k)
		ok = false
	}
	l.mu.Unlock()
	if !ok {
		return core.RouteTarget{}, false, nil
	}
	return v.target, true, nil
}
func (l *Local) SetAffinityScoped(ctx context.Context, scope core.HealthScope, affinity string, target core.RouteTarget, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !scope.Valid() {
		return ErrScopeRequired
	}
	if affinity == "" {
		return fmt.Errorf("affinity key is required")
	}
	if !validTarget(target) {
		return fmt.Errorf("target identity is required")
	}
	ttl, err := boundTTL(ttl)
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.affinity[scopedAffinityKey(scope, affinity)] = affinityHint{target: target, expires: time.Now().Add(ttl)}
	l.mu.Unlock()
	return nil
}
func key(t core.RouteTarget) string { return t.ConnectionID + "\x00" + t.ModelID + "\x00" + t.Region }

type Backend interface {
	Get(context.Context, string) ([]byte, error)
	Set(context.Context, string, []byte, time.Duration) error
	Publish(context.Context, string, []byte) error
}
type Subscriber interface {
	Subscribe(context.Context, string) (<-chan []byte, error)
}
type Remote struct {
	Backend         Backend
	Prefix, Channel string
}

type healthPayload struct {
	TenantID     string `json:"tenant_id"`
	Revision     int64  `json:"config_revision"`
	ConnectionID string `json:"connection_id"`
	ModelID      string `json:"model_id"`
	Region       string `json:"region"`
	Healthy      bool   `json:"healthy"`
}

func (r Remote) SetScoped(ctx context.Context, scope core.HealthScope, target core.RouteTarget, healthy bool, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !scope.Valid() {
		return ErrScopeRequired
	}
	if !validTarget(target) {
		return fmt.Errorf("target identity is required")
	}
	ttl, err := boundTTL(ttl)
	if err != nil {
		return err
	}
	if r.Backend == nil {
		return fmt.Errorf("coordination backend is required")
	}
	payload, err := json.Marshal(healthPayload{
		TenantID: scope.TenantID, Revision: scope.Revision,
		ConnectionID: target.ConnectionID, ModelID: target.ModelID, Region: target.Region,
		Healthy: healthy,
	})
	if err != nil {
		return err
	}
	if err = r.Backend.Set(ctx, r.Prefix+"health/"+scopedHealthKey(scope, target), payload, ttl); err != nil {
		return fmt.Errorf("store health hint: %w", err)
	}
	if r.Channel != "" {
		if err = r.Backend.Publish(ctx, r.Channel, payload); err != nil {
			return fmt.Errorf("publish health hint: %w", err)
		}
	}
	return nil
}
func (r Remote) HealthyScoped(ctx context.Context, scope core.HealthScope, target core.RouteTarget) (bool, error) {
	if r.Backend == nil {
		return true, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !scope.Valid() || !validTarget(target) {
		return true, nil
	}
	payload, err := r.Backend.Get(ctx, r.Prefix+"health/"+scopedHealthKey(scope, target))
	if errors.Is(err, ErrNotFound) || err != nil {
		return true, nil
	}
	var v healthPayload
	if json.Unmarshal(payload, &v) != nil ||
		v.TenantID != scope.TenantID || v.Revision != scope.Revision ||
		v.ConnectionID != target.ConnectionID || v.ModelID != target.ModelID || v.Region != target.Region {
		return true, nil
	}
	return v.Healthy, nil
}

type affinityPayload struct {
	TenantID string           `json:"tenant_id"`
	Revision int64            `json:"config_revision"`
	Target   core.RouteTarget `json:"target"`
}

func (r Remote) GetAffinityScoped(ctx context.Context, scope core.HealthScope, affinity string) (core.RouteTarget, bool, error) {
	if err := ctx.Err(); err != nil {
		return core.RouteTarget{}, false, err
	}
	if !scope.Valid() || affinity == "" || r.Backend == nil {
		return core.RouteTarget{}, false, nil
	}
	payload, err := r.Backend.Get(ctx, r.Prefix+"affinity/"+scopedAffinityKey(scope, affinity))
	if err != nil {
		return core.RouteTarget{}, false, nil
	}
	var v affinityPayload
	if json.Unmarshal(payload, &v) != nil || v.TenantID != scope.TenantID || v.Revision != scope.Revision || !validTarget(v.Target) {
		return core.RouteTarget{}, false, nil
	}
	return v.Target, true, nil
}
func (r Remote) SetAffinityScoped(ctx context.Context, scope core.HealthScope, affinity string, target core.RouteTarget, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !scope.Valid() {
		return ErrScopeRequired
	}
	if affinity == "" {
		return fmt.Errorf("affinity key is required")
	}
	if !validTarget(target) {
		return fmt.Errorf("target identity is required")
	}
	ttl, err := boundTTL(ttl)
	if err != nil {
		return err
	}
	if r.Backend == nil {
		return fmt.Errorf("coordination backend is required")
	}
	payload, err := json.Marshal(affinityPayload{TenantID: scope.TenantID, Revision: scope.Revision, Target: target})
	if err != nil {
		return err
	}
	return r.Backend.Set(ctx, r.Prefix+"affinity/"+scopedAffinityKey(scope, affinity), payload, ttl)
}
func (r Remote) Subscribe(ctx context.Context) (<-chan []byte, error) {
	s, ok := r.Backend.(Subscriber)
	if !ok || r.Channel == "" {
		return nil, fmt.Errorf("coordination backend does not support subscriptions")
	}
	return s.Subscribe(ctx, r.Channel)
}
func (r Remote) Publish(ctx context.Context, channel string, value []byte) error {
	if r.Backend == nil {
		return ErrNotFound
	}
	if channel == "" {
		return fmt.Errorf("coordination channel is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.Backend.Publish(ctx, channel, value)
}

// Hybrid keeps the local hint available when Redis is unavailable and bounds
// all request-path Redis work. Redis is never consulted for admission state.
type Hybrid struct {
	Local   *Local
	Remote  *Remote
	Timeout time.Duration
}

func (h *Hybrid) timeout() time.Duration {
	if h.Timeout <= 0 {
		return DefaultRemoteTimeout
	}
	return h.Timeout
}

func (h *Hybrid) HealthyScoped(ctx context.Context, scope core.HealthScope, target core.RouteTarget) (bool, error) {
	if h.Local != nil {
		healthy, err := h.Local.HealthyScoped(ctx, scope, target)
		if err != nil {
			return false, err
		}
		if !healthy {
			return false, nil
		}
	}
	if h.Remote == nil {
		return true, nil
	}
	rctx, cancel := context.WithTimeout(ctx, h.timeout())
	defer cancel()
	healthy, err := h.Remote.HealthyScoped(rctx, scope, target)
	if err != nil {
		return true, nil
	}
	return healthy, nil
}
func (h *Hybrid) SetScoped(ctx context.Context, scope core.HealthScope, target core.RouteTarget, healthy bool, ttl time.Duration) error {
	if !scope.Valid() {
		return ErrScopeRequired
	}
	if h.Local == nil {
		return fmt.Errorf("local hint store is required")
	}
	if err := h.Local.SetScoped(ctx, scope, target, healthy, ttl); err != nil {
		return err
	}
	if h.Remote != nil {
		rctx, cancel := context.WithTimeout(ctx, h.timeout())
		defer cancel()
		_ = h.Remote.SetScoped(rctx, scope, target, healthy, ttl)
	}
	return nil
}
func (h *Hybrid) GetAffinityScoped(ctx context.Context, scope core.HealthScope, affinity string) (core.RouteTarget, bool, error) {
	if h.Local != nil {
		target, ok, err := h.Local.GetAffinityScoped(ctx, scope, affinity)
		if err != nil {
			return core.RouteTarget{}, false, err
		}
		if ok {
			return target, true, nil
		}
	}
	if h.Remote == nil {
		return core.RouteTarget{}, false, nil
	}
	rctx, cancel := context.WithTimeout(ctx, h.timeout())
	defer cancel()
	target, ok, err := h.Remote.GetAffinityScoped(rctx, scope, affinity)
	if err != nil {
		return core.RouteTarget{}, false, nil
	}
	return target, ok, nil
}
func (h *Hybrid) SetAffinityScoped(ctx context.Context, scope core.HealthScope, affinity string, target core.RouteTarget, ttl time.Duration) error {
	if !scope.Valid() {
		return ErrScopeRequired
	}
	if h.Local == nil {
		return fmt.Errorf("local hint store is required")
	}
	if err := h.Local.SetAffinityScoped(ctx, scope, affinity, target, ttl); err != nil {
		return err
	}
	if h.Remote != nil {
		rctx, cancel := context.WithTimeout(ctx, h.timeout())
		defer cancel()
		_ = h.Remote.SetAffinityScoped(rctx, scope, affinity, target, ttl)
	}
	return nil
}

var _ core.ScopedRoutingHints = (*Local)(nil)
var _ core.ScopedRoutingHints = (*Remote)(nil)
var _ core.ScopedRoutingHints = (*Hybrid)(nil)
