// Package limiter defines the rate-limiting contract shared by every backend and
// the pure algorithm math (token bucket, sliding-window log) that the in-memory
// backend executes directly and the Redis Lua scripts mirror.
package limiter

import (
	"context"
	"fmt"
	"time"
)

// Algorithm selects how a Rule is enforced.
type Algorithm string

const (
	// TokenBucket allows bursts up to Limit and refills Limit tokens per Window.
	TokenBucket Algorithm = "token_bucket"
	// SlidingWindow allows at most Limit requests in any trailing Window (exact log).
	SlidingWindow Algorithm = "sliding_window"
)

// MaxSlidingWindowLimit bounds a sliding-window rule's limit. The log keeps one
// timestamp per admitted request (about 24 bytes in Go, more in a Redis sorted
// set), so a typo such as limit: 1000000 would make per-key memory unbounded.
const MaxSlidingWindowLimit = 10_000

// MaxTokenBucketLimit bounds a token-bucket rule's limit at 2^53, the largest
// integer a float64 (the bucket's token count, and a Lua number in the Redis
// script) represents exactly. Above it the token count loses precision and
// converting it to int64 for RateLimit-Remaining can overflow.
const MaxTokenBucketLimit = 1 << 53

// Valid reports whether a is a known algorithm.
func (a Algorithm) Valid() bool { return a == TokenBucket || a == SlidingWindow }

// Rule is one rate limit: Limit requests per Window using Algorithm.
type Rule struct {
	Algorithm Algorithm
	Limit     int64
	Window    time.Duration
}

// Validate checks that the rule is usable.
func (r Rule) Validate() error {
	if !r.Algorithm.Valid() {
		return fmt.Errorf("unknown algorithm %q (want %q or %q)", r.Algorithm, TokenBucket, SlidingWindow)
	}
	if r.Limit <= 0 {
		return fmt.Errorf("limit must be > 0, got %d", r.Limit)
	}
	if r.Algorithm == TokenBucket && r.Limit > MaxTokenBucketLimit {
		return fmt.Errorf("token_bucket limit must be <= %d (2^53), got %d", int64(MaxTokenBucketLimit), r.Limit)
	}
	if r.Algorithm == SlidingWindow && r.Limit > MaxSlidingWindowLimit {
		return fmt.Errorf("sliding_window limit must be <= %d (it stores one entry per request, "+
			"so memory is O(limit) per key); use token_bucket for high limits, got %d", MaxSlidingWindowLimit, r.Limit)
	}
	if r.Window < time.Millisecond {
		return fmt.Errorf("window must be >= 1ms, got %s", r.Window)
	}
	return nil
}

// Decision is the outcome of one Allow call.
type Decision struct {
	Allowed bool
	// Limit echoes the rule's limit (for RateLimit-Limit).
	Limit int64
	// Remaining is how many more requests would be allowed right now.
	Remaining int64
	// RetryAfter is how long a denied caller should wait. Zero when allowed.
	RetryAfter time.Duration
	// ResetAfter is how long until the limiter is back to its full quota.
	ResetAfter time.Duration
	// Degraded is true when the decision was made by a fallback policy because
	// the primary (shared) backend was unavailable.
	Degraded bool
	// Unavailable is true when the limit could not be checked at all and the
	// request is rejected anyway (fail-closed). It is a server-side condition,
	// not the client exceeding its quota.
	Unavailable bool
}

// Limiter decides whether the request identified by key may proceed under rule.
// Implementations must be safe for concurrent use.
type Limiter interface {
	Allow(ctx context.Context, key string, rule Rule) (Decision, error)
}
