-- Token bucket, executed atomically by Redis (scripts run single-threaded, so no
-- other client can interleave between the read and the write below).
-- Mirrors limiter.Bucket.Take in algorithms.go; all times are microseconds.
--
-- KEYS[1] = bucket hash key
-- ARGV[1] = now (µs since epoch, from the gateway's clock)
-- ARGV[2] = capacity (rule limit)
-- ARGV[3] = window (µs); refill rate = capacity / window tokens per µs
--
-- Returns {allowed (0/1), remaining, retry_after_us, reset_after_us}
local key = KEYS[1]
local now = tonumber(ARGV[1])
local capacity = tonumber(ARGV[2])
local window = tonumber(ARGV[3])
local rate = capacity / window

local state = redis.call('HMGET', key, 'tokens', 'ts')
local tokens = tonumber(state[1])
local ts = tonumber(state[2])
if tokens == nil or ts == nil then
  tokens = capacity
  ts = now
end

-- Replicas' clocks can disagree slightly; never refill for negative time and
-- never move the reference point backwards.
if now > ts then
  tokens = tokens + (now - ts) * rate
  ts = now
end
if tokens > capacity then
  tokens = capacity
end

local allowed = 0
local retry = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
else
  retry = math.ceil((1 - tokens) / rate)
end
local reset = math.ceil((capacity - tokens) / rate)

redis.call('HSET', key, 'tokens', string.format('%.17g', tokens), 'ts', string.format('%.0f', ts))
-- Once the bucket would be full again the key carries no information, so let it expire.
redis.call('PEXPIRE', key, math.ceil(reset / 1000) + 1000)

return {allowed, math.floor(tokens), retry, reset}
