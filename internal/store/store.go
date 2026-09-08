// Package store owns authoritative SQL configuration and accounting.
package store

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "github.com/jackc/pgx/v5/stdlib"
	"hoorific/internal/core"
	"hoorific/internal/credential"
	_ "modernc.org/sqlite"
)

const SchemaVersion = 3
const transactionLock int64 = 719240113

type Store struct {
	DB             *sql.DB
	Dialect        string
	CursorKey      []byte
	Config         core.BootstrapConfig
	masterKeys     credential.Keyring
	writer         sync.Mutex
	listenerMu     sync.RWMutex
	configListener func(string, int64)
}

func (s *Store) SetConfigListener(fn func(tenant string, revision int64)) {
	s.listenerMu.Lock()
	s.configListener = fn
	s.listenerMu.Unlock()
}

func (s *Store) notifyConfig(tenant string, revision int64) {
	s.listenerMu.RLock()
	fn := s.configListener
	s.listenerMu.RUnlock()
	if fn != nil {
		fn(tenant, revision)
	}
}

func Open(ctx context.Context, c core.BootstrapConfig) (*Store, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	rawKey, err := os.ReadFile(c.Encryption.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("read master key: %w", err)
	}
	keys, err := credential.LoadKeyring(c.Encryption.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load master keyring: %w", err)
	}
	if _, _, err = keys.Current(); err != nil {
		return nil, fmt.Errorf("load current master key: %w", err)
	}
	mac := hmac.New(sha256.New, rawKey)
	mac.Write([]byte("hoorific/admin-cursor/v1"))
	s := &Store{Config: c, CursorKey: mac.Sum(nil), masterKeys: keys}
	var driver, dsn string
	if c.Mode == "standalone" {
		s.Dialect = "sqlite"
		driver = "sqlite"
		if err = os.MkdirAll(filepath.Dir(c.Storage.SQLite.Path), 0700); err != nil {
			return nil, err
		}
		absolute, e := filepath.Abs(c.Storage.SQLite.Path)
		if e != nil {
			return nil, e
		}
		u := url.URL{Scheme: "file", Path: absolute}
		q := u.Query()
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "synchronous(FULL)")
		q.Add("_pragma", "foreign_keys(1)")
		q.Add("_pragma", "busy_timeout(10000)")
		q.Set("_txlock", "immediate")
		u.RawQuery = q.Encode()
		dsn = u.String()
	} else {
		s.Dialect = "postgres"
		driver = "pgx"
		b, e := os.ReadFile(c.Storage.Postgres.DSNFile)
		if e != nil {
			return nil, e
		}
		dsn = strings.TrimSpace(string(b))
		if dsn == "" {
			return nil, errors.New("empty PostgreSQL DSN")
		}
	}
	s.DB, err = sql.Open(driver, dsn)
	if err != nil {
		return nil, err
	}
	if s.Dialect == "sqlite" {
		s.DB.SetMaxOpenConns(1)
		s.DB.SetMaxIdleConns(1)
	} else {
		s.DB.SetMaxOpenConns(16)
		s.DB.SetMaxIdleConns(8)
	}
	if err = s.DB.PingContext(ctx); err != nil {
		s.DB.Close()
		return nil, err
	}
	if s.Dialect == "sqlite" {
		if err = os.Chmod(c.Storage.SQLite.Path, 0600); err != nil {
			s.DB.Close()
			return nil, err
		}
	}
	return s, nil
}
func (s *Store) Close() error { return s.DB.Close() }
func (s *Store) Query(q string) string {
	if s.Dialect != "postgres" {
		return q
	}
	var b strings.Builder
	n := 1
	quoted := false
	for _, r := range q {
		if r == '\'' {
			quoted = !quoted
		}
		if r == '?' && !quoted {
			fmt.Fprintf(&b, "$%d", n)
			n++
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// WithTx is the linearization boundary shared by authorization and mutation.
// SQLite's driver opens BEGIN IMMEDIATE; PostgreSQL takes a transaction-scoped
// advisory lock before reading any authorization or accounting row.
func (s *Store) WithTx(ctx context.Context, fn func(*sql.Tx) error) error {
	if s.Dialect == "sqlite" {
		s.writer.Lock()
		defer s.writer.Unlock()
	}
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if s.Dialect == "postgres" {
		if _, err = tx.ExecContext(ctx, "SET LOCAL synchronous_commit = on"); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", transactionLock); err != nil {
			return err
		}
	}
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) ConfigRevision(ctx context.Context, tenant string) (int64, error) {
	if tenant == "" {
		return 0, storeError("invalid_tenant", 400)
	}
	var revision int64
	err := s.DB.QueryRowContext(ctx, s.Query("SELECT revision FROM config_state WHERE id=1")).Scan(&revision)
	return revision, err
}
func (s *Store) CheckSchema(ctx context.Context) error {
	var v int
	if err := s.DB.QueryRowContext(ctx, "SELECT version FROM schema_version WHERE id=1").Scan(&v); err != nil {
		return fmt.Errorf("schema unavailable; run hoorific migrate: %w", err)
	}
	if v != SchemaVersion {
		return fmt.Errorf("incompatible schema %d, require %d", v, SchemaVersion)
	}
	return nil
}
func (s *Store) Ready(ctx context.Context) error {
	if err := s.DB.PingContext(ctx); err != nil {
		return err
	}
	return s.CheckSchema(ctx)
}
func sqlNow(ctx context.Context, tx *sql.Tx, dialect string) (int64, error) {
	q := "SELECT CAST(strftime('%s','now') AS INTEGER)"
	if dialect == "postgres" {
		q = "SELECT CAST(EXTRACT(EPOCH FROM clock_timestamp()) AS BIGINT)"
	}
	var n int64
	err := tx.QueryRowContext(ctx, q).Scan(&n)
	return n, err
}
func problem(code string, status int, message string) error {
	return core.GatewayError{Code: code, HTTPStatus: status, Message: message, Origin: "gateway"}
}
