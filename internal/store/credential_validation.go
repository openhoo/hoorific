package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"hoorific/internal/credential"
	"hoorific/internal/resource"
)

const credentialValidationLimit = 10000

// ValidateCredentials verifies every retained encrypted credential envelope and
// continuation key reference before the process starts serving. It never
// returns or logs decrypted values; an unavailable historical key fails closed.
func (s *Store) ValidateCredentials(ctx context.Context, keys credential.Keyring) error {
	if keys == nil {
		return errors.New("credential keyring is required")
	}
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		q := s.Query("SELECT tenant_id,kind,id,data FROM durable_records WHERE kind IN ('credential','credential_login','device_authorization','resource_continuation') ORDER BY kind,id LIMIT ?")
		rows, err := tx.QueryContext(ctx, q, credentialValidationLimit+1)
		if err != nil {
			return fmt.Errorf("read encrypted records: %w", err)
		}
		defer rows.Close()
		count := 0
		for rows.Next() {
			count++
			if count > credentialValidationLimit {
				return errors.New("too many retained encrypted records")
			}
			var tenant, kind, id, data string
			if err := rows.Scan(&tenant, &kind, &id, &data); err != nil {
				return err
			}
			switch kind {
			case "credential":
				var row storedCredential
				if err := decodeStoredRecord(data, &row); err != nil {
					return fmt.Errorf("credential %s/%s metadata: %w", tenant, id, err)
				}
				identity := row.Record.Identity
				if identity.TenantID != tenant || identity.ConnectionID != id || identity.CredentialID == "" || identity.AccountID == "" || identity.Provider == "" || identity.Version < 1 {
					return fmt.Errorf("credential %s/%s identity is invalid", tenant, id)
				}
				if _, err := credential.Open(keys, identity, row.Record.Envelope); err != nil {
					return fmt.Errorf("credential %s/%s envelope: %w", tenant, id, err)
				}
			case "credential_login":
				var session credential.LoginSession
				if err := decodeStoredRecord(data, &session); err != nil {
					return fmt.Errorf("OAuth login %s/%s metadata: %w", tenant, id, err)
				}
				if session.TenantID != tenant || session.StateHash != id || session.AdminSessionID == "" || session.Connector == "" || session.Nonce == "" || session.RedirectURI == "" {
					return fmt.Errorf("OAuth login %s/%s identity is invalid", tenant, id)
				}
				if (session.ConnectionID == "") != (session.AccountID == "") || (session.ConnectionID == "" && session.ConnectionVersion != 0) || (session.ConnectionID != "" && session.ConnectionVersion < 1) {
					return fmt.Errorf("OAuth login %s/%s connection binding is invalid", tenant, id)
				}
				identity := credential.Identity{TenantID: session.TenantID, ConnectionID: session.ConnectionID, CredentialID: "oauth-session:" + session.StateHash, AccountID: session.AccountID, Provider: session.Connector, Version: session.ConnectionVersion}
				secret, err := credential.Open(keys, identity, session.VerifierEnvelope)
				if err != nil || secret.APIKey == nil {
					return fmt.Errorf("OAuth login %s/%s verifier envelope: %w", tenant, id, credential.ErrInvalidEnvelope)
				}
			case "device_authorization":
				var row storedDevice
				if err := decodeStoredRecord(data, &row); err != nil {
					return fmt.Errorf("device %s/%s metadata: %w", tenant, id, err)
				}
				state := row.State
				if state.TenantID != tenant || state.ID != id || state.ConnectionID == "" || state.AccountID == "" || state.Connector == "" || state.ConnectionVersion < 1 {
					return fmt.Errorf("device %s/%s identity is invalid", tenant, id)
				}
				identity := credential.Identity{TenantID: state.TenantID, ConnectionID: state.ConnectionID, CredentialID: "device:" + state.ID, AccountID: state.AccountID, Provider: state.Connector, Version: state.ConnectionVersion}
				code, err := credential.Open(keys, identity, state.DeviceCodeEnvelope)
				if err != nil || code.APIKey == nil {
					return fmt.Errorf("device %s/%s code envelope: %w", tenant, id, credential.ErrInvalidEnvelope)
				}
				if state.Status == "authorized" {
					completion, completionErr := credential.Open(keys, identity, state.CompletionEnvelope)
					if completionErr != nil || completion.OAuth == nil {
						return fmt.Errorf("device %s/%s completion envelope: %w", tenant, id, credential.ErrInvalidEnvelope)
					}
					proof, proofErr := credential.Open(keys, identity, state.IDTokenEnvelope)
					if proofErr != nil || proof.APIKey == nil {
						return fmt.Errorf("device %s/%s ID token envelope: %w", tenant, id, credential.ErrInvalidEnvelope)
					}
				} else if state.CompletionEnvelope.KeyID != "" || len(state.CompletionEnvelope.Nonce) != 0 || len(state.CompletionEnvelope.Ciphertext) != 0 || state.IDTokenEnvelope.KeyID != "" || len(state.IDTokenEnvelope.Nonce) != 0 || len(state.IDTokenEnvelope.Ciphertext) != 0 {
					return fmt.Errorf("device %s/%s has unexpected completion proof", tenant, id)
				}
			case "resource_continuation":
				var c resource.Continuation
				if err := decodeStoredRecord(data, &c); err != nil {
					return fmt.Errorf("continuation %s/%s metadata: %w", tenant, id, err)
				}
				if c.TenantID != tenant {
					return fmt.Errorf("continuation %s/%s tenant binding is invalid", tenant, id)
				}
				if c.KeyID == "" || len(c.Ciphertext) == 0 {
					return fmt.Errorf("continuation %s/%s envelope is incomplete", tenant, id)
				}
				key, err := keys.ByID(c.KeyID)
				if err != nil {
					return fmt.Errorf("continuation %s/%s key: %w", tenant, id, err)
				}
				if _, _, err := resource.OpenContinuation(key, c, time.Time{}); err != nil {
					return fmt.Errorf("continuation %s/%s envelope: %w", tenant, id, err)
				}
			}
		}
		return rows.Err()
	})
}

func decodeStoredRecord(data string, out any) error {
	if data == "" {
		return errors.New("empty durable record")
	}
	if err := json.Unmarshal([]byte(data), out); err != nil {
		return err
	}
	return nil
}
