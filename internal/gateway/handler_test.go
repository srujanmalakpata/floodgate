package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/srujanmalakpata/floodgate/internal/clock"
	"github.com/srujanmalakpata/floodgate/internal/config"
	"github.com/srujanmalakpata/floodgate/internal/limiter"
	"github.com/srujanmalakpata/floodgate/internal/limiter/memory"
	"github.com/srujanmalakpata/floodgate/internal/metrics"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(`
upstream: http://upstream
routes:
  - {name: api, path_prefix: /api/, limit: 2, window: 10s, key_by: api_key}
  - {name: login, path_prefix: /login, methods: [POST], algorithm: sliding_window, limit: 1, window: 1m}
  - {name: keyed, path_prefix: /keyed/, limit: 2, window: 10s, key_by: api_key, ip_limit: 5}
  - {name: tight-ip, path_prefix: /tight/, limit: 10, window: 10s, key_by: api_key, ip_limit: 2}
`))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

type harness struct {
	h        *Handler
	clk      *clock.Fake
	m        *metrics.Metrics
	upstream int    // requests that reached next
	lastPath string // escaped path of the last forwarded request
}

func newHarness(t *testing.T, l func(clock.Clock) limiter.Limiter) *harness {
	t.Helper()
	cfg := testConfig(t)
	hs := &harness{clk: clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)), m: metrics.New(nil)}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hs.upstream++
		hs.lastPath = r.URL.EscapedPath()
		w.WriteHeader(http.StatusOK)
	})
	hs.h = NewHandler(func() *config.Config { return cfg }, l(hs.clk), "memory", next, hs.m, discard)
	return hs
}

func (hs *harness) do(method, path, apiKey, remote string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = remote
	if apiKey != "" {
		r.Header.Set("X-API-Key", apiKey)
	}
	w := httptest.NewRecorder()
	hs.h.ServeHTTP(w, r)
	return w
}

func memLimiter(c clock.Clock) limiter.Limiter { return memory.New(c, 4) }

func TestAllowedThenLimitedWithHeaders(t *testing.T) {
	hs := newHarness(t, memLimiter)

	want := []struct {
		status    int
		remaining string
	}{{200, "1"}, {200, "0"}, {429, "0"}}
	for i, w := range want {
		res := hs.do("GET", "/api/items", "k1", "192.0.2.1:1")
		if res.Code != w.status {
			t.Fatalf("request %d: status %d, want %d", i, res.Code, w.status)
		}
		h := res.Header()
		if h.Get("RateLimit-Limit") != "2" || h.Get("RateLimit-Remaining") != w.remaining || h.Get("RateLimit-Policy") != "2;w=10" {
			t.Fatalf("request %d: headers %v", i, h)
		}
	}

	res := hs.do("GET", "/api/items", "k1", "192.0.2.1:1")
	if got := res.Header().Get("Retry-After"); got != "5" {
		t.Fatalf("Retry-After = %q, want 5 (one token every 5s)", got)
	}
	if got := res.Header().Get("RateLimit-Reset"); got != "10" {
		t.Fatalf("RateLimit-Reset = %q, want 10", got)
	}
	var body errorBody
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil || body.RetryAfterSeconds != 5 {
		t.Fatalf("body = %+v, err %v", body, err)
	}
	if hs.upstream != 2 {
		t.Fatalf("upstream saw %d requests, want 2", hs.upstream)
	}

	// Another API key from the same IP has its own budget.
	if res := hs.do("GET", "/api/items", "k2", "192.0.2.1:1"); res.Code != 200 {
		t.Fatalf("second API key was limited: %d", res.Code)
	}
	// After 5s one token is back.
	hs.clk.Advance(5 * time.Second)
	if res := hs.do("GET", "/api/items", "k1", "192.0.2.1:1"); res.Code != 200 {
		t.Fatalf("not refilled after 5s: %d", res.Code)
	}

	if got := testutil.ToFloat64(hs.m.Requests.WithLabelValues("api", "limited")); got != 2 {
		t.Fatalf("limited counter = %v, want 2", got)
	}
	if got := testutil.ToFloat64(hs.m.Requests.WithLabelValues("api", "allowed")); got != 4 {
		t.Fatalf("allowed counter = %v, want 4", got)
	}
}

