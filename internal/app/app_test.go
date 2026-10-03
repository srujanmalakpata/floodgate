package app

// End-to-end tests: real listeners on 127.0.0.1, a real upstream (httptest),
// and an in-process Redis (miniredis) shared by several gateway replicas.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/srujanmalakpata/floodgate/internal/limiter"
)

type running struct {
	app      *App
	proxyURL string
	adminURL string
	cfgPath  string
	stop     context.CancelFunc
	done     chan error
}

func startGateway(t *testing.T, cfgYAML string) *running {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := New(Options{ConfigPath: path, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	proxyLn := listen(t)
	adminLn := listen(t)
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{
		app: a, cfgPath: path, stop: cancel, done: make(chan error, 1),
		proxyURL: "http://" + proxyLn.Addr().String(),
		adminURL: "http://" + adminLn.Addr().String(),
	}
	go func() { r.done <- a.Serve(ctx, proxyLn, adminLn) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
			t.Error("gateway did not stop within 5s")
		}
	})
	return r
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot bind 127.0.0.1 in this environment: %v", err)
	}
	return ln
}

func get(t *testing.T, url, apiKey string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	return res, string(body)
}

func upstream(t *testing.T, delay time.Duration) (*httptest.Server, *atomic.Int64) {
	var hits atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		time.Sleep(delay)
		_, _ = io.WriteString(w, "upstream ok")
	}))
	t.Cleanup(s.Close)
	return s, &hits
}

func redisConfig(upstreamURL, redisAddr, extra string) string {
	return fmt.Sprintf(`
upstream: %s
backend:
  type: redis
  redis: {addr: %q, timeout: 2s}   # generous: a starved CI host must not time out and fall back
  failure_policy: local
  breaker: {failure_threshold: 2, cooldown: 1m}
routes:
  - {name: api, path_prefix: /api/, algorithm: sliding_window, limit: 5, window: 1m, key_by: api_key}
%s`, upstreamURL, redisAddr, extra)
}

func TestTwoReplicasShareLimitsThroughRedis(t *testing.T) {
	up, hits := upstream(t, 0)
	mr := miniredis.RunT(t)
	cfg := redisConfig(up.URL, mr.Addr(), "")
	gateways := []*running{startGateway(t, cfg), startGateway(t, cfg)}

	codes := map[int]int{}
	for i := range 12 {
		res, body := get(t, gateways[i%2].proxyURL+"/api/orders", "client-a")
		codes[res.StatusCode]++
		if res.StatusCode == http.StatusOK && body != "upstream ok" {
			t.Fatalf("unexpected upstream body %q", body)
		}
		if res.StatusCode == http.StatusTooManyRequests && res.Header.Get("Retry-After") == "" {
			t.Fatal("429 without Retry-After")
		}
	}
	// A Redis error would silently switch a replica to its local limiter and
	// change the counts, so report backend errors first and explicitly.
	for i, g := range gateways {
		if _, m := get(t, g.adminURL+"/metrics", ""); !strings.Contains(m, "rlgw_backend_errors_total 0") {
			t.Errorf("replica %d had Redis errors (decisions fell back to local limits):\n%s", i, grepLines(m, "rlgw_backend_errors_total", "rlgw_breaker_state"))
		}
	}
	if codes[200] != 5 || codes[429] != 7 || hits.Load() != 5 {
		t.Fatalf("status counts %v, upstream hits %d; want 5x200 and 7x429 shared across replicas", codes, hits.Load())
	}

	// A different client still has its full quota.
	if res, _ := get(t, gateways[1].proxyURL+"/api/orders", "client-b"); res.StatusCode != 200 {
		t.Fatalf("client-b got %d", res.StatusCode)
	}

	// Each replica exports its own share of the decisions.
	_, m0 := get(t, gateways[0].adminURL+"/metrics", "")
	_, m1 := get(t, gateways[1].adminURL+"/metrics", "")
	if !strings.Contains(m0, `rlgw_requests_total{decision="allowed",route="api"}`) ||
		!strings.Contains(m1, `rlgw_requests_total{decision="limited",route="api"}`) {
		t.Fatalf("metrics missing request counters:\n%s", m0)
	}
}

