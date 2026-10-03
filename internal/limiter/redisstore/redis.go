// Package redisstore shares limiter state across gateway replicas through Redis.
// Each Lua script performs one atomic read-modify-write to prevent races between
// replicas checking the same key.
package redisstore

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/srujanmalakpata/floodgate/internal/clock"
	"github.com/srujanmalakpata/floodgate/internal/limiter"
)

var (
	//go:embed token_bucket.lua
	tokenBucketSrc string
	//go:embed sliding_window.lua
	slidingWindowSrc string

	// redis.Script sends EVALSHA and falls back to EVAL (which also caches the
	// script) the first time a Redis node has not seen it.
	tokenBucketScript   = redis.NewScript(tokenBucketSrc)
	slidingWindowScript = redis.NewScript(slidingWindowSrc)
)

// Store is a Redis-backed limiter.Limiter.
type Store struct {
	client    redis.Scripter
	clock     clock.Clock
	prefix    string
	replicaID string
	seq       atomic.Uint64
}

var _ limiter.Limiter = (*Store)(nil)

// New returns a Store. prefix namespaces keys (e.g. "rlgw"); client may be a
// *redis.Client, *redis.ClusterClient or *redis.Ring.
func New(client redis.Scripter, c clock.Clock, prefix string) *Store {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return &Store{client: client, clock: c, prefix: prefix, replicaID: hex.EncodeToString(b[:])}
}

// Allow implements limiter.Limiter by running the rule's Lua script.
func (s *Store) Allow(ctx context.Context, key string, rule limiter.Rule) (limiter.Decision, error) {
	nowUS := s.clock.Now().UnixMicro()
	windowUS := rule.Window.Microseconds()
	redisKey := s.prefix + ":" + string(rule.Algorithm) + ":" + key

	var (
		res []int64
		err error
	)
	switch rule.Algorithm {
	case limiter.SlidingWindow:
		// The member must be unique even if two replicas see the same microsecond.
		member := strconv.FormatInt(nowUS, 10) + "-" + s.replicaID + "-" + strconv.FormatUint(s.seq.Add(1), 10)
		res, err = slidingWindowScript.Run(ctx, s.client, []string{redisKey}, nowUS, rule.Limit, windowUS, member).Int64Slice()
	default:
		res, err = tokenBucketScript.Run(ctx, s.client, []string{redisKey}, nowUS, rule.Limit, windowUS).Int64Slice()
	}
	if err != nil {
		return limiter.Decision{}, fmt.Errorf("redis %s script: %w", rule.Algorithm, err)
	}
	if len(res) != 4 {
		return limiter.Decision{}, fmt.Errorf("redis %s script: unexpected reply length %d", rule.Algorithm, len(res))
	}
	return limiter.Decision{
		Allowed:    res[0] == 1,
		Limit:      rule.Limit,
		Remaining:  max(res[1], 0),
		RetryAfter: time.Duration(res[2]) * time.Microsecond,
		ResetAfter: time.Duration(res[3]) * time.Microsecond,
	}, nil
}
