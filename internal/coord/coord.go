// Package coord contains disposable coordination hints, never admission state.
package coord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hoorific/internal/core"
	"sync"
	"time"
)

var ErrNotFound = errors.New("coordination hint not found")

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
func (l *Local) Set(ctx context.Context, target core.RouteTarget, healthy bool, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ttl, err := boundTTL(ttl)
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.hints[key(target)] = hint{healthy: healthy, expires: time.Now().Add(ttl)}
	l.mu.Unlock()
	return nil
}
func (l *Local) Healthy(ctx context.Context, target core.RouteTarget) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	now := time.Now()
	k := key(target)
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
func (l *Local) GetAffinity(ctx context.Context, key string) (core.RouteTarget, bool, error) {
	if err := ctx.Err(); err != nil {
		return core.RouteTarget{}, false, err
	}
	if key == "" {
		return core.RouteTarget{}, false, nil
	}
	now := time.Now()
	l.mu.Lock()
	v, ok := l.affinity[key]
	if ok && !now.Before(v.expires) {
		delete(l.affinity, key)
		ok = false
	}
	l.mu.Unlock()
	if !ok {
		return core.RouteTarget{}, false, nil
	}
	return v.target, true, nil
}
func (l *Local) SetAffinity(ctx context.Context, key string, target core.RouteTarget, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if key == "" {
		return fmt.Errorf("affinity key is required")
	}
	ttl, err := boundTTL(ttl)
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.affinity[key] = affinityHint{target: target, expires: time.Now().Add(ttl)}
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

func (r Remote) Set(ctx context.Context, target core.RouteTarget, healthy bool, ttl time.Duration) error {
	if r.Backend == nil {
		return fmt.Errorf("coordination backend is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ttl, err := boundTTL(ttl)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(struct {
		ConnectionID string `json:"connection_id"`
		ModelID      string `json:"model_id"`
		Region       string `json:"region"`
		Healthy      bool   `json:"healthy"`
	}{target.ConnectionID, target.ModelID, target.Region, healthy})
	if err != nil {
		return err
	}
	if err = r.Backend.Set(ctx, r.Prefix+"health/"+key(target), payload, ttl); err != nil {
		return fmt.Errorf("store health hint: %w", err)
	}
	if r.Channel != "" {
		if err = r.Backend.Publish(ctx, r.Channel, payload); err != nil {
			return fmt.Errorf("publish health hint: %w", err)
		}
	}
	return nil
}
func (r Remote) Healthy(ctx context.Context, target core.RouteTarget) (bool, error) {
	if r.Backend == nil {
		return true, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	payload, err := r.Backend.Get(ctx, r.Prefix+"health/"+key(target))
	if errors.Is(err, ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return true, nil
	}
	var v struct {
		ConnectionID *string `json:"connection_id"`
		ModelID      *string `json:"model_id"`
		Region       *string `json:"region"`
		Healthy      *bool   `json:"healthy"`
	}
	if json.Unmarshal(payload, &v) != nil || v.ConnectionID == nil || v.ModelID == nil || v.Region == nil || v.Healthy == nil || *v.ConnectionID != target.ConnectionID || *v.ModelID != target.ModelID || *v.Region != target.Region {
		return true, nil
	}
	return *v.Healthy, nil
}
func (r Remote) GetAffinity(ctx context.Context, key string) (core.RouteTarget, bool, error) {
	if err := ctx.Err(); err != nil {
		return core.RouteTarget{}, false, err
	}
	if key == "" || r.Backend == nil {
		return core.RouteTarget{}, false, nil
	}
	payload, err := r.Backend.Get(ctx, r.Prefix+"affinity/"+key)
	if err != nil {
		return core.RouteTarget{}, false, nil
	}
	var v struct {
		Target core.RouteTarget `json:"target"`
	}
	if json.Unmarshal(payload, &v) != nil || v.Target.ConnectionID == "" || v.Target.ModelID == "" {
		return core.RouteTarget{}, false, nil
	}
	return v.Target, true, nil
}
func (r Remote) SetAffinity(ctx context.Context, key string, target core.RouteTarget, ttl time.Duration) error {
	if r.Backend == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if key == "" {
		return fmt.Errorf("affinity key is required")
	}
	ttl, err := boundTTL(ttl)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(struct {
		Target core.RouteTarget `json:"target"`
	}{target})
	if err != nil {
		return err
	}
	return r.Backend.Set(ctx, r.Prefix+"affinity/"+key, payload, ttl)
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
func (h *Hybrid) Healthy(ctx context.Context, target core.RouteTarget) (bool, error) {
	if h.Local != nil {
		healthy, err := h.Local.Healthy(ctx, target)
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
	healthy, err := h.Remote.Healthy(rctx, target)
	if err != nil {
		return true, nil
	}
	return healthy, nil
}
func (h *Hybrid) Set(ctx context.Context, target core.RouteTarget, healthy bool, ttl time.Duration) error {
	if h.Local == nil {
		return fmt.Errorf("local hint store is required")
	}
	if err := h.Local.Set(ctx, target, healthy, ttl); err != nil {
		return err
	}
	if h.Remote != nil {
		rctx, cancel := context.WithTimeout(ctx, h.timeout())
		defer cancel()
		_ = h.Remote.Set(rctx, target, healthy, ttl)
	}
	return nil
}
func (h *Hybrid) GetAffinity(ctx context.Context, key string) (core.RouteTarget, bool, error) {
	if h.Local != nil {
		target, ok, err := h.Local.GetAffinity(ctx, key)
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
	target, ok, err := h.Remote.GetAffinity(rctx, key)
	if err != nil {
		return core.RouteTarget{}, false, nil
	}
	return target, ok, nil
}
func (h *Hybrid) SetAffinity(ctx context.Context, key string, target core.RouteTarget, ttl time.Duration) error {
	if h.Local == nil {
		return fmt.Errorf("local hint store is required")
	}
	if err := h.Local.SetAffinity(ctx, key, target, ttl); err != nil {
		return err
	}
	if h.Remote != nil {
		rctx, cancel := context.WithTimeout(ctx, h.timeout())
		defer cancel()
		_ = h.Remote.SetAffinity(rctx, key, target, ttl)
	}
	return nil
}

var _ interface {
	Healthy(context.Context, core.RouteTarget) (bool, error)
	Set(context.Context, core.RouteTarget, bool, time.Duration) error
	GetAffinity(context.Context, string) (core.RouteTarget, bool, error)
	SetAffinity(context.Context, string, core.RouteTarget, time.Duration) error
} = (*Local)(nil)
var _ interface {
	Healthy(context.Context, core.RouteTarget) (bool, error)
	Set(context.Context, core.RouteTarget, bool, time.Duration) error
	GetAffinity(context.Context, string) (core.RouteTarget, bool, error)
	SetAffinity(context.Context, string, core.RouteTarget, time.Duration) error
} = (*Hybrid)(nil)