func TestRedisOutageFallsBackToLocalLimits(t *testing.T) {
	up, _ := upstream(t, 0)
	mr := miniredis.RunT(t)
	g := startGateway(t, redisConfig(up.URL, mr.Addr(), ""))

	if res, _ := get(t, g.proxyURL+"/api/x", "k"); res.StatusCode != 200 {
		t.Fatalf("healthy request got %d", res.StatusCode)
	}
	mr.Close()

	// Local policy: the replica keeps enforcing limit=5 on its own.
	codes := map[int]int{}
	for range 8 {
		res, _ := get(t, g.proxyURL+"/api/x", "k")
		codes[res.StatusCode]++
	}
	if codes[200] != 5 || codes[429] != 3 {
		t.Fatalf("during the outage got %v, want 5x200 3x429 from the local fallback", codes)
	}

	res, body := get(t, g.adminURL+"/readyz", "")
	if res.StatusCode != 200 || !strings.Contains(body, "redis_breaker: open") {
		t.Fatalf("readyz = %d %q; want ready with an open breaker", res.StatusCode, body)
	}
	_, metrics := get(t, g.adminURL+"/metrics", "")
	for _, want := range []string{"rlgw_backend_errors_total 2", `rlgw_degraded_decisions_total{policy="local"} 8`, "rlgw_breaker_state 2"} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

func TestHotReloadChangesAndRemovesLimits(t *testing.T) {
	up, _ := upstream(t, 0)
	routeCfg := func(limit int) string {
		return fmt.Sprintf("upstream: %s\nroutes:\n  - {name: api, path_prefix: /api/, limit: %d, window: 1h}\n", up.URL, limit)
	}
	g := startGateway(t, routeCfg(5))
	reload := func(cfg string) {
		t.Helper()
		if err := os.WriteFile(g.cfgPath, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := g.app.Reload(); err != nil { // the SIGHUP path
			t.Fatal(err)
		}
	}
	statuses := func(n int) []int {
		var out []int
		for range n {
			res, _ := get(t, g.proxyURL+"/api/x", "k")
			out = append(out, res.StatusCode)
		}
		return out
	}

	// One request spends 1 of 5 tokens; 4 remain in the existing bucket.
	if got := statuses(1); got[0] != 200 {
		t.Fatalf("first request got %d", got[0])
	}
	// Lowering the limit to 2 clamps the existing state: 2 more, not 4.
	reload(routeCfg(2))
	if got := fmt.Sprint(statuses(3)); got != "[200 200 429]" {
		t.Fatalf("after lowering the limit to 2: %s, want [200 200 429]", got)
	}
	// Raising it to 10 grants no instant tokens; refill continues at 10/h.
	reload(routeCfg(10))
	if got := statuses(1); got[0] != 429 {
		t.Fatalf("raising the limit refilled the bucket immediately: %d", got[0])
	}
	// Removing the route makes /api/ unlimited.
	reload(fmt.Sprintf("upstream: %s\n", up.URL))
	if got := statuses(1); got[0] != 200 {
		t.Fatalf("after removing the route got %d, want 200", got[0])
	}
	_, metrics := get(t, g.adminURL+"/metrics", "")
	if !strings.Contains(metrics, `rlgw_config_reloads_total{result="success"} 3`) {
		t.Error("reloads not counted in metrics")
	}
}

// With Redis down, failure_policy decides: closed rejects with 503 (a server
// condition, not the client's quota), open admits everything.
func TestRedisOutageFailClosedAndOpen(t *testing.T) {
	for _, tc := range []struct {
		policy string
		status int
	}{{"closed", http.StatusServiceUnavailable}, {"open", http.StatusOK}} {
		t.Run(tc.policy, func(t *testing.T) {
			up, hits := upstream(t, 0)
			mr := miniredis.RunT(t)
			cfg := strings.Replace(redisConfig(up.URL, mr.Addr(), ""), "failure_policy: local", "failure_policy: "+tc.policy, 1)
			g := startGateway(t, cfg)
			if res, _ := get(t, g.proxyURL+"/api/x", "k"); res.StatusCode != 200 {
				t.Fatalf("healthy request got %d", res.StatusCode)
			}
			mr.Close()

			for i := range 8 { // more than the limit of 5
				res, body := get(t, g.proxyURL+"/api/x", "k")
				if res.StatusCode != tc.status {
					t.Fatalf("request %d during the outage: %d, want %d", i, res.StatusCode, tc.status)
				}
				if tc.policy == "closed" {
					var eb struct {
						Error             string `json:"error"`
						RetryAfterSeconds int64  `json:"retry_after_seconds"`
					}
					if err := json.Unmarshal([]byte(body), &eb); err != nil || eb.Error != "rate limiter unavailable" ||
						eb.RetryAfterSeconds < 1 || res.Header.Get("Retry-After") == "" {
						t.Fatalf("503 body %q Retry-After %q", body, res.Header.Get("Retry-After"))
					}
				}
			}
			wantHits := int64(1)
			if tc.policy == "open" {
				wantHits = 9
			}
			if hits.Load() != wantHits {
				t.Fatalf("upstream hits %d, want %d", hits.Load(), wantHits)
			}
			_, metrics := get(t, g.adminURL+"/metrics", "")
			if want := fmt.Sprintf(`rlgw_degraded_decisions_total{policy=%q} 8`, tc.policy); !strings.Contains(metrics, want) {
				t.Errorf("metrics missing %q", want)
			}
		})
	}
}

func TestGracefulShutdownDrainsInFlightRequests(t *testing.T) {
	up, _ := upstream(t, 300*time.Millisecond)
	g := startGateway(t, fmt.Sprintf("upstream: %s\nshutdown: {drain_delay: 200ms, timeout: 5s}\n", up.URL))

	if res, body := get(t, g.adminURL+"/readyz", ""); res.StatusCode != 200 || !strings.HasPrefix(body, "ready") {
		t.Fatalf("readyz before shutdown = %d %q", res.StatusCode, body)
	}

	inflight := make(chan int, 1)
	go func() {
		res, err := http.Get(g.proxyURL + "/slow")
		if err != nil {
			inflight <- -1
			return
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		inflight <- res.StatusCode
	}()
	time.Sleep(50 * time.Millisecond) // let the request reach the upstream
	g.stop()

	// During the drain delay readiness fails but the listener still works.
	deadline := time.Now().Add(time.Second)
	for {
		res, _ := get(t, g.adminURL+"/readyz", "")
		if res.StatusCode == http.StatusServiceUnavailable {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("readyz never reported draining")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if code := <-inflight; code != 200 {
		t.Fatalf("in-flight request finished with %d, want 200", code)
	}
	select {
	case err := <-g.done:
		if err != nil {
			t.Fatalf("Serve returned %v", err)
		}
		g.done <- nil // let Cleanup observe completion too
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after shutdown")
	}
	if _, err := http.Get(g.proxyURL + "/after"); err == nil {
		t.Fatal("proxy still accepting connections after shutdown")
	}
}

func TestHealthzAndBadConfig(t *testing.T) {
	up, _ := upstream(t, 0)
	g := startGateway(t, "upstream: "+up.URL+"\n")
	if res, body := get(t, g.adminURL+"/healthz", ""); res.StatusCode != 200 || body != "ok\n" {
		t.Fatalf("healthz = %d %q", res.StatusCode, body)
	}

	path := filepath.Join(t.TempDir(), "bad.yaml")
	_ = os.WriteFile(path, []byte("upstream: nope\n"), 0o600)
	if _, err := New(Options{ConfigPath: path}); err == nil {
		t.Fatal("New accepted an invalid config")
	}
}

// grepLines returns the lines of s that start with any of prefixes.
func grepLines(s string, prefixes ...string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		for _, p := range prefixes {
			if strings.HasPrefix(line, p) {
				out = append(out, line)
			}
		}
	}
	return strings.Join(out, "\n")
}

func TestAccessLogRejectionsAreDebug(t *testing.T) {
	var buf strings.Builder
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	status := http.StatusOK
	h := accessLog(log, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
	for _, status = range []int{200, 429, 429, 503, 404, 502} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
	}
	got := strings.Count(buf.String(), `"msg":"request"`)
	if got != 3 || strings.Contains(buf.String(), `"status":429`) || strings.Contains(buf.String(), `"status":503`) {
		t.Fatalf("info access log has %d lines, want 3 (200, 404, 502) and no 429/503:\n%s", got, buf.String())
	}
}

// blackhole accepts TCP connections and never answers, like a hung Redis.
func blackhole(t *testing.T) string {
	t.Helper()
	ln := listen(t)
	var conns []net.Conn
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns = append(conns, c)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return ln.Addr().String()
}

// go-redis only applies a context deadline to socket I/O when
// ContextTimeoutEnabled is set; without it this call would block for the full
// 2s read timeout although the caller's deadline is 50ms. (Plain cancellation
// without a deadline still does not interrupt a read in go-redis v9.22, so a
// client disconnect waits for the per-call budget; see DESIGN.md.)
func TestRedisCallHonoursContextDeadline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := fmt.Sprintf("upstream: http://u\nbackend: {type: redis, redis: {addr: %q, timeout: 2s}}\n", blackhole(t))
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := New(Options{ConfigPath: path, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.redis.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	// The result is a context error or, if the socket deadline fires a moment
	// before the context's timer, a local-policy fallback; either way it is fast.
	_, _ = a.limiter.Allow(ctx, "k", limiter.Rule{Algorithm: limiter.TokenBucket, Limit: 1, Window: time.Second})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Allow took %v; want it bounded by the 50ms context deadline, not the 2s Redis timeout", elapsed)
	}
}
