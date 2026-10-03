package limiter

import (
	"math"
	"slices"
	"time"
)

// Bucket is token-bucket state for one key. The zero value is an untouched key
// (it starts full on first use).
//
// Capacity is rule.Limit; the refill rate is rule.Limit tokens per rule.Window
// (100 per minute permits a burst of 100, then approximately 1.67 requests/s).
type Bucket struct {
	Tokens float64
	Last   time.Time
}

// Take refills the bucket for the time elapsed since the last call, then tries to
// consume one token. It mutates b and returns the decision.
func (b *Bucket) Take(now time.Time, r Rule) Decision {
	capacity := float64(r.Limit)
	ratePerNs := capacity / float64(r.Window) // tokens per nanosecond

	if b.Last.IsZero() {
		b.Tokens, b.Last = capacity, now
	}
	// Clamp so a clock that moves backwards never removes tokens or
	// rewinds the refill reference point.
	if elapsed := now.Sub(b.Last); elapsed > 0 {
		b.Tokens += float64(elapsed) * ratePerNs
		b.Last = now
	}
	// Also clamps after a hot reload that lowered the limit.
	b.Tokens = math.Min(capacity, b.Tokens)

	d := Decision{Limit: r.Limit}
	if b.Tokens >= 1 {
		b.Tokens--
		d.Allowed = true
	} else {
		d.RetryAfter = ceilDuration((1 - b.Tokens) / ratePerNs)
	}
	d.Remaining = int64(math.Floor(b.Tokens))
	d.ResetAfter = ceilDuration((capacity - b.Tokens) / ratePerNs)
	return d
}

// Log is sliding-window-log state for one key: the timestamps of accepted
// requests inside the trailing window, sorted oldest first.
type Log struct {
	times []time.Time
}

// Take drops timestamps that fell out of the window, then admits the request if
// fewer than rule.Limit remain. It mutates l and returns the decision.
func (l *Log) Take(now time.Time, r Rule) Decision {
	cutoff := now.Add(-r.Window)
	drop := 0
	for drop < len(l.times) && !l.times[drop].After(cutoff) {
		drop++
	}
	if drop > 0 {
		// Shift in place so the backing array is reused instead of growing forever.
		n := copy(l.times, l.times[drop:])
		l.times = l.times[:n]
	}

	d := Decision{Limit: r.Limit}
	if n := int64(len(l.times)); n < r.Limit {
		l.insert(now)
		d.Allowed = true
	} else {
		// n-limit+1 entries must expire before there is room again; the last of
		// them is times[n-limit]. That is times[0] unless a hot reload lowered the
		// limit below the number of entries already retained.
		d.RetryAfter = l.times[n-r.Limit].Add(r.Window).Sub(now)
	}
	d.Remaining = max(r.Limit-int64(len(l.times)), 0)
	if n := len(l.times); n > 0 {
		d.ResetAfter = l.times[n-1].Add(r.Window).Sub(now)
	}
	return d
}

// insert adds t keeping times sorted. Real time only moves forward (time.Now
// carries a monotonic reading), so this is an append; an injected clock that
// steps backwards inserts earlier, like the Redis sorted set does.
func (l *Log) insert(t time.Time) {
	i := len(l.times)
	for i > 0 && l.times[i-1].After(t) {
		i--
	}
	l.times = slices.Insert(l.times, i, t)
}

// Len reports how many timestamps are currently retained.
func (l *Log) Len() int { return len(l.times) }

func ceilDuration(ns float64) time.Duration {
	if ns <= 0 {
		return 0
	}
	return time.Duration(math.Ceil(ns))
}
