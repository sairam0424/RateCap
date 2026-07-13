package store

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

//go:embed lua/token_bucket.lua
var tokenBucketScript string

//go:embed lua/concurrent_limiter.lua
var concurrentLimiterScript string

type RedisStore struct {
	client            *redis.Client
	tokenBucket       *redis.Script
	concurrentLimiter *redis.Script
}

var _ StateStore = (*RedisStore)(nil)

func NewRedisStore(client *redis.Client) *RedisStore {
	return &RedisStore{
		client:            client,
		tokenBucket:       redis.NewScript(tokenBucketScript),
		concurrentLimiter: redis.NewScript(concurrentLimiterScript),
	}
}

func (s *RedisStore) CheckAndDecrement(ctx context.Context, key string, rate, burst, cost int) (bool, int64, error) {
	now := time.Now().UnixMilli()
	result, err := s.tokenBucket.Run(ctx, s.client, []string{key}, rate, burst, cost, now).Slice()
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

func (s *RedisStore) IncrConcurrent(ctx context.Context, key string, cap int, maxDurationMs int64) (bool, string, error) {
	now := time.Now().UnixMilli()
	candidateToken := uuid.NewString()

	result, err := s.concurrentLimiter.Run(ctx, s.client, []string{key}, cap, maxDurationMs, now, candidateToken).Slice()
	if err != nil {
		return false, "", err
	}
	if len(result) != 2 {
		return false, "", fmt.Errorf("store: unexpected lua script result shape: %v", result)
	}

	allowed, ok := result[0].(int64)
	if !ok {
		return false, "", fmt.Errorf("store: unexpected allowed type %T in lua script result", result[0])
	}
	token, ok := result[1].(string)
	if !ok {
		return false, "", fmt.Errorf("store: unexpected token type %T in lua script result", result[1])
	}
	return allowed == 1, token, nil
}

func (s *RedisStore) DecrConcurrent(ctx context.Context, key, token string) error {
	return s.client.ZRem(ctx, key, token).Err()
}
