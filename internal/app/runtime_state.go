package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"hoorific/internal/catalog"
	"hoorific/internal/coord"
	"hoorific/internal/core"
)

// snapshotStore is intentionally small: SQL remains authoritative and the
// runtime only caches fully compiled, immutable tenant snapshots.
type snapshotStore interface {
	Snapshot(context.Context) (core.RuntimeSnapshot, error)
	ConfigRevision(context.Context, string) (int64, error)
	SetConfigListener(func(tenant string, revision int64))
}

// HintStore is the sole production contract for tenant/revision-scoped
// disposable health and affinity hints.
type HintStore interface {
	core.ScopedRoutingHints
}
type invalidationSubscriber interface {
	Subscribe(context.Context) (<-chan []byte, error)
}

type cachedSnapshot struct {
	catalog  *catalog.Catalog
	revision int64
}

// RuntimeState owns process-local immutable catalog snapshots and disposable
// routing hints. It never admits a request and never treats Redis as durable.
type RuntimeState struct {
	Snapshots  core.SnapshotSource
	Hints      HintStore
	store      snapshotStore
	remote     *coord.Remote
	redis      redis.UniversalClient
	mu         sync.RWMutex
	cache      map[string]cachedSnapshot
	generation map[string]uint64
	runMu      sync.Mutex
	cancel     context.CancelFunc
	closed     bool
}

type invalidationMessage struct {
	Tenant   string `json:"tenant"`
	Revision int64  `json:"revision"`
}

func NewRuntimeState(cfg core.BootstrapConfig, db snapshotStore) (*RuntimeState, error) {
	if db == nil {
		return nil, fmt.Errorf("runtime snapshot store is required")
	}
	s := &RuntimeState{store: db, cache: make(map[string]cachedSnapshot), generation: make(map[string]uint64)}
	local := coord.NewLocal()
	if cfg.Mode == "cluster" {
		if cfg.Coordination.Redis.URLFile == "" {
			return nil, fmt.Errorf("cluster coordination Redis URL file is required")
		}
		raw, err := os.ReadFile(cfg.Coordination.Redis.URLFile)
		if err != nil {
			return nil, fmt.Errorf("read Redis URL file: %w", err)
		}
		u, err := redis.ParseURL(string(trimSpace(raw)))
		if err != nil {
			return nil, fmt.Errorf("parse Redis URL: %w", err)
		}
		client := redis.NewClient(u)
		s.redis = client
		remote := &coord.Remote{Backend: coord.RedisBackend{Client: client}, Prefix: "hoorific/", Channel: "hoorific/config"}
		s.remote = remote
		s.Hints = &coord.Hybrid{Local: local, Remote: remote, Timeout: coord.DefaultRemoteTimeout}
	} else {
		s.Hints = local
	}
	s.Snapshots = s
	db.SetConfigListener(s.onConfigCommit)
	return s, nil
}
func trimSpace(b []byte) string { return strings.TrimSpace(string(b)) }

func (s *RuntimeState) Snapshot(ctx context.Context) (core.RuntimeSnapshot, error) {
	p, ok := core.PrincipalFromContext(ctx)
	if !ok || p.TenantID == "" {
		return core.RuntimeSnapshot{}, fmt.Errorf("tenant principal required")
	}
	for range 2 {
		s.mu.RLock()
		entry, ok := s.cache[p.TenantID]
		generation := s.generation[p.TenantID]
		s.mu.RUnlock()
		if ok {
			return entry.catalog.Snapshot(), nil
		}
		raw, err := s.store.Snapshot(ctx)
		if err != nil {
			return core.RuntimeSnapshot{}, err
		}
		if raw.Revision < 0 {
			return core.RuntimeSnapshot{}, fmt.Errorf("invalid snapshot revision")
		}
		compiled, err := catalog.Compile(raw)
		if err != nil {
			return core.RuntimeSnapshot{}, fmt.Errorf("compile runtime snapshot: %w", err)
		}
		// Never replace a newer publication with an older SQL read, and retry when
		// a post-commit invalidation raced this load.
		s.mu.Lock()
		if s.generation[p.TenantID] != generation {
			s.mu.Unlock()
			continue
		}
		current, exists := s.cache[p.TenantID]
		if !exists || compiled.Revision() >= current.revision {
			s.cache[p.TenantID] = cachedSnapshot{catalog: compiled, revision: compiled.Revision()}
		}
		entry = s.cache[p.TenantID]
		s.mu.Unlock()
		return entry.catalog.Snapshot(), nil
	}
	return core.RuntimeSnapshot{}, fmt.Errorf("runtime snapshot changed during load")
}

