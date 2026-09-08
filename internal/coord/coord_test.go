package coord

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"hoorific/internal/core"
)

type memoryBackend struct {
	mu       sync.Mutex
	values   map[string][]byte
	failGet  bool
	failSet  bool
	setKeys  []string
	pubCount int
}

func newMemoryBackend() *memoryBackend { return &memoryBackend{values: map[string][]byte{}} }

func (m *memoryBackend) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failGet {
		return nil, errors.New("backend unavailable")
	}
	value, ok := m.values[key]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), value...), nil
}
func (m *memoryBackend) Set(_ context.Context, key string, value []byte, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failSet {
		return errors.New("backend unavailable")
	}
	m.values[key] = append([]byte(nil), value...)
	m.setKeys = append(m.setKeys, key)
	return nil
}
func (m *memoryBackend) Publish(_ context.Context, _ string, _ []byte) error {
	m.mu.Lock()
	m.pubCount++
	m.mu.Unlock()
	return nil
}

func scopedTestTarget() core.RouteTarget {
	return core.RouteTarget{ConnectionID: "shared-connection", ModelID: "shared-model", Region: "us-test"}
}
func scopedTestScope(tenant string, revision int64) core.HealthScope {
	return core.HealthScope{TenantID: tenant, Revision: revision}
}

func TestLocalScopedHealthSeparatesTenantsAndRevisions(t *testing.T) {
	ctx := context.Background()
	local := NewLocal()
	target := scopedTestTarget()
	a := scopedTestScope("tenant-a", 7)
	b := scopedTestScope("tenant-b", 7)
	if err := local.SetScoped(ctx, a, target, false, time.Minute); err != nil {
		t.Fatal(err)
	}
	if healthy, err := local.HealthyScoped(ctx, a, target); err != nil || healthy {
		t.Fatalf("tenant A health = %v, %v; want false", healthy, err)
	}
	if healthy, err := local.HealthyScoped(ctx, b, target); err != nil || !healthy {
		t.Fatalf("tenant B health = %v, %v; want fail-open true", healthy, err)
	}
	if healthy, err := local.HealthyScoped(ctx, scopedTestScope("tenant-a", 8), target); err != nil || !healthy {
		t.Fatalf("new snapshot health = %v, %v; want true", healthy, err)
	}
}

func TestRemoteScopedHealthAndAffinityUseTenantAndRevision(t *testing.T) {
	ctx := context.Background()
	backend := newMemoryBackend()
	remote := Remote{Backend: backend, Prefix: "test/"}
	target := scopedTestTarget()
	a := scopedTestScope("tenant-a", 11)
	b := scopedTestScope("tenant-b", 11)
	if err := remote.SetScoped(ctx, a, target, false, time.Minute); err != nil {
		t.Fatal(err)
	}
	if healthy, err := remote.HealthyScoped(ctx, a, target); err != nil || healthy {
		t.Fatalf("tenant A remote health = %v, %v; want false", healthy, err)
	}
	if healthy, err := remote.HealthyScoped(ctx, b, target); err != nil || !healthy {
		t.Fatalf("tenant B remote health = %v, %v; want true", healthy, err)
	}
	if healthy, err := remote.HealthyScoped(ctx, scopedTestScope("tenant-a", 12), target); err != nil || !healthy {
		t.Fatalf("new remote revision health = %v, %v; want true", healthy, err)
	}
	affinityKey := "tenant-a\x00subject\x00alias"
	if err := remote.SetAffinityScoped(ctx, a, affinityKey, target, time.Minute); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := remote.GetAffinityScoped(ctx, a, affinityKey); err != nil || !ok || got != target {
		t.Fatalf("tenant A affinity = %+v, %v, %v; want target", got, ok, err)
	}
	if _, ok, err := remote.GetAffinityScoped(ctx, b, affinityKey); err != nil || ok {
		t.Fatalf("tenant B affinity = %v, %v; want absent", ok, err)
	}
	if _, ok, err := remote.GetAffinityScoped(ctx, scopedTestScope("tenant-a", 12), affinityKey); err != nil || ok {
		t.Fatalf("new revision affinity = %v, %v; want absent", ok, err)
	}
}

func TestHybridScopedHealthFailsOpenWhenRemoteUnavailable(t *testing.T) {
	ctx := context.Background()
	local := NewLocal()
	backend := newMemoryBackend()
	backend.failSet = true
	backend.failGet = true
	hybrid := &Hybrid{Local: local, Remote: &Remote{Backend: backend, Prefix: "test/"}, Timeout: time.Millisecond}
	target := scopedTestTarget()
	scope := scopedTestScope("tenant-a", 3)
	if err := hybrid.SetScoped(ctx, scope, target, false, time.Minute); err != nil {
		t.Fatal(err)
	}
	if healthy, err := hybrid.HealthyScoped(ctx, scope, target); err != nil || healthy {
		t.Fatalf("local cooldown through hybrid = %v, %v; want false", healthy, err)
	}
	if healthy, err := hybrid.HealthyScoped(ctx, scopedTestScope("tenant-b", 3), target); err != nil || !healthy {
		t.Fatalf("other tenant through hybrid = %v, %v; want true", healthy, err)
	}
}
