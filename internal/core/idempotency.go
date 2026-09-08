package core

import (
	"context"
	"errors"
	"net/http"
	"time"
)

var (
	ErrIdempotencyInvalid             = errors.New("invalid idempotency request")
	ErrIdempotencyFingerprintConflict = errors.New("idempotency key fingerprint conflict")
	ErrIdempotencyOwnerFenced         = errors.New("idempotency owner is fenced")
	ErrIdempotencyNotFound            = errors.New("idempotency record not found")
	ErrIdempotencyPending             = errors.New("idempotency request is pending")
	ErrIdempotencyUnreplayable        = errors.New("idempotency request is unreplayable")
)

// IdempotencyRequest identifies one authenticated request. SubjectID is the
// authenticated session/key identity, never the raw credential. OwnerID is a
// per-dispatch random fence chosen by the gateway; it is not an auth identity.
// ExpiresAt is the request's bounded claim hint. Stores measure completed
// replay retention from successful completion, while pending and unreplayable
// records ignore expiry and remain terminal.
type IdempotencyRequest struct {
	TenantID, SubjectID, Key, Fingerprint, OwnerID string
	ExpiresAt                                      time.Time
}

// IdempotencyResponse is the replayable terminal response. Header and Body
// are copied by store implementations; callers retain ownership of the input.
type IdempotencyResponse struct {
	Status int
	Header http.Header
	Body   []byte
}

// IdempotencyRecord reports the durable claim state. A complete record carries
// a response; pending and unreplayable records intentionally carry none and
// must never be treated as permission to dispatch again.
type IdempotencyRecord struct {
	State    string
	Response *IdempotencyResponse
}

// IdempotencyStore atomically claims and completes authenticated idempotency
// keys. Implementations must fence completion by the original OwnerID and must
// durably retain pending/unknown outcomes as unreplayable tombstones.
type IdempotencyStore interface {
	BeginIdempotency(context.Context, IdempotencyRequest) (IdempotencyRecord, error)
	FinishIdempotency(context.Context, IdempotencyRequest, *IdempotencyResponse) error
}
