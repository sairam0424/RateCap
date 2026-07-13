-- KEYS[1] = concurrency set key
-- ARGV[1] = cap (max concurrent slots)
-- ARGV[2] = max_duration_ms (reap cutoff)
-- ARGV[3] = now (unix millis)
-- ARGV[4] = token (random member to add if allowed)
--
-- Returns {allowed (1/0), token or empty string}

local key = KEYS[1]
local cap = tonumber(ARGV[1])
local max_duration_ms = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local token = ARGV[4]

redis.call("ZREMRANGEBYSCORE", key, "-inf", now - max_duration_ms)

local count = redis.call("ZCARD", key)

if count < cap then
  redis.call("ZADD", key, now, token)
  redis.call("EXPIRE", key, math.ceil(max_duration_ms / 1000) + 60)
  return {1, token}
else
  return {0, ""}
end
