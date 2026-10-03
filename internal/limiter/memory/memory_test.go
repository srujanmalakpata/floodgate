package memory

import (
	"context"
	"math/rand/v2"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srujanmalakpata/floodgate/internal/clock"
	"github.com/srujanmalakpata/floodgate/internal/limiter"
	"github.com/srujanmalakpata/floodgate/internal/limiter/limitertest"
)

func TestConformance(t *testing.T) {
	limitertest.Run(t, func(_ *testing.T, clk clock.Clock) limiter.Limiter {
		return New(clk, 8)
	})
}

func TestNewRoundsShardsToPowerOfTwo(t *testing.T) {
	for _, tt := range []struct{ in, want int }{{0, DefaultShards}, {1, 1}, {3, 4}, {64, 64}, {65, 128}} {
		if got := len(New(clock.Real{}, tt.in).shards); got != tt.want {
			t.Errorf("New(%d) has %d shards, want %d", tt.in, got, tt.want)
		}
	}
}

func TestSweepEvictsOnlyResetEntries(t *testing.T) {
	clk := clock.NewFake(limitertest.Epoch)
	s := New(clk, 4)
	ctx := context.Background()
	short := limiter.Rule{Algorithm: limiter.TokenBucket, Limit: 10, Window: time.Second}
	long := limiter.Rule{Algorithm: limiter.SlidingWindow, Limit: 10, Window: time.Hour}
	for i := range 100 {
		_, _ = s.Allow(ctx, "short-"+strconv.Itoa(i), short)
	}
	_, _ = s.Allow(ctx, "long", long)

	if n := s.Sweep(); n != 0 {
		t.Fatalf("Sweep evicted %d live entries", n)
	}
	clk.Advance(2 * time.Second)
	if n := s.Sweep(); n != 100 {
		t.Fatalf("Sweep evicted %d entries, want 100", n)
	}
	if s.Len() != 1 {
		t.Fatalf("Len = %d, want 1 (the sliding-window entry is still inside its hour)", s.Len())
	}
}

func BenchmarkAllowSingleKey(b *testing.B) {
	s := New(clock.Real{}, DefaultShards)
	r := limiter.Rule{Algorithm: limiter.TokenBucket, Limit: 1 << 40, Window: time.Second}
	ctx := context.Background()
	for b.Loop() {
		_, _ = s.Allow(ctx, "k", r)
	}
}

// BenchmarkAllowParallel spreads 10k keys over all shards from GOMAXPROCS goroutines.
func BenchmarkAllowParallel(b *testing.B) {
	for _, alg := range []limiter.Algorithm{limiter.TokenBucket, limiter.SlidingWindow} {
		b.Run(string(alg), func(b *testing.B) { benchParallel(b, DefaultShards, alg) })
	}
}

// BenchmarkAllowParallelOneShard is the contention baseline: same work, one lock.
func BenchmarkAllowParallelOneShard(b *testing.B) {
	benchParallel(b, 1, limiter.TokenBucket)
}

// benchParallel calls Allow from GOMAXPROCS goroutines. Each goroutine draws
// keys from its own random stream, so goroutines do not walk the same key
// sequence in lockstep (which would measure same-key contention instead).
func benchParallel(b *testing.B, shards int, alg limiter.Algorithm) {
	s := New(clock.Real{}, shards)
	r := limiter.Rule{Algorithm: alg, Limit: 100, Window: time.Second}
	keys := make([]string, 10_000)
	for i := range keys {
		keys[i] = "client-" + strconv.Itoa(i)
	}
	ctx := context.Background()
	var seed atomic.Uint64
	b.RunParallel(func(pb *testing.PB) {
		rng := rand.New(rand.NewPCG(seed.Add(1), 0x9e3779b97f4a7c15))
		for pb.Next() {
			_, _ = s.Allow(ctx, keys[rng.IntN(len(keys))], r)
		}
	})
}
