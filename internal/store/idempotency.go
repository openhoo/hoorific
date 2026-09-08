package store

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"hoorific/internal/core"
	"hoorific/internal/credential"
)

const (
	idempotencyMaxCoordinateBytes = 512
	idempotencyMaxKeyBytes        = 256
	idempotencyMaxFingerprint     = 4096
	idempotencyMaxOwnerBytes      = 512
	idempotencyMaxBodyBytes       = 32 << 20
	idempotencyMaxCiphertextBytes = 32 << 20
	idempotencyMaxHeaderCount     = 128
	idempotencyMaxHeaderValues    = 512
	idempotencyMaxHeaderBytes     = 64 << 10
	idempotencyMaxHeaderNameBytes = 256
	idempotencyMaxHeaderValue     = 16 << 10
	idempotencyMaxRetention       = 24 * time.Hour
	idempotencyCleanupBatch       = 256
	idempotencyKeyPurpose         = "idempotency-response/v1"
)

type idempotencyRow struct {
	FingerprintHash string
	OwnerID         string
	State           string
	ExpiresAt       int64
	KeyID           string
	Nonce           []byte
	Ciphertext      []byte
}

type idempotencyCleanupKey struct {
	TenantID  string
	SubjectID string
	KeyHash   string
}

type idempotencyHeader struct {
	Name   string
	Values []string
}

func validIdempotencyString(value string, limit int) bool {
	return value != "" && len(value) <= limit && utf8.ValidString(value) && !strings.ContainsRune(value, 0) && strings.TrimSpace(value) != ""
}
func validIdempotencyHeaderName(name string) bool {
	if name == "" || len(name) > idempotencyMaxHeaderNameBytes || !utf8.ValidString(name) || strings.ContainsAny(name, "\x00\r\n") {
		return false
	}
	for i := range len(name) {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			continue
		}
		switch c {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return true
}

func validateIdempotencyRequest(req core.IdempotencyRequest, allowExpired bool) error {
	if !validIdempotencyString(req.TenantID, idempotencyMaxCoordinateBytes) ||
		!validIdempotencyString(req.SubjectID, idempotencyMaxCoordinateBytes) ||
		!validIdempotencyString(req.Key, idempotencyMaxKeyBytes) ||
		!validIdempotencyString(req.Fingerprint, idempotencyMaxFingerprint) ||
		!validIdempotencyString(req.OwnerID, idempotencyMaxOwnerBytes) {
		return core.ErrIdempotencyInvalid
	}
	if req.ExpiresAt.IsZero() {
		return core.ErrIdempotencyInvalid
	}
	if !allowExpired && !req.ExpiresAt.After(time.Now()) {
		return core.ErrIdempotencyInvalid
	}
	return nil
}

func idempotencyHash(purpose, value string) string {
	h := sha256.New()
	_, _ = io.WriteString(h, "hoorific/idempotency/")
	_, _ = io.WriteString(h, purpose)
	_, _ = io.WriteString(h, "/v1\x00")
	_, _ = io.WriteString(h, value)
	return hex.EncodeToString(h.Sum(nil))
}

func idempotencyAAD(tenantID, subjectID, keyHash, fingerprintHash string) []byte {
	out := make([]byte, 0, 32+len(tenantID)+len(subjectID)+len(keyHash)+len(fingerprintHash))
	out = append(out, "hoorific/idempotency-response/v1\x00"...)
	for _, value := range []string{tenantID, subjectID, keyHash, fingerprintHash} {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(value)))
		out = append(out, n[:]...)
		out = append(out, value...)
	}
	return out
}

type purposeKeyring struct {
	base    credential.Keyring
	purpose string
}

func derivePurposeKey(master []byte, purpose string) []byte {
	mac := hmac.New(sha256.New, master)
	_, _ = io.WriteString(mac, "hoorific/store-key/v1\x00")
	_, _ = io.WriteString(mac, purpose)
	return mac.Sum(nil)
}

