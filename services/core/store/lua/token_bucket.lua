-- KEYS[1] = bucket key
-- ARGV[1] = rate (tokens per second)
-- ARGV[2] = burst (max bucket capacity)
-- ARGV[3] = cost (tokens requested)
-- ARGV[4] = now (unix millis)
--
-- Returns {allowed (1/0), retry_after_ms}

local key = KEYS[1]
local rate = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local cost = tonumber(ARGV[3])
local now = tonumber(ARGV[4])

local bucket = redis.call("HMGET", key, "tokens", "updated_at")
local tokens = tonumber(bucket[1])
local updated_at = tonumber(bucket[2])

if tokens == nil then
  tokens = burst
  updated_at = now
end

local elapsed_ms = math.max(0, now - updated_at)
local refill = (elapsed_ms / 1000) * rate
tokens = math.min(burst, tokens + refill)

local allowed, retry_after_ms
if tokens >= cost then
  tokens = tokens - cost
  allowed, retry_after_ms = 1, 0
else
  local deficit = cost - tokens
  allowed, retry_after_ms = 0, math.ceil((deficit / rate) * 1000)
end

redis.call("HSET", key, "tokens", tokens, "updated_at", now)
redis.call("EXPIRE", key, math.ceil(burst / rate) + 60)
return {allowed, retry_after_ms}
