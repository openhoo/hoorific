package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"hoorific/internal/core"
)

type RoutingHints interface {
	Healthy(context.Context, core.RouteTarget) (bool, error)
	Set(context.Context, core.RouteTarget, bool, time.Duration) error
	GetAffinity(context.Context, string) (core.RouteTarget, bool, error)
	SetAffinity(context.Context, string, core.RouteTarget, time.Duration) error
}

func closeCredentialLease(lease core.CredentialLease) {
	_ = core.CloseCredentialLease(lease)
}

func (g *Gateway) finalize(ctx context.Context, outcome core.AttemptOutcome) {
	deadline, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	for {
		if err := g.deps.Admission.FinalizeAttempt(deadline, outcome); err == nil {
			return
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-deadline.Done():
			timer.Stop()
			slog.Error("attempt finalization remains unresolved", "request_id", outcome.RequestID, "attempt_id", outcome.AttemptID)
			return
		case <-timer.C:
		}
	}
}

type preIntentStaleError struct{ cause error }

func (e preIntentStaleError) Error() string { return e.cause.Error() }
func (e preIntentStaleError) Unwrap() error { return e.cause }
func isPreIntentStale(err error) bool       { var stale preIntentStaleError; return errors.As(err, &stale) }
func markPreIntentStale(err error) error {
	if err == nil {
		return nil
	}
	var ge core.GatewayError
	if errors.As(err, &ge) {
		if ge.Code == "configuration_stale" {
			return preIntentStaleError{cause: err}
		}
		return err
	}
	// Planning/admission failure without a public domain error means the
	// authoritative boundary is unavailable. Never dispatch or expose SQL details.
	return failure("unavailable", http.StatusServiceUnavailable, "admission is unavailable")
}
func (g *Gateway) refreshSnapshot(ctx context.Context) (core.RuntimeSnapshot, error) {
	if source, ok := g.deps.Snapshots.(interface {
		RefreshSnapshot(context.Context) (core.RuntimeSnapshot, error)
	}); ok {
		return source.RefreshSnapshot(ctx)
	}
	return g.deps.Snapshots.Snapshot(ctx)
}
func sameOrigin(a, b string) bool {
	left, e := url.Parse(a)
	if e != nil {
		return false
	}
	right, e := url.Parse(b)
	return e == nil && strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}
func cooldownDuration(h http.Header) time.Duration {
	value := h.Get("Retry-After")
	duration := time.Second
	if seconds, err := strconv.ParseInt(value, 10, 32); err == nil && seconds > 0 {
		duration = time.Duration(seconds) * time.Second
	} else if until, err := http.ParseTime(value); err == nil {
		duration = time.Until(until)
	}
	return min(max(duration, time.Second), 5*time.Minute)
}