func (k purposeKeyring) Current() (string, []byte, error) {
	if k.base == nil {
		return "", nil, credential.ErrInvalidCredential
	}
	id, key, err := k.base.Current()
	if err != nil {
		return "", nil, err
	}
	return id, derivePurposeKey(key, k.purpose), nil
}

func (k purposeKeyring) ByID(id string) ([]byte, error) {
	if k.base == nil {
		return nil, credential.ErrInvalidCredential
	}
	key, err := k.base.ByID(id)
	if err != nil {
		return nil, err
	}
	return derivePurposeKey(key, k.purpose), nil
}

func (s *Store) idempotencyKeyring() credential.Keyring {
	return purposeKeyring{base: s.masterKeys, purpose: idempotencyKeyPurpose}
}

func (s *Store) sealIdempotency(tenantID, subjectID, keyHash, fingerprintHash string, plain []byte) (credential.Envelope, error) {
	keyID, key, err := s.idempotencyKeyring().Current()
	if err != nil {
		return credential.Envelope{}, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return credential.Envelope{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return credential.Envelope{}, err
	}
	if len(plain) > idempotencyMaxCiphertextBytes-aead.Overhead() {
		return credential.Envelope{}, core.ErrIdempotencyInvalid
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return credential.Envelope{}, err
	}
	return credential.Envelope{
		KeyID:      keyID,
		Nonce:      nonce,
		Ciphertext: aead.Seal(nil, nonce, plain, idempotencyAAD(tenantID, subjectID, keyHash, fingerprintHash)),
	}, nil
}

func (s *Store) openIdempotency(tenantID, subjectID, keyHash, fingerprintHash string, envelope credential.Envelope) ([]byte, error) {
	if len(envelope.Ciphertext) > idempotencyMaxCiphertextBytes {
		return nil, core.ErrIdempotencyUnreplayable
	}
	key, err := s.idempotencyKeyring().ByID(envelope.KeyID)
	if err != nil {
		return nil, core.ErrIdempotencyUnreplayable
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, core.ErrIdempotencyUnreplayable
	}
	aead, err := cipher.NewGCM(block)
	if err != nil || len(envelope.Nonce) != aead.NonceSize() || len(envelope.Ciphertext) < aead.Overhead() {
		return nil, core.ErrIdempotencyUnreplayable
	}
	plain, err := aead.Open(nil, envelope.Nonce, envelope.Ciphertext, idempotencyAAD(tenantID, subjectID, keyHash, fingerprintHash))
	if err != nil {
		return nil, core.ErrIdempotencyUnreplayable
	}
	return plain, nil
}

func normalizeIdempotencyHeaders(headers http.Header) ([]idempotencyHeader, int, error) {
	if len(headers) > idempotencyMaxHeaderCount {
		return nil, 0, core.ErrIdempotencyInvalid
	}
	byName := make(map[string][]string, len(headers))
	totalValues := 0
	totalBytes := 0
	for rawName, values := range headers {
		name := http.CanonicalHeaderKey(rawName)
		if name == "" || name != strings.TrimSpace(rawName) || !utf8.ValidString(rawName) || strings.ContainsAny(rawName, "\x00\r\n") || len(name) > idempotencyMaxHeaderNameBytes {
			return nil, 0, core.ErrIdempotencyInvalid
		}
		if len(values) > idempotencyMaxHeaderValues || totalValues > idempotencyMaxHeaderValues-len(values) {
			return nil, 0, core.ErrIdempotencyInvalid
		}
		if totalBytes > idempotencyMaxHeaderBytes-len(name) {
			return nil, 0, core.ErrIdempotencyInvalid
		}
		totalBytes += len(name)
		for _, value := range values {
			if len(value) > idempotencyMaxHeaderValue || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n") || totalBytes > idempotencyMaxHeaderBytes-len(value) {
				return nil, 0, core.ErrIdempotencyInvalid
			}
			totalBytes += len(value)
		}
		byName[name] = append(byName[name], values...)
		totalValues += len(values)
	}
	if len(byName) > idempotencyMaxHeaderCount || totalValues > idempotencyMaxHeaderValues {
		return nil, 0, core.ErrIdempotencyInvalid
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]idempotencyHeader, 0, len(names))
	for _, name := range names {
		out = append(out, idempotencyHeader{Name: name, Values: append([]string(nil), byName[name]...)})
	}
	return out, totalBytes, nil
}

