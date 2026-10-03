// Package memory is a single-process limiter backend: a sharded map where each
// shard has its own mutex, so unrelated keys rarely contend on the same lock.
package memory

import (
	"context"
	"hash/maphash"
	"sync"
	"time"

	"github.com/srujanmalakpata/floodgate/internal/clock"
	"github.com/srujanmalakpata/floodgate/internal/limiter"
)

// DefaultShards is the shard count used when New is given n <= 0.
const DefaultShards = 64

type entry struct {
	bucket  limiter.Bucket
	log     limiter.Log
	expires time.Time // after this the entry holds no information and can be evicted
}

type shard struct {
	mu      sync.Mutex
	entries map[string]*entry
}

// Store is an in-memory limiter.Limiter. It is safe for concurrent use.
type Store struct {
	clock  clock.Clock
	seed   maphash.Seed
	shards []shard
	mask   uint64
}

var _ limiter.Limiter = (*Store)(nil)

// New returns a Store with the given number of shards, rounded up to a power of two.
func New(c clock.Clock, shards int) *Store {
	if shards <= 0 {
		shards = DefaultShards
	}
	n := 1
	for n < shards {
		n <<= 1
	}
	s := &Store{clock: c, seed: maphash.MakeSeed(), shards: make([]shard, n), mask: uint64(n - 1)}
	for i := range s.shards {
		s.shards[i].entries = make(map[string]*entry)
	}
	return s
}

// Allow implements limiter.Limiter. It never returns an error.
func (s *Store) Allow(_ context.Context, key string, rule limiter.Rule) (limiter.Decision, error) {
	// The algorithm is part of the map key so switching a route's algorithm
	// during a hot reload starts from fresh state instead of misreading it.
	k := string(rule.Algorithm) + "|" + key
	sh := &s.shards[maphash.String(s.seed, k)&s.mask]
	now := s.clock.Now()

	sh.mu.Lock()
	defer sh.mu.Unlock()

	e, ok := sh.entries[k]
	if !ok {
		e = &entry{}
		sh.entries[k] = e
	}
	var d limiter.Decision
	switch rule.Algorithm {
	case limiter.SlidingWindow:
		d = e.log.Take(now, rule)
	default:
		d = e.bucket.Take(now, rule)
	}
	e.expires = now.Add(d.ResetAfter)
	return d, nil
}

// Sweep evicts entries whose state has fully reset and returns how many were removed.
// It locks one shard at a time so it never stalls the whole store.
func (s *Store) Sweep() int {
	now := s.clock.Now()
	removed := 0
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		for k, e := range sh.entries {
			if !now.Before(e.expires) {
				delete(sh.entries, k)
				removed++
			}
		}
		sh.mu.Unlock()
	}
	return removed
}

// Len returns the number of tracked keys (for tests and metrics).
func (s *Store) Len() int {
	n := 0
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		n += len(sh.entries)
		sh.mu.Unlock()
	}
	return n
}

// RunJanitor calls Sweep every interval until ctx is cancelled.
func (s *Store) RunJanitor(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Sweep()
		}
	}
}
