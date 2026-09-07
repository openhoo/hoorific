package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"hoorific/internal/realtime"
	"time"
)

func (s *Store) PutTicket(ctx context.Context, t realtime.Ticket) error {
	if t.TenantID == "" || t.ConnectionID == "" || t.ModelID == "" || len(t.IDHash) == 0 || !t.ExpiresAt.After(time.Now()) {
		return errors.New("invalid realtime ticket")
	}
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		return s.durablePut(ctx, tx, t.TenantID, "realtime_ticket", hex.EncodeToString(t.IDHash), 0, t)
	})
}

// ConsumeTicketBound atomically locks, validates route scope/expiry/used state,
// and deletes the bearer record. The stored principal/key metadata is returned
// for the caller's current authorization checks.
func (s *Store) ConsumeTicketBound(ctx context.Context, hash []byte, now time.Time, connection, model string) (out realtime.Ticket, err error) {
	id := hex.EncodeToString(hash)
	err = s.WithTx(ctx, func(tx *sql.Tx) error {
		var t realtime.Ticket
		_, _, e := s.durableGetAny(ctx, tx, "realtime_ticket", id, &t)
		if e != nil {
			return e
		}
		if t.Used || !t.ExpiresAt.After(now) || t.ConnectionID != connection || t.ModelID != model {
			return errors.New("invalid or expired realtime ticket")
		}
		if e = s.durableDelete(ctx, tx, t.TenantID, "realtime_ticket", id); e != nil {
			return e
		}
		out = t
		return nil
	})
	return
}

var _ realtime.TicketStore = (*Store)(nil)