func appendUint16(dst []byte, value uint16) []byte {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], value)
	return append(dst, b[:]...)
}

func appendUint32(dst []byte, value uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], value)
	return append(dst, b[:]...)
}

func encodeIdempotencyResponse(response *core.IdempotencyResponse) ([]byte, error) {
	if response == nil || response.Status < 100 || response.Status > 599 || len(response.Body) > idempotencyMaxBodyBytes {
		return nil, core.ErrIdempotencyInvalid
	}
	headers, headerBytes, err := normalizeIdempotencyHeaders(response.Header)
	if err != nil {
		return nil, err
	}
	if len(headers) > int(^uint16(0)) {
		return nil, core.ErrIdempotencyInvalid
	}
	size := 4 + 2 + 2 + 4
	for _, header := range headers {
		size += 2 + 2 + len(header.Name)
		for _, value := range header.Values {
			size += 4 + len(value)
		}
	}
	if size < 0 || size > idempotencyMaxBodyBytes+idempotencyMaxHeaderBytes+12 || headerBytes > idempotencyMaxHeaderBytes {
		return nil, core.ErrIdempotencyInvalid
	}
	out := make([]byte, 0, size+len(response.Body))
	out = append(out, 'H', 'I', 'D', '1')
	out = appendUint16(out, uint16(response.Status))
	out = appendUint16(out, uint16(len(headers)))
	out = appendUint32(out, uint32(len(response.Body)))
	for _, header := range headers {
		out = appendUint16(out, uint16(len(header.Name)))
		out = appendUint16(out, uint16(len(header.Values)))
		out = append(out, header.Name...)
		for _, value := range header.Values {
			out = appendUint32(out, uint32(len(value)))
			out = append(out, value...)
		}
	}
	out = append(out, response.Body...)
	return out, nil
}

