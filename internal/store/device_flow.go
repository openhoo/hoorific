package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hoorific/internal/core"
	"hoorific/internal/credential"
	"time"
)

type deviceFlowRecord struct {
	TenantID, BindingHash string
	Version               int64
	ExpiresAt             time.Time
	Ciphertext            []byte
	Consumed              bool
}

func (s *Store) PutDeviceFlow(ctx context.Context, p core.Principal, id, binding string, version int64, expires time.Time, ciphertext []byte) error {
	if p.TenantID == "" || id == "" || binding == "" || version <= 0 || !expires.After(time.Now()) || len(ciphertext) == 0 {
		return errors.New("invalid device flow")
	}
	h := sha256.Sum256([]byte(binding))
	r := deviceFlowRecord{TenantID: p.TenantID, BindingHash: hex.EncodeToString(h[:]), Version: version, ExpiresAt: expires, Ciphertext: append([]byte(nil), ciphertext...)}
	return s.WithTx(ctx, func(tx *sql.Tx) error { return s.durablePut(ctx, tx, p.TenantID, "device_flow", id, 0, r) })
}
func (s *Store) TakeDeviceFlow(ctx context.Context, p core.Principal, id, binding string, version int64, now time.Time) ([]byte, error) {
	if p.TenantID == "" || id == "" || binding == "" || version <= 0 {
		return nil, errors.New("invalid device flow")
	}
	h := sha256.Sum256([]byte(binding))
	want := hex.EncodeToString(h[:])
	var out []byte
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		var r deviceFlowRecord
		_, e := s.durableGet(ctx, tx, p.TenantID, "device_flow", id, &r)
		if e != nil {
			return e
		}
		if r.Consumed || r.BindingHash != want || r.Version != version || !r.ExpiresAt.After(now) {
			return errors.New("device flow expired or binding mismatch")
		}
		out = append([]byte(nil), r.Ciphertext...)
		return s.durableDelete(ctx, tx, p.TenantID, "device_flow", id)
	})
	return out, err
}

type storedDevice struct {
	State credential.DeviceState `json:"state"`
}

func deviceRecord(s credential.DeviceState) storedDevice { return storedDevice{State: s} }
func (s *Store) CreateDevice(ctx context.Context, d credential.DeviceState) error {
	if d.TenantID == "" || d.ID == "" || d.ConnectionID == "" || d.AccountID == "" || d.Version <= 0 || len(d.DeviceCodeEnvelope.Ciphertext) == 0 {
		return errors.New("invalid device state")
	}
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		return s.durablePut(ctx, tx, d.TenantID, "device_authorization", d.ID, 0, deviceRecord(d))
	})
}
func (s *Store) LoadDevice(ctx context.Context, tenant, id string) (credential.DeviceState, error) {
	if tenant == "" || id == "" {
		return credential.DeviceState{}, errors.New("invalid device identity")
	}
	var r storedDevice
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		_, e := s.durableGet(ctx, tx, tenant, "device_authorization", id, &r)
		return e
	})
	return r.State, err
}
func (s *Store) AcquireDeviceLease(ctx context.Context, tenant, id, owner string, now time.Time, d time.Duration) (credential.DeviceState, error) {
	if tenant == "" || id == "" || owner == "" || d <= 0 {
		return credential.DeviceState{}, errors.New("invalid device lease")
	}
	var out credential.DeviceState
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		var r storedDevice
		_, e := s.durableGet(ctx, tx, tenant, "device_authorization", id, &r)
		if e != nil {
			return e
		}
		st := r.State
		if st.Status != "pending" || !st.ExpiresAt.After(now) {
			return errors.New("device authorization is expired or no longer pending")
		}
		if st.PollAfter.After(now) {
			return errors.New("device authorization polling is not due")
		}
		if st.LeaseOwner != "" {
			return errors.New("device authorization has an unresolved poll lease")
		}
		old := st.Version
		st.LeaseOwner = owner
		st.LeaseUntil = now.Add(d)
		st.Version++
		b := deviceRecord(st)
		q := s.Query("UPDATE durable_records SET version=?,data=? WHERE tenant_id=? AND kind=? AND id=? AND version=?")
		raw, _ := json.Marshal(b)
		res, e := tx.ExecContext(ctx, q, st.Version, string(raw), tenant, "device_authorization", id, old)
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return errors.New("device lease changed")
		}
		out = st
		return nil
	})
	return out, err
}
func (s *Store) UpdateDevice(ctx context.Context, d credential.DeviceState, expected int64) error {
	if d.TenantID == "" || d.ID == "" || expected <= 0 {
		return errors.New("invalid device update")
	}
	d.Version = expected + 1
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		b := deviceRecord(d)
		raw, _ := json.Marshal(b)
		q := s.Query("UPDATE durable_records SET version=?,data=? WHERE tenant_id=? AND kind=? AND id=? AND version=?")
		res, e := tx.ExecContext(ctx, q, d.Version, string(raw), d.TenantID, "device_authorization", d.ID, expected)
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return errors.New("device version conflict")
		}
		return nil
	})
}
func (s *Store) CompleteDevice(ctx context.Context, tenant, id, owner string, now time.Time) error {
	return s.deviceStatus(ctx, tenant, id, owner, now, "authorized", "")
}
func (s *Store) MarkDeviceAmbiguous(ctx context.Context, tenant, id, owner, reason string) error {
	return s.deviceStatus(ctx, tenant, id, owner, time.Now(), "ambiguous", reason)
}
func (s *Store) deviceStatus(ctx context.Context, tenant, id, owner string, now time.Time, status, reason string) error {
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		var r storedDevice
		_, e := s.durableGet(ctx, tx, tenant, "device_authorization", id, &r)
		if e != nil {
			return e
		}
		if r.State.LeaseOwner != owner {
			return errors.New("device lease owner changed")
		}
		if status == "authorized" && !r.State.ExpiresAt.After(now) {
			return errors.New("device authorization expired")
		}
		r.State.Status = status
		r.State.LastError = reason
		r.State.LeaseOwner = ""
		r.State.LeaseUntil = time.Time{}
		r.State.Version++
		raw, _ := json.Marshal(r)
		q := s.Query("UPDATE durable_records SET version=?,data=? WHERE tenant_id=? AND kind=? AND id=? AND version=?")
		res, e := tx.ExecContext(ctx, q, r.State.Version, string(raw), tenant, "device_authorization", id, r.State.Version-1)
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return errors.New("device state changed")
		}
		return nil
	})
}

var _ credential.DeviceRepository = (*Store)(nil)
