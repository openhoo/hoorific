package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	"hoorific/internal/admin"
	"hoorific/internal/core"
	"hoorific/internal/credential"
	"hoorific/internal/resource"
)

const durableProjectionLimit = 10000

func operationalKind(kind string) bool {
	switch kind {
	case "usage_ledger", "audit_events", "admissions", "reconciliations":
		return true
	default:
		return durableOperationalKind(kind)
	}
}

func durableOperationalKind(kind string) bool {
	return kind == "upstream_operations" || kind == "oauth_sessions"
}

type durableProjection struct {
	resource core.Resource
	cursor   string
	sourceID string
}

// readDurableOperationalResources projects the durable_records rows that back
// provider jobs and OAuth login attempts. The cursor contains only a digest of
// the durable key: OAuth state hashes and credential coordinates never appear
// in an API response, including a pagination token.
func (s *Store) readDurableOperationalResources(ctx context.Context, p core.Principal, kind, id, cursor string, limit int, single bool) (core.ResourcePage, error) {
	out := core.ResourcePage{Items: []core.Resource{}}
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 500 {
		return out, storeError("invalid_limit", 400)
	}
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		current, err := s.resolveMembershipTx(ctx, tx, p.TenantID, p.SubjectID)
		if err != nil {
			return err
		}
		p = current
		if err = authorize(p, kind, false); err != nil {
			return err
		}
		after, err := s.tenancyCursor(p, kind, cursor, limit)
		if err != nil {
			return err
		}

		durableKind := "resource_job"
		if kind == "oauth_sessions" {
			durableKind = "credential_login"
		}
		rows, err := tx.QueryContext(ctx, s.Query("SELECT id,version,data FROM durable_records WHERE tenant_id=? AND kind=? ORDER BY id LIMIT ?"), p.TenantID, durableKind, durableProjectionLimit+1)
		if err != nil {
			return err
		}
		defer rows.Close()

		items := make([]durableProjection, 0)
		now := time.Now()
		count := 0
		for rows.Next() {
			count++
			if count > durableProjectionLimit {
				return storeError("too_many_operational_records", 503)
			}
			var sourceID, data string
			var version int64
			if err = rows.Scan(&sourceID, &version, &data); err != nil {
				return err
			}
			item, err := projectDurableOperationalResource(kind, p.TenantID, sourceID, version, []byte(data), now)
			if err != nil {
				return err
			}
			item.cursor = durableProjectionCursor(kind, p.TenantID, sourceID)
			items = append(items, item)
		}
		if err = rows.Err(); err != nil {
			return err
		}

		if single {
			matches := 0
			for _, item := range items {
				if item.resource.ID != id {
					continue
				}
				matches++
				out.Items = append(out.Items, item.resource)
			}
			switch matches {
			case 0:
				return storeError("not_found", 404)
			case 1:
				return nil
			default:
				return storeError("conflict", 409)
			}
		}

		// A digest is used for ordering and resume so the signed cursor does not
		// disclose the durable key. sourceID remains an internal tie-breaker.
		sort.Slice(items, func(i, j int) bool {
			if items[i].cursor != items[j].cursor {
				return items[i].cursor < items[j].cursor
			}
			return items[i].sourceID < items[j].sourceID
		})
		eligible := make([]durableProjection, 0, len(items))
		for _, item := range items {
			if after != "" && item.cursor <= after {
				continue
			}
			eligible = append(eligible, item)
		}
		if len(eligible) > limit {
			out.Items = make([]core.Resource, limit)
			for i := range out.Items {
				out.Items[i] = eligible[i].resource
			}
			out.NextCursor, err = s.encodeCursor(resourceCursor{p.TenantID, p.SubjectID, p.Role, kind, eligible[limit-1].cursor, limit, time.Now().Add(15 * time.Minute).Unix()})
			return err
		}
		for _, item := range eligible {
			out.Items = append(out.Items, item.resource)
		}
		return nil
	})
	if err != nil {
		return core.ResourcePage{Items: []core.Resource{}}, err
	}
	return out, nil
}

func durableProjectionCursor(kind, tenant, sourceID string) string {
	sum := sha256.Sum256([]byte("hoorific/admin/durable-cursor/v1\x00" + kind + "\x00" + tenant + "\x00" + sourceID))
	return hex.EncodeToString(sum[:])
}

func oauthSessionProjectionID(tenant, stateHash string) string {
	sum := sha256.Sum256([]byte("hoorific/admin/oauth-session/v1\x00" + tenant + "\x00" + stateHash))
	return hex.EncodeToString(sum[:])
}

func projectDurableOperationalResource(kind, tenant, sourceID string, version int64, raw []byte, now time.Time) (durableProjection, error) {
	if tenant == "" || sourceID == "" || version < 1 {
		return durableProjection{}, invalidOperationalRecord()
	}
	switch kind {
	case "upstream_operations":
		var job resource.Job
		if json.Unmarshal(raw, &job) != nil || job.TenantID != tenant || job.ConnectionID == "" || job.AccountID == "" || job.ResourceID == "" || job.Operation == "" || job.Status == "" || sourceID != jobID(job) {
			return durableProjection{}, invalidOperationalRecord()
		}
		data, err := json.Marshal(admin.UpstreamOperationData{
			ConnectionID: job.ConnectionID,
			Operation:    job.Operation,
			ResourceID:   job.ResourceID,
			Status:       job.Status,
		})
		if err != nil {
			return durableProjection{}, invalidOperationalRecord()
		}
		return durableProjection{
			resource: core.Resource{ID: job.ResourceID, TenantID: tenant, Kind: kind, Version: version, Data: data},
			sourceID: sourceID,
		}, nil
	case "oauth_sessions":
		var session credential.LoginSession
		if json.Unmarshal(raw, &session) != nil || session.TenantID != tenant || session.StateHash == "" || session.AdminSessionID == "" || session.Connector == "" || sourceID != session.StateHash || session.ExpiresAt.IsZero() {
			return durableProjection{}, invalidOperationalRecord()
		}
		status := "pending"
		if !session.ExpiresAt.After(now) {
			status = "expired"
		}
		data, err := json.Marshal(admin.OAuthSessionData{
			Connector: session.Connector,
			Status:    status,
			ExpiresAt: session.ExpiresAt.UTC().Format(time.RFC3339Nano),
		})
		if err != nil {
			return durableProjection{}, invalidOperationalRecord()
		}
		return durableProjection{
			resource: core.Resource{ID: oauthSessionProjectionID(tenant, session.StateHash), TenantID: tenant, Kind: kind, Version: version, Data: data},
			sourceID: sourceID,
		}, nil
	default:
		return durableProjection{}, storeError("invalid_kind", 400)
	}
}
