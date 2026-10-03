-- Sliding-window log, executed atomically by Redis. Each accepted request is a
-- member of a sorted set scored by its timestamp. Mirrors limiter.Log.Take in
-- algorithms.go; all times are microseconds.
--
-- KEYS[1] = sorted-set key
-- ARGV[1] = now (µs since epoch)
-- ARGV[2] = limit
-- ARGV[3] = window (µs)
-- ARGV[4] = unique member id for this request
--
-- Returns {allowed (0/1), remaining, retry_after_us, reset_after_us}
local key = KEYS[1]
local now = tonumber(ARGV[1])
local limit = tonumber(ARGV[2])
local window = tonumber(ARGV[3])
local member = ARGV[4]

-- Drop everything at or before now - window (same boundary as the Go version).
redis.call('ZREMRANGEBYSCORE', key, '-inf', string.format('%.0f', now - window))
local count = redis.call('ZCARD', key)

local allowed = 0
local retry = 0
if count < limit then
  redis.call('ZADD', key, string.format('%.0f', now), member)
  count = count + 1
  allowed = 1
else
  -- count-limit+1 entries must expire before there is room; the last of them is
  -- at rank count-limit (rank 0 unless a hot reload lowered the limit).
  local blocking = redis.call('ZRANGE', key, count - limit, count - limit, 'WITHSCORES')
  retry = tonumber(blocking[2]) + window - now
end

local reset = 0
local newest = redis.call('ZRANGE', key, -1, -1, 'WITHSCORES')
if newest[2] then
  reset = tonumber(newest[2]) + window - now
  redis.call('PEXPIRE', key, math.ceil(reset / 1000) + 1000)
end

return {allowed, math.max(limit - count, 0), retry, reset}
