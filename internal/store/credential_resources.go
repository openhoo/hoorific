package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"hoorific/internal/admin"
	"hoorific/internal/core"
)

// readCredentialMetadata projects only public envelope metadata. Ciphertext,
// refresh state, verifier material and plaintext are never serialized to admin.
func (s *Store) readCredentialMetadata(ctx context.Context, p core.Principal, id, cursor string, limit int, single bool) (out core.ResourcePage, err error) {
	out.Items = []core.Resource{}
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 200 {
		return out, storeError("invalid_limit", 400)
	}
	err = s.WithTx(ctx, func(tx *sql.Tx) error {
		current, e := s.resolveMembershipTx(ctx, tx, p.TenantID, p.SubjectID)
		if e != nil {
			return e
		}
		if e = authorize(current, "credentials", false); e != nil {
			return e
		}
		after, e := s.tenancyCursor(current, "credentials", cursor, limit)
		if e != nil {
			return e
		}
		query := "SELECT id,data FROM durable_records WHERE tenant_id=? AND kind='credential'"
		args := []any{current.TenantID}
		if single {
			query += " AND id=?"
			args = append(args, id)
		} else {
			query += " AND id>? ORDER BY id LIMIT ?"
			args = append(args, after, limit+1)
		}
		rows, e := tx.QueryContext(ctx, s.Query(query), args...)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var connectionID, raw string
			if e = rows.Scan(&connectionID, &raw); e != nil {
				return e
			}
			var stored storedCredential
			if e = json.Unmarshal([]byte(raw), &stored); e != nil {
				return e
			}
			record := stored.Record
			if record.Identity.TenantID != current.TenantID || record.Identity.ConnectionID != connectionID {
				return storeError("credential_identity_mismatch", 409)
			}
			metadata := admin.CredentialMetadataData{ConnectionID: connectionID, CredentialID: record.Identity.CredentialID, Provider: record.Identity.Provider, AccountID: record.Identity.AccountID, Status: record.Status, Version: record.Identity.Version, RotatedAt: record.RotatedAt}
			data, e := json.Marshal(metadata)
			if e != nil {
				return e
			}
			out.Items = append(out.Items, core.Resource{ID: connectionID, TenantID: current.TenantID, Kind: "credentials", Version: record.Identity.Version, Data: data})
		}
		if e = rows.Err(); e != nil {
			return e
		}
		if single && len(out.Items) == 0 {
			return storeError("not_found", 404)
		}
		if len(out.Items) > limit {
			out.Items = out.Items[:limit]
			out.NextCursor, e = s.encodeCursor(resourceCursor{current.TenantID, current.SubjectID, current.Role, "credentials", out.Items[limit-1].ID, limit, time.Now().Add(15 * time.Minute).Unix()})
		}
		return e
	})
	return
}
