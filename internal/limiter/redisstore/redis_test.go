package redisstore

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/srujanmalakpata/floodgate/internal/clock"
	"github.com/srujanmalakpata/floodgate/internal/limiter"
	"github.com/srujanmalakpata/floodgate/internal/limiter/limitertest"
)

// newRedis starts an in-process miniredis (which runs real Lua via gopher-lua).
func newRedis(t testing.TB) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	return mr, c
}

func TestConformance(t *testing.T) {
	limitertest.Run(t, func(t *testing.T, clk clock.Clock) limiter.Limiter {
		_, c := newRedis(t)
		return New(c, clk, "test")
	})
}

// TestConformanceRealRedis runs the same suite against a real Redis server
// (CI starts one as a service container). miniredis executes Lua with
// gopher-lua, so this is what proves the scripts behave the same on Redis itself.
func TestConformanceRealRedis(t *testing.T) {
	addr := os.Getenv("RLGW_TEST_REDIS")
	if addr == "" {
		t.Skip("set RLGW_TEST_REDIS=host:port to run against a real Redis")
	}
	c := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("RLGW_TEST_REDIS=%s is not reachable: %v", addr, err)
	}
	run := strconv.FormatInt(time.Now().UnixNano(), 36)
	n := 0
	limitertest.Run(t, func(_ *testing.T, clk clock.Clock) limiter.Limiter {
		n++ // a fresh key prefix per scenario keeps them independent
		return New(c, clk, "conformance-"+run+"-"+strconv.Itoa(n))
	})
}

// Two gateway replicas pointing at one Redis must enforce one shared limit.
func TestReplicasShareOneLimit(t *testing.T) {
	for _, alg := range []limiter.Algorithm{limiter.TokenBucket, limiter.SlidingWindow} {
		t.Run(string(alg), func(t *testing.T) {
			_, c := newRedis(t)
			clk := clock.NewFake(limitertest.Epoch)
			replicas := []*Store{New(c, clk, "rlgw"), New(c, clk, "rlgw")}
			rule := limiter.Rule{Algorithm: alg, Limit: 10, Window: time.Minute}

			allowed := 0
			for i := range 30 {
				d, err := replicas[i%2].Allow(context.Background(), "client", rule)
				if err != nil {
					t.Fatal(err)
				}
				if d.Allowed {
					allowed++
				}
			}
			if allowed != 10 {
				t.Fatalf("2 replicas allowed %d requests in total, want 10", allowed)
			}
		})
	}
}

func TestKeysExpireOnceStateIsReset(t *testing.T) {
	mr, c := newRedis(t)
	s := New(c, clock.NewFake(limitertest.Epoch), "rlgw")
	ctx := context.Background()
	_, _ = s.Allow(ctx, "a", limiter.Rule{Algorithm: limiter.TokenBucket, Limit: 10, Window: 10 * time.Second})
	_, _ = s.Allow(ctx, "b", limiter.Rule{Algorithm: limiter.SlidingWindow, Limit: 10, Window: 10 * time.Second})

	for _, k := range []string{"rlgw:token_bucket:a", "rlgw:sliding_window:b"} {
		if ttl := mr.TTL(k); ttl <= 0 || ttl > 12*time.Second {
			t.Errorf("TTL(%s) = %v, want a positive TTL around the reset time", k, ttl)
		}
	}
	mr.FastForward(20 * time.Second)
	if keys := mr.Keys(); len(keys) != 0 {
		t.Fatalf("keys left after TTL: %v", keys)
	}
}

func TestBackendDownReturnsError(t *testing.T) {
	mr, c := newRedis(t)
	s := New(c, clock.Real{}, "rlgw")
	mr.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := s.Allow(ctx, "k", limiter.Rule{Algorithm: limiter.TokenBucket, Limit: 1, Window: time.Second}); err == nil {
		t.Fatal("expected an error when Redis is unreachable")
	}
}

// BenchmarkAllow measures one EVALSHA round trip. Against miniredis this is an
// in-process TCP loop, so it mostly measures client + Lua overhead. Set
// RLGW_BENCH_REDIS=host:port to benchmark against a real Redis instead.
func BenchmarkAllow(b *testing.B) {
	var c *redis.Client
	if addr := os.Getenv("RLGW_BENCH_REDIS"); addr != "" {
		c = redis.NewClient(&redis.Options{Addr: addr})
		b.Cleanup(func() { _ = c.Close() })
	} else {
		_, c = newRedis(b)
	}
	for _, alg := range []limiter.Algorithm{limiter.TokenBucket, limiter.SlidingWindow} {
		b.Run(string(alg), func(b *testing.B) {
			s := New(c, clock.Real{}, "bench-"+strconv.FormatInt(time.Now().UnixNano(), 36))
			r := limiter.Rule{Algorithm: alg, Limit: 100, Window: time.Second}
			ctx := context.Background()
			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					if _, err := s.Allow(ctx, "client-"+strconv.Itoa(i%1000), r); err != nil {
						b.Error(err)
						return
					}
					i++
				}
			})
		})
	}
}