func TestRetryAfterIsAtLeastOneSecond(t *testing.T) {
	hs := newHarness(t, memLimiter)
	hs.do("POST", "/login", "", "192.0.2.1:1")
	hs.clk.Advance(time.Minute - 100*time.Millisecond)
	res := hs.do("POST", "/login", "", "192.0.2.1:1")
	if res.Code != 429 || res.Header().Get("Retry-After") != "1" {
		t.Fatalf("status %d Retry-After %q, want 429 and 1", res.Code, res.Header().Get("Retry-After"))
	}
}

func TestUnmatchedRouteIsNotLimited(t *testing.T) {
	hs := newHarness(t, memLimiter)
	for range 10 {
		if res := hs.do("GET", "/public", "", "192.0.2.1:1"); res.Code != 200 || res.Header().Get("RateLimit-Limit") != "" {
			t.Fatalf("unmatched route was limited: %d %v", res.Code, res.Header())
		}
	}
}

// A path that an upstream would normalise to /login must hit the /login limit
// and be forwarded in its canonical form.
func TestNonCanonicalPathsCannotBypassRoute(t *testing.T) {
	paths := []string{"//login", "/./login", "/x/../login", "/x/..%2Flogin", "/%2E%2E/login", "/login/."}
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			hs := newHarness(t, memLimiter)
			if res := hs.do("POST", "/login", "", "192.0.2.1:1"); res.Code != 200 {
				t.Fatalf("first /login: status %d", res.Code)
			}
			if res := hs.do("POST", p, "", "192.0.2.1:1"); res.Code != http.StatusTooManyRequests {
				t.Fatalf("POST %s: status %d, want 429 (limit of 1 already used by /login)", p, res.Code)
			}
			// A fresh client is admitted and the upstream sees the canonical path.
			if res := hs.do("POST", p, "", "192.0.2.2:1"); res.Code != 200 || hs.lastPath != "/login" {
				t.Fatalf("POST %s from new IP: status %d, upstream path %q, want 200 and /login", p, res.Code, hs.lastPath)
			}
		})
	}
}

