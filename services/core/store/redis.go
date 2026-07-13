package store

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

//go:embed lua/token_bucket.lua
var tokenBucketScript string

type RedisStore struct {
	client *redis.Client
	script *redis.Script
}

var _ StateStore = (*RedisStore)(nil)

func NewRedisStore(client *redis.Client) *RedisStore {
	return &RedisStore{
		client: client,
		script: redis.NewScript(tokenBucketScript),
	}
}

func (s *RedisStore) CheckAndDecrement(ctx context.Context, key string, rate, burst, cost int) (bool, int64, error) {
	now := time.Now().UnixMilli()
	result, err := s.script.Run(ctx, s.client, []string{key}, rate, burst, cost, now).Slice()
	if err != nil {
		return false, 0, err
	}
	if len(result) != 2 {
		return false, 0, fmt.Errorf("store: unexpected lua script result shape: %v", result)
	}

	allowed, ok := result[0].(int64)
	if !ok {
		return false, 0, fmt.Errorf("store: unexpected allowed type %T in lua script result", result[0])
	}
	retryAfterMs, ok := result[1].(int64)
	if !ok {
		return false, 0, fmt.Errorf("store: unexpected retryAfterMs type %T in lua script result", result[1])
	}
	return allowed == 1, retryAfterMs, nil
}
