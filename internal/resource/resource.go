// Package resource owns durable mappings for provider-native asynchronous work.
package resource

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"
	"time"

	"hoorific/internal/core"
)

type Job struct {
	TenantID, ConnectionID, AccountID, ResourceID, Operation, Status, RequestID string
	Usage                                                                       *core.Usage
	CreatedAt, UpdatedAt, NextPoll                                              time.Time
	LeaseOwner                                                                  string
	Fence                                                                       int64
	LeaseUntil                                                                  time.Time
	SettlementPending                                                           bool
	Metadata                                                                    []byte
}
type Continuation struct {
	ID, TenantID, ConnectionID, AccountID, UploadID, UpstreamURL string
	ExpiresAt                                                    time.Time
	Ciphertext                                                   []byte
	KeyID                                                        string
	Metadata                                                     []byte
}
type Store interface {
	PutJob(context.Context, Job) error
	GetJob(context.Context, string, string, string) (Job, error)
	ClaimJobs(context.Context, string, time.Time, time.Duration, int) ([]Job, error)
	UpdateJob(context.Context, Job, string, int64) error
	PutContinuation(context.Context, Continuation) error
	GetContinuation(context.Context, string, string, string) (Continuation, error)
	DeleteContinuation(context.Context, string, string, string) error
}
type Reconciler struct {
	store Store
	now   func() time.Time
}

func NewReconciler(s Store) *Reconciler { return &Reconciler{store: s, now: time.Now} }

// UnknownCreate records an acknowledgement-loss outcome without retrying the provider create.
func UnknownCreate(tenant, connection, account, operation, request string, now time.Time) Job {
	return Job{TenantID: tenant, ConnectionID: connection, AccountID: account, ResourceID: "unknown:" + request, Operation: operation, RequestID: request, Status: "outcome_unknown", CreatedAt: now, UpdatedAt: now}
}

// Reconcile updates only a row claimed by owner at its exact fencing token.
func (r *Reconciler) Reconcile(ctx context.Context, job Job, owner string, fence int64, status string, usage *core.Usage) error {
	if owner == "" || fence <= 0 {
		return errors.New("invalid reconciliation fence")
	}
	if job.LeaseOwner != owner || job.Fence != fence {
		return errors.New("stale reconciliation lease")
	}
	if (job.Status == "completed" || job.Status == "failed" || job.Status == "cancelled") && !job.SettlementPending {
		return nil
	}
	job.Status = status
	job.Usage = usage
	job.UpdatedAt = r.now()
	job.LeaseOwner = ""
	job.LeaseUntil = time.Time{}
	return r.store.UpdateJob(ctx, job, owner, fence)
}
func ValidateScope(c Continuation, tenant, connection, account string, now time.Time) error {
	if c.TenantID != tenant || c.ConnectionID != connection || c.AccountID != account {
		return errors.New("continuation scope mismatch")
	}
	if c.ID == "" || !c.ExpiresAt.After(now) {
		return errors.New("continuation expired")
	}
	if c.UpstreamURL == "" {
		if len(c.Ciphertext) == 0 {
			return errors.New("continuation URL missing")
		}
		return nil
	}
	if !validContinuationURL(c.UpstreamURL) {
		return errors.New("invalid continuation URL")
	}
	return nil
}
func AllowedContinuationOrigin(raw string, origins []string) bool {
	u, err := url.Parse(raw)
	if err != nil || !continuationSchemeAllowed(u) {
		return false
	}
	for _, origin := range origins {
		v, e := url.Parse(origin)
		if e == nil && strings.EqualFold(v.Scheme, u.Scheme) && strings.EqualFold(v.Host, u.Host) {
			return true
		}
	}
	return false
}
func IDForContinuation(tenant, connection, nonce string) string {
	sum := sha256.Sum256([]byte(tenant + "\x00" + connection + "\x00" + nonce))
	return hex.EncodeToString(sum[:16])
}
func IDForScopedContinuation(tenant, connection, account, nonce string) string {
	sum := sha256.Sum256([]byte(tenant + "\x00" + connection + "\x00" + account + "\x00" + nonce))
	return hex.EncodeToString(sum[:16])
}