func TestCleanPath(t *testing.T) {
	tests := map[string]string{
		"/": "/", "/api/": "/api/", "/api//x/": "/api/x/", "/a/./b": "/a/b", "/a/../../b": "/b",
		"//": "/", "/login/..": "/", "*": "*", "": "",
	}
	for in, want := range tests {
		if got := cleanPath(in); got != want {
			t.Errorf("cleanPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// key_by api_key cannot check that keys are real, so ip_limit caps one IP
// across all the keys it sends.
func TestRotatingAPIKeysCannotExceedIPLimit(t *testing.T) {
	hs := newHarness(t, memLimiter)
	allowed := 0
	for i := range 20 {
		if res := hs.do("GET", "/keyed/x", fmt.Sprintf("made-up-%d", i), "192.0.2.1:1"); res.Code == 200 {
			allowed++
		}
	}
	if allowed != 5 {
		t.Fatalf("one IP with 20 rotating keys got %d requests through, want ip_limit=5", allowed)
	}
	// Keys are still limited individually below the IP cap, and other IPs are unaffected.
	hs2 := newHarness(t, memLimiter)
	codes := []int{}
	for range 3 {
		codes = append(codes, hs2.do("GET", "/keyed/x", "real", "192.0.2.9:1").Code)
	}
	if codes[0] != 200 || codes[1] != 200 || codes[2] != 429 {
		t.Fatalf("per-key limit 2: got %v, want [200 200 429]", codes)
	}
	if res := hs.do("GET", "/keyed/x", "made-up-0", "192.0.2.2:1"); res.Code != 200 {
		t.Fatalf("a different IP was limited by another IP's cap: %d", res.Code)
	}
}

// When a route has two checks (per-IP cap and per-key limit), the headers on
// an allowed response describe the tighter one, so RateLimit-Remaining reaches
// 0 exactly when the next request would be denied.
func TestHeadersReportTightestOfTwoChecks(t *testing.T) {
	hs := newHarness(t, memLimiter)
	want := []struct {
		key, limit, remaining string
		status                int
	}{
		{"a", "2", "1", 200}, // ip cap 2 is tighter than the key limit 10
		{"b", "2", "0", 200},
		{"c", "2", "0", 429}, // denied by the ip cap
	}
	for i, w := range want {
		res := hs.do("GET", "/tight/x", w.key, "192.0.2.1:1")
		h := res.Header()
		if res.Code != w.status || h.Get("RateLimit-Limit") != w.limit || h.Get("RateLimit-Remaining") != w.remaining {
			t.Fatalf("request %d: status %d limit %q remaining %q, want %d %s %s",
				i, res.Code, h.Get("RateLimit-Limit"), h.Get("RateLimit-Remaining"), w.status, w.limit, w.remaining)
		}
		if res.Code == 200 && h.Get("RateLimit-Policy") != "2;w=10, 10;w=10" {
			t.Fatalf("request %d: RateLimit-Policy %q, want both policies", i, h.Get("RateLimit-Policy"))
		}
	}
}

// stubLimiter returns a fixed decision.
type stubLimiter struct {
	d   limiter.Decision
	err error
}

func (s stubLimiter) Allow(context.Context, string, limiter.Rule) (limiter.Decision, error) {
	return s.d, s.err
}

func TestFailClosedDegradedDenialIs503(t *testing.T) {
	hs := newHarness(t, func(clock.Clock) limiter.Limiter {
		return stubLimiter{d: limiter.Decision{Allowed: false, Limit: 2, RetryAfter: 3 * time.Second, Degraded: true, Unavailable: true}}
	})
	res := hs.do("GET", "/api/x", "", "192.0.2.1:1")
	if res.Code != http.StatusServiceUnavailable || res.Header().Get("Retry-After") != "3" {
		t.Fatalf("status %d Retry-After %q, want 503 and 3", res.Code, res.Header().Get("Retry-After"))
	}
}

func TestLimiterErrorIs503(t *testing.T) {
	hs := newHarness(t, func(clock.Clock) limiter.Limiter { return stubLimiter{err: context.Canceled} })
	if res := hs.do("GET", "/api/x", "", "192.0.2.1:1"); res.Code != http.StatusServiceUnavailable || hs.upstream != 0 {
		t.Fatalf("status %d upstream %d", res.Code, hs.upstream)
	}
}

func TestProxyForwardsAndAppendsXFF(t *testing.T) {
	var gotXFF, gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXFF, gotPath = r.Header.Get("X-Forwarded-For"), r.URL.Path
		_, _ = io.WriteString(w, "hello")
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	m := metrics.New(nil)
	p := NewProxy(u, m, discard)

	r := httptest.NewRequest("GET", "/api/thing", nil)
	r.RemoteAddr = "192.0.2.1:1234"
	r.Header.Set("X-Forwarded-For", "198.51.100.9")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Body.String() != "hello" || gotPath != "/api/thing" || gotXFF != "198.51.100.9, 192.0.2.1" {
		t.Fatalf("body %q path %q xff %q", w.Body.String(), gotPath, gotXFF)
	}

	up.Close() // upstream down -> 502 and a counted error
	w = httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusBadGateway || testutil.ToFloat64(m.UpstreamErrors) != 1 {
		t.Fatalf("status %d errors %v", w.Code, testutil.ToFloat64(m.UpstreamErrors))
	}
}

func BenchmarkHandlerMemory(b *testing.B) {
	cfg, _ := config.Parse([]byte("upstream: http://u\ndefault: {limit: 1000000000, window: 1s}\n"))
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h := NewHandler(func() *config.Config { return cfg }, memory.New(clock.Real{}, 64), "memory", next, metrics.New(nil), discard)
	b.RunParallel(func(pb *testing.PB) {
		r := httptest.NewRequest("GET", "/api/x", nil)
		r.RemoteAddr = "192.0.2.1:1"
		for pb.Next() {
			h.ServeHTTP(httptest.NewRecorder(), r)
		}
	})
}
