// Package breaker is a small consecutive-failure circuit breaker.
//
//	closed --(N consecutive failures)--> open --(cooldown elapsed)--> half-open
//	half-open --(probe succeeds)--> closed
//	half-open --(probe fails)-----> open
//
// While open, callers skip the dependency entirely, so a dead Redis costs
// nothing per request instead of a full network timeout.
package breaker

import (
	"sync"
	"time"

	"github.com/srujanmalakpata/floodgate/internal/clock"
)

// State is the breaker state.
type State int

const (
	Closed State = iota
	HalfOpen
	Open
)

func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case HalfOpen:
		return "half_open"
	case Open:
		return "open"
	}
	return "unknown"
}

// Ticket identifies one permitted call. Outcomes are reported with the ticket
// so that a slow call that started in an earlier state (generation) cannot
// change the breaker after it has moved on: a stale success must not close an
// open breaker, and a stale outcome must not free the half-open probe slot.
type Ticket struct {
	gen   uint64
	probe bool
}

// Breaker is safe for concurrent use.
type Breaker struct {
	mu        sync.Mutex
	clock     clock.Clock
	threshold int
	cooldown  time.Duration
	onChange  func(from, to State) // called with mu held; must not call back into the breaker

	state    State
	gen      uint64 // incremented on every state transition
	failures int
	openedAt time.Time
	probing  bool // a half-open probe is in flight
}

// New returns a closed breaker that opens after threshold consecutive failures
// and allows a single probe after cooldown. onChange may be nil.
func New(c clock.Clock, threshold int, cooldown time.Duration, onChange func(from, to State)) *Breaker {
	if threshold < 1 {
		threshold = 1
	}
	if onChange == nil {
		onChange = func(State, State) {}
	}
	return &Breaker{clock: c, threshold: threshold, cooldown: cooldown, onChange: onChange}
}

// Allow reports whether the caller may try the protected operation. Every
// true result must be followed by exactly one Success, Failure or Ignore call
// with the returned ticket.
func (b *Breaker) Allow() (Ticket, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case Closed:
		return Ticket{gen: b.gen}, true
	case Open:
		if b.clock.Now().Sub(b.openedAt) < b.cooldown {
			return Ticket{}, false
		}
		b.transition(HalfOpen)
		b.probing = true
		return Ticket{gen: b.gen, probe: true}, true
	default: // HalfOpen: only one probe at a time
		if b.probing {
			return Ticket{}, false
		}
		b.probing = true
		return Ticket{gen: b.gen, probe: true}, true
	}
}

// current reports whether t was issued in the breaker's current generation.
// Callers hold mu.
func (b *Breaker) current(t Ticket) bool { return t.gen == b.gen }

// Success records a successful call.
func (b *Breaker) Success(t Ticket) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.current(t) {
		return
	}
	b.failures = 0
	if t.probe {
		b.probing = false
		b.transition(Closed)
	}
}

// Failure records a failed call.
func (b *Breaker) Failure(t Ticket) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.current(t) {
		return
	}
	if t.probe {
		b.probing = false
		b.open()
		return
	}
	b.failures++
	if b.failures >= b.threshold {
		b.open()
	}
}

// Ignore ends a call whose outcome says nothing about the dependency's health
// (for example, the client cancelled). It frees the half-open probe slot if t
// was the probe.
func (b *Breaker) Ignore(t Ticket) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.current(t) && t.probe {
		b.probing = false
	}
}

// State returns the current state.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// RetryIn returns how long until an open breaker will allow a probe.
func (b *Breaker) RetryIn() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state != Open {
		return 0
	}
	return max(b.cooldown-b.clock.Now().Sub(b.openedAt), 0)
}

func (b *Breaker) open() {
	b.openedAt = b.clock.Now()
	b.failures = 0
	b.transition(Open)
}

func (b *Breaker) transition(to State) {
	from := b.state
	b.state = to
	b.gen++
	b.onChange(from, to)
}
