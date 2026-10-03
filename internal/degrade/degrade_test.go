package degrade

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/srujanmalakpata/floodgate/internal/breaker"
	"github.com/srujanmalakpata/floodgate/internal/clock"
	"github.com/srujanmalakpata/floodgate/internal/limiter"
	"github.com/srujanmalakpata/floodgate/internal/limiter/memory"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// fakeBackend fails while down is true and counts calls.
type fakeBackend struct {
	down  bool
	calls int
}

func (f *fakeBackend) Allow(_ context.Context, _ string, r limiter.Rule) (limiter.Decision, error) {
	f.calls++
	if f.down {
		return limiter.Decision{}, errors.New("connection refused")
	}
	return limiter.Decision{Allowed: true, Limit: r.Limit, Remaining: r.Limit - 1}, nil
}

var rule = limiter.Rule{Algorithm: limiter.TokenBucket, Limit: 2, Window: time.Minute}

func TestPolicies(t *testing.T) {
	tests := []struct {
		policy      Policy
		wantAllowed []bool // three requests while the backend is down
	}{
		{FailOpen, []bool{true, true, true}},
		{FailClosed, []bool{false, false, false}},
		{FailLocal, []bool{true, true, false}}, // local limiter enforces limit=2
	}
	for _, tt := range tests {
		t.Run(string(tt.policy), func(t *testing.T) {
			clk := clock.NewFake(t0)
			backend := &fakeBackend{down: true}
			var errs, degraded int
			l, err := New(backend, memory.New(clk, 1), breaker.New(clk, 5, time.Second, nil), tt.policy, 10*time.Millisecond,
				Hooks{BackendError: func(error) { errs++ }, Degraded: func(Policy) { degraded++ }})
			if err != nil {
				t.Fatal(err)
			}
			for i, want := range tt.wantAllowed {
				d, err := l.Allow(context.Background(), "k", rule)
				if err != nil {
					t.Fatalf("request %d: unexpected error %v", i, err)
				}
				if d.Allowed != want || !d.Degraded {
					t.Fatalf("request %d: got allowed=%v degraded=%v, want allowed=%v degraded=true", i, d.Allowed, d.Degraded, want)
				}
				if d.Unavailable != (tt.policy == FailClosed) {
					t.Fatalf("request %d: Unavailable = %v; only fail-closed decisions are unavailable", i, d.Unavailable)
				}
				if tt.policy == FailClosed && d.RetryAfter < time.Second {
					t.Fatalf("fail-closed RetryAfter = %v, want >= 1s", d.RetryAfter)
				}
			}
			if errs != 3 || degraded != 3 {
				t.Fatalf("hooks saw %d errors and %d degraded decisions, want 3 and 3", errs, degraded)
			}
		})
	}
}

func TestBreakerStopsCallingDeadBackendAndRecovers(t *testing.T) {
	clk := clock.NewFake(t0)
	backend := &fakeBackend{down: true}
	l, _ := New(backend, nil, breaker.New(clk, 3, 5*time.Second, nil), FailOpen, time.Millisecond, Hooks{})
	ctx := context.Background()

	for range 10 {
		_, _ = l.Allow(ctx, "k", rule)
	}
	if backend.calls != 3 {
		t.Fatalf("backend called %d times, want 3 (breaker should open after 3 failures)", backend.calls)
	}

	backend.down = false
	clk.Advance(5 * time.Second)
	d, _ := l.Allow(ctx, "k", rule)
	if d.Degraded || backend.calls != 4 {
		t.Fatalf("after cooldown the probe should reach the healthy backend: %+v calls=%d", d, backend.calls)
	}
}

func TestHealthyBackendPassesThrough(t *testing.T) {
	clk := clock.NewFake(t0)
	l, _ := New(&fakeBackend{}, nil, breaker.New(clk, 1, time.Second, nil), FailClosed, time.Second, Hooks{})
	d, err := l.Allow(context.Background(), "k", rule)
	if err != nil || !d.Allowed || d.Degraded || d.Remaining != 1 {
		t.Fatalf("got %+v, %v", d, err)
	}
}

func TestCancelledClientIsNotABackendFailure(t *testing.T) {
	clk := clock.NewFake(t0)
	b := breaker.New(clk, 1, time.Second, nil)
	l, _ := New(&fakeBackend{down: true}, nil, b, FailOpen, time.Second, Hooks{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.Allow(ctx, "k", rule); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if b.State() != breaker.Closed {
		t.Fatalf("breaker = %v, want closed", b.State())
	}
}

func TestNewValidates(t *testing.T) {
	b := breaker.New(clock.Real{}, 1, time.Second, nil)
	if _, err := New(&fakeBackend{}, nil, b, "sometimes", time.Second, Hooks{}); err == nil {
		t.Error("expected error for unknown policy")
	}
	if _, err := New(&fakeBackend{}, nil, b, FailLocal, time.Second, Hooks{}); err == nil {
		t.Error("expected error for local policy without a local limiter")
	}
}
