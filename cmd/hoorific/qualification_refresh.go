//go:build qualification

package main

import (
	"context"
	"errors"
	"fmt"
	"hoorific/internal/credential"
	"hoorific/internal/store"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const qualificationRefreshCommitGateEnv = "HOORIFIC_QUALIFICATION_REFRESH_COMMIT_GATE"

type gatedCredentialRepository struct {
	credential.Repository
	gate qualificationRefreshGate
}

type qualificationRefreshGate struct {
	ready   string
	release string
}

// gatedCredentialRepository is compiled only into the explicitly requested
// qualification binary. It is intentionally a repository wrapper: the normal
// gateway request, credential validation, token parsing and envelope sealing all
// run unchanged before CommitRefresh reaches this boundary.
func qualificationRefreshRepository(db *store.Store) (credential.Repository, error) {
	if db == nil {
		return nil, errors.New("credential store is required")
	}
	raw := os.Getenv(qualificationRefreshCommitGateEnv)
	if raw == "" {
		return db, nil
	}
	if strings.TrimSpace(raw) != raw {
		return nil, errors.New("qualification refresh gate must not contain surrounding whitespace")
	}
	path := raw
	gate, err := newQualificationRefreshGate(path)
	if err != nil {
		return nil, err
	}
	return &gatedCredentialRepository{Repository: db, gate: gate}, nil
}

func newQualificationRefreshGate(path string) (qualificationRefreshGate, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return qualificationRefreshGate{}, errors.New("qualification refresh gate must be a clean absolute path")
	}
	parent := filepath.Dir(path)
	info, err := os.Stat(parent)
	if err != nil {
		return qualificationRefreshGate{}, fmt.Errorf("qualification refresh gate parent: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return qualificationRefreshGate{}, errors.New("qualification refresh gate parent must be a private directory")
	}
	ready, release := path+".ready", path+".release"
	for _, name := range []string{path, ready, release} {
		if _, err := os.Lstat(name); err == nil {
			return qualificationRefreshGate{}, fmt.Errorf("qualification refresh gate path already exists: %s", name)
		} else if !errors.Is(err, os.ErrNotExist) {
			return qualificationRefreshGate{}, fmt.Errorf("qualification refresh gate path: %w", err)
		}
	}
	return qualificationRefreshGate{ready: ready, release: release}, nil
}

func (r *gatedCredentialRepository) CommitRefresh(ctx context.Context, intent credential.RefreshIntent, next credential.Record) error {
	if err := r.gate.await(ctx); err != nil {
		return err
	}
	return r.Repository.CommitRefresh(ctx, intent, next)
}
func (r *gatedCredentialRepository) LoadUsableCredential(ctx context.Context, tenant, id string) (credential.Record, error) {
	loader, ok := r.Repository.(interface {
		LoadUsableCredential(context.Context, string, string) (credential.Record, error)
	})
	if !ok {
		return credential.Record{}, credential.ErrInvalidCredential
	}
	return loader.LoadUsableCredential(ctx, tenant, id)
}

func (g qualificationRefreshGate) await(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ready, err := os.OpenFile(g.ready, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("qualification refresh gate ready file: %w", err)
	}
	if _, err = ready.Write([]byte{1}); err != nil {
		_ = ready.Close()
		return fmt.Errorf("qualification refresh gate readiness: %w", err)
	}
	if err = ready.Close(); err != nil {
		return fmt.Errorf("qualification refresh gate readiness close: %w", err)
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		info, statErr := os.Lstat(g.release)
		switch {
		case statErr == nil:
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return errors.New("qualification refresh gate release path must be a regular file")
			}
			return nil
		case errors.Is(statErr, os.ErrNotExist):
			// Keep the process at the post-validation, pre-commit boundary.
		default:
			return fmt.Errorf("qualification refresh gate release file: %w", statErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

var _ credential.Repository = (*gatedCredentialRepository)(nil)