func decodeIdempotencyResponse(plain []byte) (core.IdempotencyResponse, error) {
	var out core.IdempotencyResponse
	if len(plain) < 12 || !bytes.Equal(plain[:4], []byte("HID1")) {
		return out, core.ErrIdempotencyUnreplayable
	}
	plain = plain[4:]
	status := binary.BigEndian.Uint16(plain[:2])
	headerCount := binary.BigEndian.Uint16(plain[2:4])
	bodyLength := binary.BigEndian.Uint32(plain[4:8])
	plain = plain[8:]
	if status < 100 || status > 599 || headerCount > idempotencyMaxHeaderCount || bodyLength > idempotencyMaxBodyBytes {
		return out, core.ErrIdempotencyUnreplayable
	}
	header := make(http.Header, int(headerCount))
	totalValues := 0
	totalHeaderBytes := 0
	take := func(size uint64) ([]byte, bool) {
		if size > uint64(len(plain)) {
			return nil, false
		}
		out := plain[:int(size)]
		plain = plain[int(size):]
		return out, true
	}
	takeU16 := func() (uint16, bool) {
		b, ok := take(2)
		if !ok {
			return 0, false
		}
		return binary.BigEndian.Uint16(b), true
	}
	takeU32 := func() (uint32, bool) {
		b, ok := take(4)
		if !ok {
			return 0, false
		}
		return binary.BigEndian.Uint32(b), true
	}
	for range int(headerCount) {
		nameLength, ok := takeU16()
		if !ok || nameLength == 0 || nameLength > idempotencyMaxHeaderNameBytes {
			return out, core.ErrIdempotencyUnreplayable
		}
		valueCount, ok := takeU16()
		if !ok || valueCount > idempotencyMaxHeaderValues || totalValues > idempotencyMaxHeaderValues-int(valueCount) {
			return out, core.ErrIdempotencyUnreplayable
		}
		nameBytes, ok := take(uint64(nameLength))
		if !ok {
			return out, core.ErrIdempotencyUnreplayable
		}
		name := string(nameBytes)
		if http.CanonicalHeaderKey(name) != name || strings.ContainsAny(name, "\x00\r\n") || !utf8.ValidString(name) {
			return out, core.ErrIdempotencyUnreplayable
		}
		if _, exists := header[name]; exists {
			return out, core.ErrIdempotencyUnreplayable
		}
		if totalHeaderBytes > idempotencyMaxHeaderBytes-len(name) {
			return out, core.ErrIdempotencyUnreplayable
		}
		totalHeaderBytes += len(name)
		values := make([]string, 0, int(valueCount))
		for range int(valueCount) {
			valueLength, ok := takeU32()
			if !ok || valueLength > idempotencyMaxHeaderValue || uint64(valueLength) > uint64(len(plain)) || totalHeaderBytes > idempotencyMaxHeaderBytes-int(valueLength) {
				return out, core.ErrIdempotencyUnreplayable
			}
			valueBytes, ok := take(uint64(valueLength))
			if !ok {
				return out, core.ErrIdempotencyUnreplayable
			}
			value := string(valueBytes)
			if !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n") {
				return out, core.ErrIdempotencyUnreplayable
			}
			totalHeaderBytes += len(value)
			values = append(values, value)
		}
		header[name] = values
		totalValues += int(valueCount)
	}
	if uint64(bodyLength) != uint64(len(plain)) || len(plain) > idempotencyMaxBodyBytes {
		return out, core.ErrIdempotencyUnreplayable
	}
	out.Status = int(status)
	out.Header = header
	out.Body = append([]byte(nil), plain...)
	return out, nil
}

func (s *Store) loadIdempotencyRow(ctx context.Context, tx *sql.Tx, tenantID, subjectID, keyHash string) (idempotencyRow, error) {
	q := "SELECT fingerprint_hash,owner_id,state,expires_at,key_id,nonce,ciphertext FROM idempotency_records WHERE tenant_id=? AND subject_id=? AND key_hash=?"
	if s.Dialect == "postgres" {
		q += " FOR UPDATE"
	}
	var row idempotencyRow
	err := tx.QueryRowContext(ctx, s.Query(q), tenantID, subjectID, keyHash).Scan(&row.FingerprintHash, &row.OwnerID, &row.State, &row.ExpiresAt, &row.KeyID, &row.Nonce, &row.Ciphertext)
	return row, err
}

func (s *Store) insertIdempotencyPending(ctx context.Context, tx *sql.Tx, req core.IdempotencyRequest, keyHash, fingerprintHash string, expiresAt, now int64) error {
	result, err := tx.ExecContext(ctx, s.Query(`INSERT INTO idempotency_records(tenant_id,subject_id,key_hash,fingerprint_hash,owner_id,state,expires_at,key_id,nonce,ciphertext,created_at,updated_at) VALUES (?,?,?,?,?,'pending',?,?,?,?,?,?)`), req.TenantID, req.SubjectID, keyHash, fingerprintHash, req.OwnerID, expiresAt, "", []byte{}, []byte{}, now, now)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("idempotency claim was not inserted")
	}
	return nil
}

func (s *Store) markIdempotencyUnreplayable(ctx context.Context, tx *sql.Tx, tenantID, subjectID, keyHash string, now int64) error {
	result, err := tx.ExecContext(ctx, s.Query("UPDATE idempotency_records SET state='unreplayable',key_id='',nonce=?,ciphertext=?,updated_at=? WHERE tenant_id=? AND subject_id=? AND key_hash=?"), []byte{}, []byte{}, now, tenantID, subjectID, keyHash)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return core.ErrIdempotencyNotFound
	}
	return nil
}

