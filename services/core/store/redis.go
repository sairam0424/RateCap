package store

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	coremetrics "github.com/ratecap/core/metrics"
)

//go:embed lua/token_bucket.lua
var tokenBucketScript string

//go:embed lua/concurrent_limiter.lua
var concurrentLimiterScript string

//go:embed lua/refund_tokens.lua
var refundTokensScript string

type RedisStore struct {
	client            *redis.Client
	tokenBucket       *redis.Script
	concurrentLimiter *redis.Script
	refundTokens      *redis.Script
	signingKey        []byte
}

var _ StateStore = (*RedisStore)(nil)

func NewRedisStore(client *redis.Client, signingKey []byte) *RedisStore {
	return &RedisStore{
		client:            client,
		tokenBucket:       redis.NewScript(tokenBucketScript),
		concurrentLimiter: redis.NewScript(concurrentLimiterScript),
		refundTokens:      redis.NewScript(refundTokensScript),
		signingKey:        signingKey,
	}
}

// signToken embeds an HMAC-SHA256 signature into the returned token
// (<uuid>.<hex-hmac>) so ReleaseConcurrency can verify a token was actually
// issued by this core instance before releasing it — closing the
// forgeable-bearer-token gap in issue #12. The Lua script and DecrConcurrent
// both treat the whole string as an opaque Redis set member, so this needs
// zero changes on that side.
func signToken(candidateUUID string, signingKey []byte) string {
	mac := hmac.New(sha256.New, signingKey)
	mac.Write([]byte(candidateUUID))
	return candidateUUID + "." + hex.EncodeToString(mac.Sum(nil))
}

// rateLimiterKeyPrefix and concurrencyKeyPrefix keep the two tiers' Redis
// keys disjoint. Both tiers key off the same caller-supplied req.Key (e.g.
// Pipeline checks Tier 1 then Tier 2 for one request), but they store
// different Redis types (hash vs sorted set) — without a prefix, a shared
// key errors with WRONGTYPE the moment both tiers touch it.
const rateLimiterKeyPrefix = "rl:"
const concurrencyKeyPrefix = "cc:"

func (s *RedisStore) CheckAndDecrement(ctx context.Context, key string, rate, burst, cost int) (bool, int64, error) {
	key = rateLimiterKeyPrefix + key
	now := time.Now().UnixMilli()
	start := time.Now()
	result, err := s.tokenBucket.Run(ctx, s.client, []string{key}, rate, burst, cost, now).Slice()
	coremetrics.RecordRedisCall("check_and_decrement", time.Since(start), err)
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
	key = concurrencyKeyPrefix + key
	now := time.Now().UnixMilli()
	candidateToken := signToken(uuid.NewString(), s.signingKey)

	start := time.Now()
	result, err := s.concurrentLimiter.Run(ctx, s.client, []string{key}, cap, maxDurationMs, now, candidateToken).Slice()
	coremetrics.RecordRedisCall("incr_concurrent", time.Since(start), err)
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

func (s *RedisStore) RefundTokens(ctx context.Context, key string, burst, refundAmount int) error {
	key = rateLimiterKeyPrefix + key
	start := time.Now()
	err := s.refundTokens.Run(ctx, s.client, []string{key}, burst, refundAmount).Err()
	coremetrics.RecordRedisCall("refund_tokens", time.Since(start), err)
	return err
}

func (s *RedisStore) DecrConcurrent(ctx context.Context, key, token string) error {
	start := time.Now()
	err := s.client.ZRem(ctx, concurrencyKeyPrefix+key, token).Err()
	coremetrics.RecordRedisCall("decr_concurrent", time.Since(start), err)
	return err
}
