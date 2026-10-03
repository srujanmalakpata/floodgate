package limiter

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestRuleValidate(t *testing.T) {
	tests := []struct {
		name    string
		rule    Rule
		wantErr bool
	}{
		{"token bucket ok", Rule{TokenBucket, 10, time.Second}, false},
		{"sliding window ok", Rule{SlidingWindow, 1, time.Millisecond}, false},
		{"unknown algorithm", Rule{"leaky", 10, time.Second}, true},
		{"zero limit", Rule{TokenBucket, 0, time.Second}, true},
		{"negative limit", Rule{TokenBucket, -1, time.Second}, true},
		{"sub-millisecond window", Rule{TokenBucket, 1, time.Microsecond}, true},
		{"sliding window at the cap", Rule{SlidingWindow, MaxSlidingWindowLimit, time.Minute}, false},
		{"sliding window above the cap", Rule{SlidingWindow, MaxSlidingWindowLimit + 1, time.Minute}, true},
		{"token bucket has no cap", Rule{TokenBucket, 1_000_000, time.Minute}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.rule.Validate(); (err != nil) != tt.wantErr {
				t.Fatalf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestBucketFractionalRefill(t *testing.T) {
	// 100 per minute = one token every 600ms.
	r := Rule{TokenBucket, 100, time.Minute}
	var b Bucket
	for i := range 100 {
		if d := b.Take(t0, r); !d.Allowed {
			t.Fatalf("request %d denied during initial burst", i)
		}
	}
	d := b.Take(t0.Add(599*time.Millisecond), r)
	if d.Allowed {
		t.Fatal("allowed before a full token had refilled")
	}
	if d.RetryAfter <= 0 || d.RetryAfter > 2*time.Millisecond {
		t.Fatalf("RetryAfter = %v, want ~1ms", d.RetryAfter)
	}
	if d := b.Take(t0.Add(600*time.Millisecond), r); !d.Allowed {
		t.Fatalf("denied after a full token refilled: %+v", d)
	}
}

func TestBucketClampsAfterLimitLowered(t *testing.T) {
	var b Bucket
	b.Take(t0, Rule{TokenBucket, 100, time.Minute}) // 99 tokens left
	d := b.Take(t0, Rule{TokenBucket, 5, time.Minute})
	if d.Remaining != 4 {
		t.Fatalf("Remaining = %d after lowering limit to 5, want 4", d.Remaining)
	}
}

func TestLogReusesBackingArray(t *testing.T) {
	r := Rule{SlidingWindow, 4, time.Second}
	var l Log
	now := t0
	for range 1000 {
		l.Take(now, r)
		now = now.Add(300 * time.Millisecond)
	}
	if l.Len() > int(r.Limit) || cap(l.times) > 8 {
		t.Fatalf("log grew unbounded: len=%d cap=%d", l.Len(), cap(l.times))
	}
}

func BenchmarkBucketTake(b *testing.B) {
	r := Rule{TokenBucket, 1_000_000, time.Second}
	var bk Bucket
	now := t0
	for b.Loop() {
		now = now.Add(time.Microsecond)
		bk.Take(now, r)
	}
}

func BenchmarkLogTake(b *testing.B) {
	r := Rule{SlidingWindow, 100, time.Second}
	var l Log
	now := t0
	for b.Loop() {
		now = now.Add(10 * time.Millisecond)
		l.Take(now, r)
	}
}