// BeginIdempotency atomically claims a key for one owner, or returns the
// durable terminal state. Expired complete responses may be removed and
// claimed again; pending and unreplayable rows are never reclaimed.
func (s *Store) BeginIdempotency(ctx context.Context, req core.IdempotencyRequest) (out core.IdempotencyRecord, err error) {
	if err = validateIdempotencyRequest(req, true); err != nil {
		return out, err
	}
	keyHash := idempotencyHash("key", req.Key)
	fingerprintHash := idempotencyHash("fingerprint", req.Fingerprint)
	var terminalErr error
	err = s.WithTx(ctx, func(tx *sql.Tx) error {
		now, e := sqlNow(ctx, tx, s.Dialect)
		if e != nil {
			return e
		}
		requestedExpiry := req.ExpiresAt.Unix()
		row, e := s.loadIdempotencyRow(ctx, tx, req.TenantID, req.SubjectID, keyHash)
		if errors.Is(e, sql.ErrNoRows) {
			if requestedExpiry <= now {
				return core.ErrIdempotencyInvalid
			}
			expiresAt := requestedExpiry
			maxExpiry := now + int64(idempotencyMaxRetention/time.Second)
			if expiresAt > maxExpiry {
				expiresAt = maxExpiry
			}
			if e = s.insertIdempotencyPending(ctx, tx, req, keyHash, fingerprintHash, expiresAt, now); e != nil {
				return e
			}
			out = core.IdempotencyRecord{State: "new"}
			return nil
		}
		if e != nil {
			return e
		}
		if row.State == "complete" && row.ExpiresAt <= now {
			if requestedExpiry <= now {
				return core.ErrIdempotencyInvalid
			}
			if _, e = tx.ExecContext(ctx, s.Query("DELETE FROM idempotency_records WHERE tenant_id=? AND subject_id=? AND key_hash=? AND state='complete' AND expires_at<=?"), req.TenantID, req.SubjectID, keyHash, now); e != nil {
				return e
			}
			expiresAt := requestedExpiry
			maxExpiry := now + int64(idempotencyMaxRetention/time.Second)
			if expiresAt > maxExpiry {
				expiresAt = maxExpiry
			}
			if e = s.insertIdempotencyPending(ctx, tx, req, keyHash, fingerprintHash, expiresAt, now); e != nil {
				return e
			}
			out = core.IdempotencyRecord{State: "new"}
			return nil
		}
		if row.FingerprintHash != fingerprintHash {
			return core.ErrIdempotencyFingerprintConflict
		}
		switch row.State {
		case "pending":
			out = core.IdempotencyRecord{State: "pending"}
			return nil
		case "unreplayable":
			out = core.IdempotencyRecord{State: "unreplayable"}
			return nil
		case "complete":
			plain, openErr := s.openIdempotency(req.TenantID, req.SubjectID, keyHash, fingerprintHash, credential.Envelope{KeyID: row.KeyID, Nonce: row.Nonce, Ciphertext: row.Ciphertext})
			if openErr == nil {
				response, decodeErr := decodeIdempotencyResponse(plain)
				if decodeErr == nil {
					out = core.IdempotencyRecord{State: "complete", Response: &response}
					return nil
				}
			}
			if e = s.markIdempotencyUnreplayable(ctx, tx, req.TenantID, req.SubjectID, keyHash, now); e != nil {
				return e
			}
			out = core.IdempotencyRecord{State: "unreplayable"}
			terminalErr = core.ErrIdempotencyUnreplayable
			return nil
		default:
			return core.ErrIdempotencyUnreplayable
		}
	})
	if err == nil && terminalErr != nil {
		err = terminalErr
	}
	return out, err
}

