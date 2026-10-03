package breaker

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srujanmalakpata/floodgate/internal/clock"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestStateMachine(t *testing.T) {
	clk := clock.NewFake(t0)
	var transitions []string
	b := New(clk, 3, 10*time.Second, func(from, to State) {
		transitions = append(transitions, from.String()+"->"+to.String())
	})

	// Two failures are not enough; a success resets the count.
	for range 2 {
		b.Failure(mustAllow(t, b))
	}
	b.Success(mustAllow(t, b))
	for range 2 {
		b.Failure(mustAllow(t, b))
	}
	if b.State() != Closed {
		t.Fatalf("state = %v after non-consecutive failures, want closed", b.State())
	}

	// Third consecutive failure opens it.
	b.Failure(mustAllow(t, b))
	if _, ok := b.Allow(); b.State() != Open || ok {
		t.Fatal("breaker should be open and reject calls")
	}
	if got := b.RetryIn(); got != 10*time.Second {
		t.Fatalf("RetryIn = %v, want 10s", got)
	}

	// After the cooldown exactly one probe goes through.
	clk.Advance(10 * time.Second)
	probe := mustAllow(t, b)
	if _, ok := b.Allow(); b.State() != HalfOpen || ok {
		t.Fatal("half-open breaker must allow only one concurrent probe")
	}
	b.Failure(probe) // probe fails -> open again
	if b.State() != Open {
		t.Fatalf("state = %v after failed probe, want open", b.State())
	}

	clk.Advance(10 * time.Second)
	b.Success(mustAllow(t, b)) // probe succeeds -> closed
	if b.State() != Closed {
		t.Fatalf("state = %v after successful probe, want closed", b.State())
	}

	want := []string{"closed->open", "open->half_open", "half_open->open", "open->half_open", "half_open->closed"}
	if len(transitions) != len(want) {
		t.Fatalf("transitions = %v, want %v", transitions, want)
	}
	for i := range want {
		if transitions[i] != want[i] {
			t.Fatalf("transitions = %v, want %v", transitions, want)
		}
	}
}

func TestIgnoreFreesProbeWithoutClosing(t *testing.T) {
	clk := clock.NewFake(t0)
	b := New(clk, 1, time.Second, nil)
	b.Failure(mustAllow(t, b))
	clk.Advance(time.Second)
	b.Ignore(mustAllow(t, b))
	if b.State() != HalfOpen {
		t.Fatalf("state = %v, want half_open", b.State())
	}
	mustAllow(t, b) // the probe slot is free again
}

// Outcomes of calls that started before a state change must not affect the
// new state: they would otherwise close an open breaker without a probe or
// let a second half-open probe through.
func TestStaleOutcomesAreIgnored(t *testing.T) {
	tests := []struct {
		name    string
		outcome func(*Breaker, Ticket)
	}{
		{"success", (*Breaker).Success},
		{"failure", (*Breaker).Failure},
		{"ignore", (*Breaker).Ignore},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clk := clock.NewFake(t0)
			b := New(clk, 2, time.Second, nil)
			slow := mustAllow(t, b) // starts while closed, finishes late
			b.Failure(mustAllow(t, b))
			b.Failure(mustAllow(t, b)) // opens

			tt.outcome(b, slow) // stale outcome while open
			if b.State() != Open {
				t.Fatalf("stale %s changed open breaker to %v", tt.name, b.State())
			}

			clk.Advance(time.Second)
			mustAllow(t, b)     // the probe (left in flight)
			tt.outcome(b, slow) // stale outcome while the probe is in flight
			if b.State() != HalfOpen {
				t.Fatalf("stale %s changed half-open breaker to %v", tt.name, b.State())
			}
			if _, ok := b.Allow(); ok {
				t.Fatalf("stale %s freed the probe slot: a second concurrent probe was allowed", tt.name)
			}
		})
	}
}

func mustAllow(t *testing.T, b *Breaker) Ticket {
	t.Helper()
	tk, ok := b.Allow()
	if !ok {
		t.Fatalf("Allow() = false in state %v", b.State())
	}
	return tk
}

// Many goroutines share one breaker while the clock moves. Run with -race.
// Invariants: at most one half-open probe is in flight at any moment, and
// every transition is one the state machine allows.
func TestConcurrentUseKeepsInvariants(t *testing.T) {
	clk := clock.NewFake(t0)
	var transitions []string // appended by onChange, which runs with b.mu held
	b := New(clk, 3, 10*time.Millisecond, func(from, to State) {
		transitions = append(transitions, from.String()+"->"+to.String())
	})

	var probing atomic.Bool
	var overlaps atomic.Int64
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 2000 {
				ticket, ok := b.Allow()
				if !ok {
					clk.Advance(time.Millisecond)
					continue
				}
				if ticket.probe {
					if !probing.CompareAndSwap(false, true) {
						overlaps.Add(1) // another probe is still in flight
					}
					runtime.Gosched()    // widen the window in which a second probe would be caught
					probing.Store(false) // before the outcome, which may admit the next probe
				}
				switch (g + i) % 5 {
				case 0, 1:
					b.Failure(ticket)
				case 2:
					b.Ignore(ticket)
				default:
					b.Success(ticket)
				}
			}
		}()
	}
	wg.Wait()

	if n := overlaps.Load(); n > 0 {
		t.Fatalf("a half-open probe was issued while another was in flight (%d times)", n)
	}
	valid := map[string]bool{"closed->open": true, "open->half_open": true, "half_open->closed": true, "half_open->open": true}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(transitions) == 0 {
		t.Fatal("the breaker never changed state; the test exercised nothing")
	}
	for _, tr := range transitions {
		if !valid[tr] {
			t.Fatalf("invalid transition %s in %v", tr, transitions)
		}
	}
	t.Logf("%d transitions", len(transitions))
}
