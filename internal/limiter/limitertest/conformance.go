// Package limitertest is a conformance suite every limiter backend must pass,
// so the in-memory Go implementation and the Redis Lua scripts provably agree.
package limitertest

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srujanmalakpata/floodgate/internal/clock"
	"github.com/srujanmalakpata/floodgate/internal/limiter"
)

// Epoch is the fake clock's start time used by the suite.
var Epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// Factory builds a fresh, empty limiter driven by clk.
type Factory func(t *testing.T, clk clock.Clock) limiter.Limiter

// step is one call in a scenario: advance the clock, call Allow, check the result.
type step struct {
	advance    time.Duration
	key        string // defaults to "k"
	allowed    bool
	remaining  int64
	retryAfter time.Duration
	resetAfter time.Duration
}

var (
	tb3per3s   = limiter.Rule{Algorithm: limiter.TokenBucket, Limit: 3, Window: 3 * time.Second}
	tb10per10s = limiter.Rule{Algorithm: limiter.TokenBucket, Limit: 10, Window: 10 * time.Second}
	sw2per10s  = limiter.Rule{Algorithm: limiter.SlidingWindow, Limit: 2, Window: 10 * time.Second}
)

var scenarios = []struct {
	name  string
	rule  limiter.Rule
	steps []step
}{
	{
		name: "token bucket allows a burst up to the limit then denies",
		rule: tb3per3s,
		steps: []step{
			{allowed: true, remaining: 2, resetAfter: time.Second},
			{allowed: true, remaining: 1, resetAfter: 2 * time.Second},
			{allowed: true, remaining: 0, resetAfter: 3 * time.Second},
			{allowed: false, remaining: 0, retryAfter: time.Second, resetAfter: 3 * time.Second},
		},
	},
	{
		name: "token bucket refills at limit/window",
		rule: tb3per3s,
		steps: []step{
			{allowed: true, remaining: 2, resetAfter: time.Second},
			{allowed: true, remaining: 1, resetAfter: 2 * time.Second},
			{allowed: true, remaining: 0, resetAfter: 3 * time.Second},
			{advance: 500 * time.Millisecond, allowed: false, remaining: 0, retryAfter: 500 * time.Millisecond, resetAfter: 2500 * time.Millisecond},
			{advance: 500 * time.Millisecond, allowed: true, remaining: 0, resetAfter: 3 * time.Second},
		},
	},
	{
		name: "token bucket never refills beyond capacity",
		rule: tb10per10s,
		steps: []step{
			{allowed: true, remaining: 9, resetAfter: time.Second},
			{advance: time.Hour, allowed: true, remaining: 9, resetAfter: time.Second},
		},
	},
	{
		name: "token bucket keys are independent",
		rule: tb3per3s,
		steps: []step{
			{key: "a", allowed: true, remaining: 2, resetAfter: time.Second},
			{key: "a", allowed: true, remaining: 1, resetAfter: 2 * time.Second},
			{key: "a", allowed: true, remaining: 0, resetAfter: 3 * time.Second},
			{key: "a", allowed: false, remaining: 0, retryAfter: time.Second, resetAfter: 3 * time.Second},
			{key: "b", allowed: true, remaining: 2, resetAfter: time.Second},
		},
	},
	{
		name: "sliding window admits limit requests per trailing window",
		rule: sw2per10s,
		steps: []step{
			{allowed: true, remaining: 1, resetAfter: 10 * time.Second},                                                        // t=0
			{advance: 3 * time.Second, allowed: true, remaining: 0, resetAfter: 10 * time.Second},                              // t=3
			{advance: 2 * time.Second, allowed: false, remaining: 0, retryAfter: 5 * time.Second, resetAfter: 8 * time.Second}, // t=5
			{advance: 5 * time.Second, allowed: true, remaining: 0, resetAfter: 10 * time.Second},                              // t=10: t=0 expired
			{advance: time.Second, allowed: false, remaining: 0, retryAfter: 2 * time.Second, resetAfter: 9 * time.Second},     // t=11
			{advance: 2 * time.Second, allowed: true, remaining: 0, resetAfter: 10 * time.Second},                              // t=13
		},
	},
	{
		name: "sliding window forgets everything after a full window",
		rule: sw2per10s,
		steps: []step{
			{allowed: true, remaining: 1, resetAfter: 10 * time.Second},
			{allowed: true, remaining: 0, resetAfter: 10 * time.Second},
			{advance: 10 * time.Second, allowed: true, remaining: 1, resetAfter: 10 * time.Second},
		},
	},
	{
		name: "sliding window has no burst at the boundary unlike a fixed window",
		rule: sw2per10s,
		steps: []step{
			{advance: 9 * time.Second, allowed: true, remaining: 1, resetAfter: 10 * time.Second},                              // t=9
			{allowed: true, remaining: 0, resetAfter: 10 * time.Second},                                                        // t=9
			{advance: 2 * time.Second, allowed: false, remaining: 0, retryAfter: 8 * time.Second, resetAfter: 8 * time.Second}, // t=11: a fixed window would reset at t=10
		},
	},
}

