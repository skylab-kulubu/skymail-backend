package accessgate

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

type Decision string

const (
	Allowed     Decision = "allowed"
	Blocked     Decision = "blocked"
	Unavailable Decision = "unavailable"
)

type Reader interface {
	Check(context.Context, string) Decision
	Ready(context.Context) error
}

type redisReader interface {
	MGet(context.Context, ...string) *redis.SliceCmd
	Get(context.Context, string) *redis.StringCmd
}

type RedisGate struct {
	client   redisReader
	deadline time.Duration
}

func NewRedisGate(client redisReader, deadline time.Duration) *RedisGate {
	if deadline <= 0 {
		deadline = 200 * time.Millisecond
	}
	return &RedisGate{client: client, deadline: deadline}
}

func (g *RedisGate) Check(ctx context.Context, subject string) Decision {
	if g == nil || g.client == nil || subject == "" {
		return Unavailable
	}
	opCtx, cancel := context.WithTimeout(ctx, g.deadline)
	defer cancel()

	values, err := g.client.MGet(opCtx, ContractKey, MarkerKey(subject)).Result()
	if err != nil || len(values) != 2 {
		return Unavailable
	}
	contract, ok := values[0].(string)
	if !ok || contract != ContractValue {
		return Unavailable
	}
	if values[1] == nil {
		return Allowed
	}
	marker, ok := values[1].(string)
	if !ok || marker != MarkerValue {
		return Unavailable
	}
	return Blocked
}

func (g *RedisGate) Ready(ctx context.Context) error {
	if g == nil || g.client == nil {
		return errors.New("account access gate unavailable")
	}
	opCtx, cancel := context.WithTimeout(ctx, g.deadline)
	defer cancel()

	value, err := g.client.Get(opCtx, ContractKey).Result()
	if err != nil {
		return errors.New("account access gate unavailable")
	}
	if value != ContractValue {
		return errors.New("account access gate contract mismatch")
	}
	return nil
}
