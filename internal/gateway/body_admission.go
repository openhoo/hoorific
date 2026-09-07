package gateway

import (
	"context"
	"net/http"
	"strings"

	"hoorific/internal/core"
)

type tenantPolicyKey struct{}

func withTenantPolicy(ctx context.Context, policy core.TenantPolicy) context.Context {
	return context.WithValue(ctx, tenantPolicyKey{}, policy)
}
func downwardLimit(global, tenant int64) int64 {
	if tenant > 0 && tenant < global {
		return tenant
	}
	return global
}
func bodyLimit(ctx context.Context, upload bool) int64 {
	policy, _ := ctx.Value(tenantPolicyKey{}).(core.TenantPolicy)
	global := int64(32 << 20)
	if upload {
		global = 1 << 30
	}
	return downwardLimit(global, policy.MaxBodyBytes)
}
func eventLimit(ctx context.Context) int64 {
	policy, _ := ctx.Value(tenantPolicyKey{}).(core.TenantPolicy)
	return downwardLimit(1<<20, policy.MaxEventBytes)
}

// Reserve actual known body size plus bounded codec working space. Small long-
// lived streams do not consume an entire 32-MiB JSON admission slot.
func (g *Gateway) admitBody(r *http.Request) (func(), error) {
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	upload := ct != "" && !strings.Contains(ct, "json")
	size := r.ContentLength
	limit := bodyLimit(r.Context(), upload)
	if size > limit {
		return nil, failure("invalid_request", 413, "request body limit exceeded")
	}
	if size < 0 {
		size = min(limit, 32<<20)
	}
	reserve := max(int64(64<<10), size*3)
	if upload {
		reserve = 8 << 20
		select {
		case g.uploads <- struct{}{}:
		default:
			return nil, failure("quota_exceeded", 429, "upload concurrency exhausted")
		}
	}
	for {
		used := g.bodyMemory.Load()
		if reserve > 128<<20-used {
			if upload {
				<-g.uploads
			}
			return nil, failure("quota_exceeded", 429, "body memory admission exhausted")
		}
		if g.bodyMemory.CompareAndSwap(used, used+reserve) {
			break
		}
	}
	return func() {
		g.bodyMemory.Add(-reserve)
		if upload {
			<-g.uploads
		}
	}, nil
}