// tolerance absorbs the Redis scripts working in whole microseconds and float rounding.
const tolerance = time.Millisecond

// Run executes the conformance suite against newLimiter.
func Run(t *testing.T, newLimiter Factory) {
	t.Helper()
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			clk := clock.NewFake(Epoch)
			l := newLimiter(t, clk)
			for i, st := range sc.steps {
				clk.Advance(st.advance)
				key := st.key
				if key == "" {
					key = "k"
				}
				d, err := l.Allow(context.Background(), key, sc.rule)
				if err != nil {
					t.Fatalf("step %d: Allow: %v", i, err)
				}
				if d.Allowed != st.allowed || d.Remaining != st.remaining || d.Limit != sc.rule.Limit {
					t.Fatalf("step %d: got allowed=%v remaining=%d limit=%d, want allowed=%v remaining=%d limit=%d",
						i, d.Allowed, d.Remaining, d.Limit, st.allowed, st.remaining, sc.rule.Limit)
				}
				if !near(d.RetryAfter, st.retryAfter) {
					t.Errorf("step %d: RetryAfter = %v, want %v", i, d.RetryAfter, st.retryAfter)
				}
				if !near(d.ResetAfter, st.resetAfter) {
					t.Errorf("step %d: ResetAfter = %v, want %v", i, d.ResetAfter, st.resetAfter)
				}
			}
		})
	}

	t.Run("clock moving backwards grants nothing", func(t *testing.T) {
		clk := clock.NewFake(Epoch)
		l := newLimiter(t, clk)
		for range 3 {
			mustAllow(t, l, tb3per3s)
		}
		clk.Set(Epoch.Add(-time.Hour))
		if d, _ := l.Allow(context.Background(), "k", tb3per3s); d.Allowed {
			t.Fatal("request allowed after the clock went backwards")
		}
	})

	t.Run("sliding window with a clock moving backwards keeps exact expiry", func(t *testing.T) {
		// An entry recorded at an earlier clock reading must expire at its own
		// time, not be held back by the newer entry (the Redis sorted set
		// orders by score; the Go log must keep the same order).
		clk := clock.NewFake(Epoch)
		l := newLimiter(t, clk)
		clk.Set(Epoch.Add(5 * time.Second))
		mustAllow(t, l, sw2per10s) // entry at 5s
		clk.Set(Epoch)
		mustAllow(t, l, sw2per10s) // entry at 0s
		d, _ := l.Allow(context.Background(), "k", sw2per10s)
		if d.Allowed || !near(d.RetryAfter, 10*time.Second) {
			t.Fatalf("full log at 0s: allowed=%v retryAfter=%v, want denied for 10s", d.Allowed, d.RetryAfter)
		}
		clk.Set(Epoch.Add(11 * time.Second)) // the 0s entry expired, the 5s one has not
		d, _ = l.Allow(context.Background(), "k", sw2per10s)
		if !d.Allowed || d.Remaining != 0 || !near(d.ResetAfter, 10*time.Second) {
			t.Fatalf("at 11s: allowed=%v remaining=%d resetAfter=%v, want allowed, 0, 10s",
				d.Allowed, d.Remaining, d.ResetAfter)
		}
	})

	t.Run("sliding window after a hot reload lowers the limit", func(t *testing.T) {
		// 5 entries retained at t=0..4s, then the limit drops from 5 to 2.
		clk := clock.NewFake(Epoch)
		l := newLimiter(t, clk)
		high := limiter.Rule{Algorithm: limiter.SlidingWindow, Limit: 5, Window: 10 * time.Second}
		low := limiter.Rule{Algorithm: limiter.SlidingWindow, Limit: 2, Window: 10 * time.Second}
		for i := range 5 {
			if i > 0 {
				clk.Advance(time.Second)
			}
			mustAllow(t, l, high)
		}
		clk.Advance(time.Second) // t=5s
		d, err := l.Allow(context.Background(), "k", low)
		if err != nil {
			t.Fatal(err)
		}
		// Entries at 0,1,2,3s must expire (the last at 13s) before fewer than 2 remain.
		// The old code said times[0]+window, i.e. 5s, and the client was denied again.
		if d.Allowed || d.Remaining != 0 || !near(d.RetryAfter, 8*time.Second) {
			t.Fatalf("got allowed=%v remaining=%d retryAfter=%v, want denied, remaining 0, retry after 8s",
				d.Allowed, d.Remaining, d.RetryAfter)
		}
		clk.Advance(8*time.Second - time.Millisecond)
		if d, _ := l.Allow(context.Background(), "k", low); d.Allowed {
			t.Fatal("allowed before Retry-After elapsed")
		}
		clk.Advance(time.Millisecond)
		mustAllow(t, l, low)
	})

	t.Run("algorithms keep separate state for the same key", func(t *testing.T) {
		clk := clock.NewFake(Epoch)
		l := newLimiter(t, clk)
		for range 3 {
			mustAllow(t, l, tb3per3s)
		}
		if d, _ := l.Allow(context.Background(), "k", sw2per10s); !d.Allowed {
			t.Fatal("sliding-window rule was affected by token-bucket state")
		}
	})

	for _, rule := range []limiter.Rule{
		{Algorithm: limiter.TokenBucket, Limit: 50, Window: time.Minute},
		{Algorithm: limiter.SlidingWindow, Limit: 50, Window: time.Minute},
	} {
		t.Run("concurrent callers never exceed the limit/"+string(rule.Algorithm), func(t *testing.T) {
			clk := clock.NewFake(Epoch) // frozen: no refill during the test
			l := newLimiter(t, clk)
			var allowed atomic.Int64
			var wg sync.WaitGroup
			for range 16 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for range 20 {
						d, err := l.Allow(context.Background(), "hot", rule)
						if err != nil {
							t.Error(err)
							return
						}
						if d.Allowed {
							allowed.Add(1)
						}
					}
				}()
			}
			wg.Wait()
			if got := allowed.Load(); got != rule.Limit {
				t.Fatalf("allowed %d of 320 concurrent requests, want exactly %d", got, rule.Limit)
			}
		})
	}
}

func mustAllow(t *testing.T, l limiter.Limiter, r limiter.Rule) {
	t.Helper()
	d, err := l.Allow(context.Background(), "k", r)
	if err != nil || !d.Allowed {
		t.Fatalf("expected allow, got %+v err=%v", d, err)
	}
}

func near(got, want time.Duration) bool {
	diff := got - want
	return diff >= -tolerance && diff <= tolerance
}
