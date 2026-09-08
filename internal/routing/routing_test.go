package routing

import (
	"context"
	"testing"

	"hoorific/internal/coord"
	"hoorific/internal/core"
)

func scopedSnapshot(tenant string, revision, connectionVersion int64) (core.RuntimeSnapshot, core.RouteTarget) {
	target := core.RouteTarget{ConnectionID: "shared-connection", ModelID: "shared-model", Priority: 0, Weight: 1}
	return core.RuntimeSnapshot{
		Revision: revision,
		Connections: map[string]core.Connection{
			target.ConnectionID: {TenantID: tenant, ID: target.ConnectionID, Version: connectionVersion, Connector: "fixture", AccountID: "shared-account"},
		},
		Models: map[string]core.Model{
			target.ModelID: {ID: "upstream-model", CatalogID: target.ModelID, ConnectionID: target.ConnectionID, Operations: []core.Operation{"generate"}},
		},
		Aliases:       map[string][]core.RouteTarget{"model": {target}},
		RoutePolicies: map[string]core.RoutePolicy{"model": {}},
	}, target
}

func TestOrderScopesHealthByTenantAndSnapshotRevision(t *testing.T) {
	ctx := context.Background()
	hints := coord.NewLocal()
	snapshotA, target := scopedSnapshot("tenant-a", 7, 1)
	snapshotB, _ := scopedSnapshot("tenant-b", 7, 1)
	scopeA, ok := TargetScope(snapshotA, target)
	if !ok {
		t.Fatal("tenant A route scope is invalid")
	}
	if err := hints.SetScoped(ctx, scopeA, target, false, coord.MaxHintTTL); err != nil {
		t.Fatal(err)
	}
	if _, err := Order(ctx, snapshotA, "model", Requirements{Operation: "generate"}, hints); err == nil {
		t.Fatal("tenant A unhealthy hint must suppress its only target")
	}
	ordered, err := Order(ctx, snapshotB, "model", Requirements{Operation: "generate"}, hints)
	if err != nil || len(ordered) != 1 || ordered[0] != target {
		t.Fatalf("tenant B order = %+v, %v; want shared target", ordered, err)
	}
	snapshotA2, _ := scopedSnapshot("tenant-a", 8, 2)
	ordered, err = Order(ctx, snapshotA2, "model", Requirements{Operation: "generate"}, hints)
	if err != nil || len(ordered) != 1 || ordered[0] != target {
		t.Fatalf("reconfigured tenant A order = %+v, %v; want target outside old cooldown", ordered, err)
	}
}

func TestOrderRetainsHealthFailOpenSemantics(t *testing.T) {
	ctx := context.Background()
	snapshot, target := scopedSnapshot("tenant-a", 1, 1)
	hints := failingScopedHealth{}
	ordered, err := Order(ctx, snapshot, "model", Requirements{Operation: "generate"}, hints)
	if err != nil || len(ordered) != 1 || ordered[0] != target {
		t.Fatalf("health backend error should fail open: order = %+v, %v", ordered, err)
	}
}

type failingScopedHealth struct{}

func (failingScopedHealth) HealthyScoped(context.Context, core.HealthScope, core.RouteTarget) (bool, error) {
	return false, context.DeadlineExceeded
}