// RefreshSnapshot bypasses disposable hints after SQL rejects a stale plan.
func (s *RuntimeState) RefreshSnapshot(ctx context.Context) (core.RuntimeSnapshot, error) {
	p, ok := core.PrincipalFromContext(ctx)
	if !ok || p.TenantID == "" {
		return core.RuntimeSnapshot{}, fmt.Errorf("tenant principal required")
	}
	s.invalidate(p.TenantID, 0)
	return s.Snapshot(ctx)
}
func (s *RuntimeState) invalidate(tenant string, revision int64) {
	if tenant == "" {
		return
	}
	s.mu.Lock()
	s.generation[tenant]++
	if current, ok := s.cache[tenant]; ok && (revision <= 0 || current.revision < revision) {
		delete(s.cache, tenant)
	}
	s.mu.Unlock()
}
func (s *RuntimeState) onConfigCommit(tenant string, revision int64) {
	if tenant == "" {
		return
	}
	s.mu.Lock()
	// The local writer knows this commit completed; invalidate even if a
	// backend reports an unexpectedly unchanged revision.
	// Health and affinity hints include this global revision in their scoped
	// keys. A committed connection, account, or credential mutation therefore
	// re-scopes all old hints without a request-path SQL lookup or a delete
	// round-trip to Redis. The global counter advances for one tenant, so this
	// intentionally invalidates disposable hints for other tenants too.
	s.generation[tenant]++
	delete(s.cache, tenant)
	s.mu.Unlock()
	payload, err := json.Marshal(invalidationMessage{Tenant: tenant, Revision: revision})
	if err != nil {
		return
	}
	if s.remote == nil {
		return
	}
	// Publication is a freshness hint; SQL commit already succeeded. Do not
	// make a successful config mutation depend on Redis availability.
	publishCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	_ = s.remote.Publish(publishCtx, s.remote.Channel, payload)
}

func (s *RuntimeState) Run(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("runtime context is required")
	}
	s.runMu.Lock()
	if s.closed {
		s.runMu.Unlock()
		return fmt.Errorf("runtime state is closed")
	}
	if s.cancel != nil {
		s.runMu.Unlock()
		return fmt.Errorf("runtime state already running")
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.runMu.Unlock()
	defer cancel()
	defer func() { s.runMu.Lock(); s.cancel = nil; s.runMu.Unlock() }()
	var sub <-chan []byte
	if s.remote != nil {
		if ch, err := s.remote.Subscribe(runCtx); err == nil {
			sub = ch
		}
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-runCtx.Done():
			return runCtx.Err()
		case msg, ok := <-sub:
			if !ok {
				sub = nil
				continue
			}
			s.applyInvalidation(msg)
		case <-ticker.C:
			s.repair(runCtx)
		}
	}
}
func (s *RuntimeState) applyInvalidation(raw []byte) {
	var msg invalidationMessage
	if json.Unmarshal(raw, &msg) == nil {
		s.invalidate(msg.Tenant, msg.Revision)
	}
}
func (s *RuntimeState) repair(ctx context.Context) {
	s.mu.RLock()
	tenants := make([]string, 0, len(s.cache))
	for tenant := range s.cache {
		tenants = append(tenants, tenant)
	}
	s.mu.RUnlock()
	for _, tenant := range tenants {
		if ctx.Err() != nil {
			return
		}
		rev, err := s.store.ConfigRevision(ctx, tenant)
		if err != nil {
			continue
		}
		s.mu.RLock()
		entry, ok := s.cache[tenant]
		s.mu.RUnlock()
		if ok && rev > entry.revision {
			s.invalidate(tenant, rev)
		}
	}
}
func (s *RuntimeState) Close() error {
	s.runMu.Lock()
	if s.closed {
		s.runMu.Unlock()
		return nil
	}
	s.closed = true
	if s.cancel != nil {
		s.cancel()
	}
	s.runMu.Unlock()
	if s.redis != nil {
		return s.redis.Close()
	}
	return nil
}

var _ core.SnapshotSource = (*RuntimeState)(nil)
