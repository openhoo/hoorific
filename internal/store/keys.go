package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"hoorific/internal/admin"
	"hoorific/internal/core"
)

var _ core.KeyAuthenticator = (*Store)(nil)

// A token carries only its lookup coordinates and 256 random secret bits.
// The verifier covers the coordinates too; no token is persisted or audited.
func newKeyToken(tenant, id string) (string, string, error) {
	if !validKeyCoordinate(tenant) || !validKeyCoordinate(id) {
		return "", "", problem("invalid_key", 400, "invalid key coordinates")
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", "", err
	}
	enc := base64.RawURLEncoding
	publicID := enc.EncodeToString([]byte(tenant)) + "." + enc.EncodeToString([]byte(id))
	token := "hg_" + publicID + "_" + enc.EncodeToString(secret[:])
	sum := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(sum[:]), nil
}

func validKeyCoordinate(value string) bool {
	return value != "" && len(value) <= 512 && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func parseKeyToken(token string) (string, string, bool) {
	if len(token) > 1500 {
		return "", "", false
	}
	parts := strings.SplitN(token, "_", 3)
	if len(parts) != 3 || parts[0] != "hg" {
		return "", "", false
	}
	enc := base64.RawURLEncoding.Strict()
	ids := strings.Split(parts[1], ".")
	if len(ids) != 2 {
		return "", "", false
	}
	tenant, err := enc.DecodeString(ids[0])
	if err != nil || enc.EncodeToString(tenant) != ids[0] || !validKeyCoordinate(string(tenant)) {
		return "", "", false
	}
	id, err := enc.DecodeString(ids[1])
	if err != nil || enc.EncodeToString(id) != ids[1] || !validKeyCoordinate(string(id)) {
		return "", "", false
	}
	secret, err := enc.DecodeString(parts[2])
	if err != nil || len(secret) != 32 || enc.EncodeToString(secret) != parts[2] {
		return "", "", false
	}
	return string(tenant), string(id), true
}

type keyData struct {
	Name          string           `json:"name,omitempty"`
	Role          string           `json:"role"`
	Permissions   []string         `json:"permissions"`
	Aliases       []string         `json:"aliases,omitempty"`
	Connections   []string         `json:"connections,omitempty"`
	Operations    []core.Operation `json:"operations,omitempty"`
	Portable      bool             `json:"portable"`
	NativeAccount bool             `json:"native_account"`
	Realtime      bool             `json:"realtime"`
}

func decodeKeyData(data json.RawMessage) (keyData, error) {
	var kd keyData
	if len(data) == 0 || string(data) == "null" {
		return kd, storeError("invalid_key_metadata", 400)
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&kd); err != nil || kd.Role == "" {
		return kd, storeError("invalid_key_metadata", 400)
	}
	switch kd.Role {
	case "viewer", "operator", "admin":
	default:
		return kd, storeError("invalid_key_metadata", 400)
	}
	if len(kd.Permissions) == 0 || !validKeyOperations(kd.Operations) {
		return kd, storeError("invalid_key_metadata", 400)
	}
	return kd, nil
}

func issuerAllowsKey(p core.Principal, kd keyData) bool {
	rank := map[string]int{"viewer": 1, "operator": 2, "admin": 3, "owner": 4}
	if rank[p.Role] < rank[kd.Role] {
		return false
	}
	allowed := func(v string) bool {
		for _, x := range p.Permissions {
			if x == "*" || x == v {
				return true
			}
		}
		return false
	}
	for _, v := range kd.Permissions {
		if !allowed(v) {
			return false
		}
	}
	for _, v := range kd.Aliases {
		if !allowed("alias:"+v) && !allowed("aliases:*") && !allowed("*") {
			return false
		}
	}
	for _, v := range kd.Connections {
		if !allowed("connection:"+v) && !allowed("connections:*") && !allowed("*") {
			return false
		}
	}
	for _, v := range kd.Operations {
		if !allowed("operation:"+string(v)) && !allowed("operations:*") && !allowed("*") {
			return false
		}
	}
	return true
}

func principalForKey(tenant, id string, version int64, kd keyData) core.Principal {
	return core.Principal{
		TenantID: tenant, SubjectID: id, KeyID: id, KeyRevision: version,
		Role: kd.Role, Permissions: append([]string(nil), kd.Permissions...),
		Aliases: append([]string(nil), kd.Aliases...), Connections: append([]string(nil), kd.Connections...),
		Operations: append([]core.Operation(nil), kd.Operations...), Portable: kd.Portable,
		NativeAccount: kd.NativeAccount, Realtime: kd.Realtime,
	}
}

func (s *Store) resolveActiveKeyTx(ctx context.Context, tx *sql.Tx, tenant, id string, expectedVersion int64) (core.Principal, error) {
	if tenant == "" || id == "" || expectedVersion <= 0 {
		return core.Principal{}, storeError("unauthorized", 401)
	}
	var tenantData string
	var tenantState admin.TenantData
	if err := tx.QueryRowContext(ctx, s.Query("SELECT data FROM tenants WHERE id=?"), tenant).Scan(&tenantData); err != nil {
		return core.Principal{}, storeError("unauthorized", 401)
	}
	if json.Unmarshal([]byte(tenantData), &tenantState) != nil || !tenantState.Enabled {
		return core.Principal{}, storeError("unauthorized", 401)
	}
	var version int64
	var data string
	var revoked int
	if err := tx.QueryRowContext(ctx, s.Query("SELECT version,data,revoked FROM api_keys WHERE tenant_id=? AND id=?"), tenant, id).Scan(&version, &data, &revoked); err != nil || revoked != 0 || version != expectedVersion {
		return core.Principal{}, storeError("unauthorized", 401)
	}
	var resourceVersion int64
	var resourceData string
	if err := tx.QueryRowContext(ctx, s.Query("SELECT version,data FROM resources WHERE tenant_id=? AND kind='api_keys' AND id=?"), tenant, id).Scan(&resourceVersion, &resourceData); err != nil || resourceVersion != version {
		return core.Principal{}, storeError("unauthorized", 401)
	}
	kd, err := decodeKeyData(json.RawMessage(resourceData))
	if err != nil {
		return core.Principal{}, storeError("unauthorized", 401)
	}
	return principalForKey(tenant, id, version, kd), nil
}

func (s *Store) resolvePrincipalTx(ctx context.Context, tx *sql.Tx, p core.Principal) (core.Principal, error) {
	if p.KeyID != "" {
		return s.resolveActiveKeyTx(ctx, tx, p.TenantID, p.KeyID, p.KeyRevision)
	}
	current, err := s.resolveMembershipTx(ctx, tx, p.TenantID, p.SubjectID)
	if err != nil {
		return core.Principal{}, err
	}
	current.SessionID = p.SessionID
	current.Permissions = append([]string(nil), p.Permissions...)
	return current, nil
}

func (s *Store) AuthenticateKey(ctx context.Context, token string) (out core.Principal, err error) {
	tenant, id, ok := parseKeyToken(token)
	if !ok {
		return out, storeError("unauthorized", 401)
	}
	sum := sha256.Sum256([]byte(token))
	present := hex.EncodeToString(sum[:])
	err = s.WithTx(ctx, func(tx *sql.Tx) error {
		var version int64
		var verifier, data string
		var revoked int
		e := tx.QueryRowContext(ctx, s.Query("SELECT version,verifier,data,revoked FROM api_keys WHERE tenant_id=? AND id=?"), tenant, id).Scan(&version, &verifier, &data, &revoked)
		if e != nil {
			return storeError("unauthorized", 401)
		}
		if revoked != 0 || subtle.ConstantTimeCompare([]byte(verifier), []byte(present)) != 1 {
			return storeError("unauthorized", 401)
		}
		out, e = s.resolveActiveKeyTx(ctx, tx, tenant, id, version)
		return e
	})
	if err != nil {
		return core.Principal{}, storeError("unauthorized", 401)
	}
	return out, nil
}

func (s *Store) IssueKey(ctx context.Context, p core.Principal, id string, data json.RawMessage) (out core.Resource, token string, err error) {
	if id == "" || len(id) > 256 || strings.ContainsAny(id, "\x00\r\n") {
		return out, "", storeError("invalid_resource", 400)
	}
	kd, e := decodeKeyData(data)
	if e != nil {
		return out, "", e
	}
	token, verifier, e := newKeyToken(p.TenantID, id)
	if e != nil {
		return out, "", e
	}
	err = s.WithTx(ctx, func(tx *sql.Tx) error {
		current, e := s.resolvePrincipalTx(ctx, tx, p)
		if e != nil {
			return e
		}
		if e = authorize(current, "api_keys", true); e != nil {
			return e
		}
		if !issuerAllowsKey(current, kd) {
			return storeError("forbidden", 403)
		}
		var exists int
		if e := tx.QueryRowContext(ctx, s.Query("SELECT 1 FROM resources WHERE tenant_id=? AND kind='api_keys' AND id=?"), current.TenantID, id).Scan(&exists); e == nil {
			return storeError("already_exists", 409)
		} else if e != sql.ErrNoRows {
			return e
		}
		encoded, e := json.Marshal(kd)
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, s.Query("INSERT INTO resources(tenant_id,kind,id,version,data) VALUES(?,?,?,1,?)"), current.TenantID, "api_keys", id, string(encoded)); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, s.Query("INSERT INTO api_keys(tenant_id,id,version,verifier,data,revoked) VALUES(?,?,1,?,?,0)"), current.TenantID, id, verifier, string(encoded)); e != nil {
			return e
		}
		if e = s.validateTenantTx(ctx, tx, current.TenantID); e != nil {
			return e
		}
		if e = s.bumpRevisionTx(ctx, tx); e != nil {
			return e
		}
		if e = s.auditTx(ctx, tx, current, "api_keys", id, "issue", 1); e != nil {
			return e
		}
		out = core.Resource{ID: id, TenantID: current.TenantID, Kind: "api_keys", Version: 1, Data: encoded}
		return nil
	})
	if err != nil {
		return core.Resource{}, "", err
	}
	return out, token, nil
}