// FinishIdempotency durably records a terminal response or an unreplayable
// tombstone. Only the owner that won BeginIdempotency may finish a pending row.
func (s *Store) FinishIdempotency(ctx context.Context, req core.IdempotencyRequest, response *core.IdempotencyResponse) error {
	if err := validateIdempotencyRequest(req, true); err != nil {
		return err
	}
	keyHash := idempotencyHash("key", req.Key)
	fingerprintHash := idempotencyHash("fingerprint", req.Fingerprint)
	var envelope credential.Envelope
	if response != nil {
		plain, err := encodeIdempotencyResponse(response)
		if err != nil {
			return err
		}
		envelope, err = s.sealIdempotency(req.TenantID, req.SubjectID, keyHash, fingerprintHash, plain)
		if err != nil {
			return err
		}
	}
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		row, err := s.loadIdempotencyRow(ctx, tx, req.TenantID, req.SubjectID, keyHash)
		if errors.Is(err, sql.ErrNoRows) {
			return core.ErrIdempotencyNotFound
		}
		if err != nil {
			return err
		}
		if row.FingerprintHash != fingerprintHash {
			return core.ErrIdempotencyFingerprintConflict
		}
		if row.OwnerID != req.OwnerID {
			return core.ErrIdempotencyOwnerFenced
		}
		switch row.State {
		case "complete":
			return nil
		case "unreplayable":
			return core.ErrIdempotencyUnreplayable
		case "pending":
			now, err := sqlNow(ctx, tx, s.Dialect)
			if err != nil {
				return err
			}
			var result sql.Result
			if response == nil {
				result, err = tx.ExecContext(ctx, s.Query("UPDATE idempotency_records SET state='unreplayable',key_id='',nonce=?,ciphertext=?,updated_at=? WHERE tenant_id=? AND subject_id=? AND key_hash=? AND owner_id=? AND state='pending'"), []byte{}, []byte{}, now, req.TenantID, req.SubjectID, keyHash, req.OwnerID)
			} else {
				expiresAt := now + int64(idempotencyMaxRetention/time.Second)
				result, err = tx.ExecContext(ctx, s.Query("UPDATE idempotency_records SET state='complete',key_id=?,nonce=?,ciphertext=?,expires_at=?,updated_at=? WHERE tenant_id=? AND subject_id=? AND key_hash=? AND owner_id=? AND state='pending'"), envelope.KeyID, envelope.Nonce, envelope.Ciphertext, expiresAt, now, req.TenantID, req.SubjectID, keyHash, req.OwnerID)
			}
			if err != nil {
				return err
			}
			changed, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if changed != 1 {
				return core.ErrIdempotencyOwnerFenced
			}
			return nil
		default:
			return core.ErrIdempotencyUnreplayable
		}
	})
}

// cleanupIdempotencyTx is called by RunMaintenance while it already holds the
// store transaction/advisory lock. It only removes expired complete rows and
// is deliberately bounded so a large tenant cannot monopolize maintenance.
func (s *Store) cleanupIdempotencyTx(ctx context.Context, tx *sql.Tx, now int64) error {
	rows, err := tx.QueryContext(ctx, s.Query("SELECT tenant_id,subject_id,key_hash FROM idempotency_records WHERE state='complete' AND expires_at<=? ORDER BY expires_at,tenant_id,subject_id,key_hash LIMIT ?"), now, idempotencyCleanupBatch)
	if err != nil {
		return err
	}
	keys := make([]idempotencyCleanupKey, 0, idempotencyCleanupBatch)
	for rows.Next() {
		var key idempotencyCleanupKey
		if err = rows.Scan(&key.TenantID, &key.SubjectID, &key.KeyHash); err != nil {
			rows.Close()
			return err
		}
		keys = append(keys, key)
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, key := range keys {
		if _, err = tx.ExecContext(ctx, s.Query("DELETE FROM idempotency_records WHERE tenant_id=? AND subject_id=? AND key_hash=? AND state='complete' AND expires_at<=?"), key.TenantID, key.SubjectID, key.KeyHash, now); err != nil {
			return err
		}
	}
	return nil
}

var _ core.IdempotencyStore = (*Store)(nil)
