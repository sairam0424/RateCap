-- KEYS[1] = bucket key
-- ARGV[1] = burst (max bucket capacity, for clamping)
-- ARGV[2] = refund_amount (tokens to give back)
--
-- Returns the new token count after the refund. Clamps on write,
-- unconditionally — unlike token_bucket.lua's refill arithmetic, which
-- only re-clamps to burst on the NEXT call, this script must never leave
-- the bucket above burst even transiently, since nothing else reads or
-- corrects it until the next real check.

local key = KEYS[1]
local burst = tonumber(ARGV[1])
local refund_amount = tonumber(ARGV[2])

local tokens = tonumber(redis.call("HGET", key, "tokens"))
if tokens == nil then
  -- Bucket was never created (or has since expired) — nothing was ever
  -- decremented from it, so there is nothing to refund into.
  return 0
end

tokens = math.min(burst, tokens + refund_amount)
redis.call("HSET", key, "tokens", tokens)
return tokens