func (s *Store) RotateKey(ctx context.Context, p core.Principal, id string, expectedVersion int64) (out core.Resource, token string, err error) {
	if id == "" || expectedVersion <= 0 {
		return out, "", storeError("invalid_resource", 400)
	}
	token, verifier, e := newKeyToken(p.TenantID, id)
	if e != nil {
		return out, "", e
	}
	err = s.WithTx(ctx, func(tx *sql.Tx) error {
		current, e := s.resolvePrincipalTx(ctx, tx, p)
		if e != nil {
			return e
		}
		if e = authorize(current, "api_keys", true); e != nil {
			return e
		}
		var version int64
		var data string
		var revoked int
		if e := tx.QueryRowContext(ctx, s.Query("SELECT version,data,revoked FROM api_keys WHERE tenant_id=? AND id=?"), current.TenantID, id).Scan(&version, &data, &revoked); e != nil {
			return storeError("not_found", 404)
		}
		if revoked != 0 || version != expectedVersion {
			return storeError("version_conflict", 412)
		}
		var kd keyData
		if e := json.Unmarshal([]byte(data), &kd); e != nil {
			return storeError("invalid_key_metadata", 500)
		}
		next := version + 1
		res, e := tx.ExecContext(ctx, s.Query("UPDATE api_keys SET version=?,verifier=? WHERE tenant_id=? AND id=? AND version=?"), next, verifier, current.TenantID, id, version)
		if e != nil {
			return e
		}
		if n, e := res.RowsAffected(); e != nil || n != 1 {
			return storeError("version_conflict", 412)
		}
		res, e = tx.ExecContext(ctx, s.Query("UPDATE resources SET version=? WHERE tenant_id=? AND kind='api_keys' AND id=? AND version=?"), next, current.TenantID, id, version)
		if e != nil {
			return e
		}
		if n, e := res.RowsAffected(); e != nil || n != 1 {
			return storeError("version_conflict", 412)
		}
		if e := s.bumpRevisionTx(ctx, tx); e != nil {
			return e
		}
		if e := s.auditTx(ctx, tx, current, "api_keys", id, "rotate", next); e != nil {
			return e
		}
		out = core.Resource{ID: id, TenantID: current.TenantID, Kind: "api_keys", Version: next, Data: json.RawMessage(data)}
		_ = kd
		return nil
	})
	if err != nil {
		return core.Resource{}, "", err
	}
	return out, token, nil
}

