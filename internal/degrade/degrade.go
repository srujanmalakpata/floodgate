// Package degrade wraps a shared (remote) limiter with a timeout, a circuit
// breaker and an explicit policy for what to do when the backend is unavailable.
package degrade

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/srujanmalakpata/floodgate/internal/breaker"
	"github.com/srujanmalakpata/floodgate/internal/limiter"
)

// Policy decides requests while the primary backend is failing.
type Policy string

const (
	// FailOpen admits every request: availability over protection.
	FailOpen Policy = "open"
	// FailClosed rejects every request: protection over availability.
	FailClosed Policy = "closed"
	// FailLocal enforces the same rules with a per-replica in-memory limiter.
	// With N replicas the effective global limit becomes up to N x limit.
	FailLocal Policy = "local"
)

// Valid reports whether p is a known policy.
func (p Policy) Valid() bool { return p == FailOpen || p == FailClosed || p == FailLocal }

// Hooks observe degraded behaviour (metrics, logs). Any field may be nil.
type Hooks struct {
	BackendError func(err error)
	Degraded     func(p Policy)
}

// Limiter is a limiter.Limiter that never returns an error: backend failures
// are converted into decisions according to its Policy.
type Limiter struct {
	primary limiter.Limiter
	local   limiter.Limiter
	breaker *breaker.Breaker
	policy  Policy
	timeout time.Duration
	hooks   Hooks
}

var _ limiter.Limiter = (*Limiter)(nil)

// New wraps primary. local is required only for FailLocal.
func New(primary, local limiter.Limiter, b *breaker.Breaker, p Policy, timeout time.Duration, h Hooks) (*Limiter, error) {
	if !p.Valid() {
		return nil, fmt.Errorf("unknown failure policy %q", p)
	}
	if p == FailLocal && local == nil {
		return nil, errors.New("failure policy \"local\" needs a local limiter")
	}
	return &Limiter{primary: primary, local: local, breaker: b, policy: p, timeout: timeout, hooks: h}, nil
}

// Allow asks the primary backend (bounded by the timeout) unless the breaker
// is open, and falls back to the policy on any error.
func (l *Limiter) Allow(ctx context.Context, key string, rule limiter.Rule) (limiter.Decision, error) {
	ticket, ok := l.breaker.Allow()
	if !ok {
		return l.fallback(ctx, key, rule)
	}
	callCtx, cancel := context.WithTimeout(ctx, l.timeout)
	d, err := l.primary.Allow(callCtx, key, rule)
	cancel()
	if err == nil {
		l.breaker.Success(ticket)
		return d, nil
	}
	if ctx.Err() != nil {
		// The client went away; that says nothing about backend health.
		l.breaker.Ignore(ticket)
		return limiter.Decision{}, ctx.Err()
	}
	l.breaker.Failure(ticket)
	if l.hooks.BackendError != nil {
		l.hooks.BackendError(err)
	}
	return l.fallback(ctx, key, rule)
}

func (l *Limiter) fallback(ctx context.Context, key string, rule limiter.Rule) (limiter.Decision, error) {
	if l.hooks.Degraded != nil {
		l.hooks.Degraded(l.policy)
	}
	switch l.policy {
	case FailLocal:
		d, err := l.local.Allow(ctx, key, rule)
		d.Degraded = true
		return d, err
	case FailClosed:
		return limiter.Decision{
			Allowed:     false,
			Limit:       rule.Limit,
			RetryAfter:  max(l.breaker.RetryIn(), time.Second),
			Degraded:    true,
			Unavailable: true,
		}, nil
	default: // FailOpen
		return limiter.Decision{Allowed: true, Limit: rule.Limit, Remaining: rule.Limit, Degraded: true}, nil
	}
}
