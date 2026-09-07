package coord

import (
	"context"
	"github.com/redis/go-redis/v9"
	"time"
)

// RedisBackend adapts the approved Redis client without making Redis an
// admission authority. Health data is disposable and TTL-bound.
type RedisBackend struct{ Client redis.UniversalClient }

func (r RedisBackend) Get(ctx context.Context, key string) ([]byte, error) {
	if r.Client == nil {
		return nil, ErrNotFound
	}
	b, err := r.Client.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, ErrNotFound
	}
	return b, err
}
func (r RedisBackend) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if r.Client == nil {
		return ErrNotFound
	}
	return r.Client.Set(ctx, key, value, ttl).Err()
}
func (r RedisBackend) Publish(ctx context.Context, channel string, value []byte) error {
	if r.Client == nil {
		return ErrNotFound
	}
	return r.Client.Publish(ctx, channel, value).Err()
}

// Subscribe is parent-context owned and reconnects after transient Redis
// failures. The returned channel closes when ctx is cancelled or the client is
// closed; callers must not use it as SQL state.
func (r RedisBackend) Subscribe(ctx context.Context, channel string) (<-chan []byte, error) {
	if r.Client == nil {
		return nil, ErrNotFound
	}
	if channel == "" {
		return nil, ErrNotFound
	}
	out := make(chan []byte, 32)
	go func() {
		defer close(out)
		backoff := 50 * time.Millisecond
		for {
			if ctx.Err() != nil {
				return
			}
			ps := r.Client.Subscribe(ctx, channel)
			for {
				msg, err := ps.ReceiveMessage(ctx)
				if err == nil {
					backoff = 50 * time.Millisecond
					select {
					case out <- []byte(msg.Payload):
					case <-ctx.Done():
						_ = ps.Close()
						return
					}
					continue
				}
				_ = ps.Close()
				if ctx.Err() != nil {
					return
				}
				timer := time.NewTimer(backoff)
				select {
				case <-timer.C:
				case <-ctx.Done():
					if !timer.Stop() {
						<-timer.C
					}
					return
				}
				if backoff < time.Second {
					backoff *= 2
				}
				break
			}
		}
	}()
	return out, nil
}