func (s *Store) syncKeyMetadataTx(ctx context.Context, tx *sql.Tx, p core.Principal, m core.Mutation, data json.RawMessage, version int64) error {
	if m.ID == "" {
		return storeError("invalid_resource", 400)
	}
	if version <= 1 {
		return storeError("invalid_key_mutation", 400)
	}
	var oldVersion int64
	var oldData string
	var revoked int
	if err := tx.QueryRowContext(ctx, s.Query("SELECT version,data,revoked FROM api_keys WHERE tenant_id=? AND id=?"), p.TenantID, m.ID).Scan(&oldVersion, &oldData, &revoked); err != nil {
		return storeError("not_found", 404)
	}
	if oldVersion+1 != version {
		return storeError("version_conflict", 412)
	}
	nextData := oldData
	nextRevoked := revoked
	if m.Delete {
		nextRevoked = 1
	} else {
		kd, err := decodeKeyData(data)
		if err != nil || !issuerAllowsKey(p, kd) {
			if err != nil {
				return err
			}
			return storeError("forbidden", 403)
		}
		encoded, err := json.Marshal(kd)
		if err != nil {
			return err
		}
		nextData = string(encoded)
	}
	_, err := tx.ExecContext(ctx, s.Query("UPDATE api_keys SET version=?,data=?,revoked=? WHERE tenant_id=? AND id=? AND version=?"), version, nextData, nextRevoked, p.TenantID, m.ID, oldVersion)
	return err
}
