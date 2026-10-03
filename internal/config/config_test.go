package config

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srujanmalakpata/floodgate/internal/degrade"
	"github.com/srujanmalakpata/floodgate/internal/limiter"
)

const minimal = `
upstream: http://up:9000
routes:
  - name: api
    path_prefix: /api/
    limit: 10
    window: 1s
`

// Every config shipped in the repo must stay valid as the schema evolves.
func TestShippedConfigsAreValid(t *testing.T) {
	for _, path := range []string{
		"../../examples/config.yaml",
		"../../examples/compose-config.yaml",
		"../../deploy/k8s/base/config.yaml",
	} {
		t.Run(filepath.Base(filepath.Dir(path))+"/"+filepath.Base(path), func(t *testing.T) {
			cfg, _, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(cfg.Routes) == 0 || cfg.Default == nil {
				t.Fatalf("expected routes and a default route in %s", path)
			}
		})
	}
}

func TestDefaults(t *testing.T) {
	cfg, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.Routes[0]
	checks := []struct {
		name      string
		got, want any
	}{
		{"listen", cfg.Listen, ":8080"},
		{"admin_listen", cfg.AdminListen, ":9090"},
		{"backend", cfg.Backend.Type, "memory"},
		{"failure_policy", cfg.Backend.FailurePolicy, degrade.FailLocal},
		{"redis timeout", cfg.Backend.Redis.Timeout, 50 * time.Millisecond},
		{"key header", cfg.Key.Header, "X-API-Key"},
		{"algorithm", r.Algorithm, limiter.TokenBucket},
		{"key_by", r.KeyBy, KeyByIP},
		{"ip_limit", r.IPLimit, int64(0)},
		{"window", r.Window, time.Second},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestJSONIsAccepted(t *testing.T) {
	cfg, err := Parse([]byte(`{"upstream":"http://up","routes":[{"name":"a","path_prefix":"/","limit":1,"window":"1m","algorithm":"sliding_window"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Routes[0].Window != time.Minute || cfg.Routes[0].Algorithm != limiter.SlidingWindow {
		t.Fatalf("got %+v", cfg.Routes[0])
	}
}

func TestValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"unknown field", minimal + "colour: blue\n", "field colour not found"},
		{"missing upstream", "routes: []\n", "upstream"},
		{"relative upstream", "upstream: up:9000\n", "upstream"},
		{"bad backend", "upstream: http://u\nbackend: {type: etcd}\n", "backend.type"},
		{"redis without addr", "upstream: http://u\nbackend: {type: redis}\n", "backend.redis.addr"},
		{"bad policy", "upstream: http://u\nbackend: {type: redis, redis: {addr: r:6379}, failure_policy: maybe}\n", "failure_policy"},
		{"bad algorithm", "upstream: http://u\nroutes: [{name: a, path_prefix: /, limit: 1, window: 1s, algorithm: leaky}]\n", "unknown algorithm"},
		{"zero limit", "upstream: http://u\nroutes: [{name: a, path_prefix: /, window: 1s}]\n", "limit must be > 0"},
		{"no window", "upstream: http://u\nroutes: [{name: a, path_prefix: /, limit: 1}]\n", "window"},
		{"bad prefix", "upstream: http://u\nroutes: [{name: a, path_prefix: api, limit: 1, window: 1s}]\n", "must start with /"},
		{"duplicate names", "upstream: http://u\nroutes: [{name: a, path_prefix: /x, limit: 1, window: 1s}, {name: a, path_prefix: /y, limit: 1, window: 1s}]\n", "duplicate"},
		{"missing name", "upstream: http://u\nroutes: [{path_prefix: /x, limit: 1, window: 1s}]\n", "name is required"},
		{"bad key_by", "upstream: http://u\nroutes: [{name: a, path_prefix: /, limit: 1, window: 1s, key_by: cookie}]\n", "key_by"},
		{"negative ip_limit", "upstream: http://u\nroutes: [{name: a, path_prefix: /, limit: 1, window: 1s, key_by: api_key, ip_limit: -1}]\n", "ip_limit must be >= 0"},
		{"ip_limit with key_by ip", "upstream: http://u\nroutes: [{name: a, path_prefix: /, limit: 1, window: 1s, ip_limit: 5}]\n", "ip_limit only applies"},
		{"sliding ip_limit above cap", "upstream: http://u\nroutes: [{name: a, path_prefix: /, limit: 1, window: 1s, algorithm: sliding_window, key_by: api_key, ip_limit: 20000}]\n", "ip_limit: sliding_window limit must be <="},
		{"bad duration", "upstream: http://u\nroutes: [{name: a, path_prefix: /, limit: 1, window: soon}]\n", "parse"},
		{"colon in name", "upstream: http://u\nroutes: [{name: \"a:ipcap\", path_prefix: /b/, limit: 1, window: 1s}]\n", "must not contain ':'"},
		{"methods on default", "upstream: http://u\ndefault: {limit: 1, window: 1h, methods: [POST]}\n", "default: methods is not allowed"},
		{"path_prefix on default", "upstream: http://u\ndefault: {limit: 1, window: 1h, path_prefix: /api/}\n", "default: path_prefix is not allowed"},
		{"bad policy with memory backend", "upstream: http://u\nbackend: {failure_policy: maybe}\n", "failure_policy"},
		{"negative redis timeout", "upstream: http://u\nbackend: {redis: {timeout: -1s}}\n", "backend.redis.timeout: must be > 0"},
		{"negative breaker cooldown", "upstream: http://u\nbackend: {breaker: {cooldown: -5s}}\n", "backend.breaker.cooldown: must be > 0"},
		{"negative failure threshold", "upstream: http://u\nbackend: {breaker: {failure_threshold: -1}}\n", "failure_threshold: must be >= 1"},
		{"negative shutdown timeout", "upstream: http://u\nshutdown: {timeout: -1s}\n", "shutdown.timeout: must be > 0"},
		{"negative drain delay", "upstream: http://u\nshutdown: {drain_delay: -2s}\n", "shutdown.drain_delay: must be >= 0"},
		{"token bucket limit too large", "upstream: http://u\nroutes: [{name: a, path_prefix: /, limit: 9223372036854775807, window: 1s}]\n", "token_bucket limit must be <="},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestMatchRoute(t *testing.T) {
	cfg, err := Parse([]byte(`
upstream: http://u
routes:
  - {name: api, path_prefix: /api/, limit: 1, window: 1s}
  - {name: api-v2, path_prefix: /api/v2/, limit: 1, window: 1s}
  - {name: login, path_prefix: /login, methods: [post], limit: 1, window: 1s}
  - {name: docs, path_prefix: /docs, methods: [GET], limit: 1, window: 1s}
default: {limit: 1, window: 1s}
`))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct{ method, path, want string }{
		{"GET", "/api/users", "api"},
		{"GET", "/api/v2/users", "api-v2"}, // longest prefix wins
		{"POST", "/login", "login"},
		{"GET", "/login", "default"}, // method not listed
		{"GET", "/", "default"},
		{"GET", "/api", "api"},             // "/api/" covers the bare "/api" too
		{"GET", "/apix", "default"},        // prefixes match whole segments
		{"POST", "/login/", "login"},       // below the prefix
		{"POST", "/login-help", "default"}, // not a segment of /login
		{"HEAD", "/docs/a", "docs"},        // GET covers HEAD
		{"POST", "/docs/a", "default"},     // but not other methods
		{"GET", "/API/users", "default"},   // case-sensitive, like ServeMux
	}
	for _, tt := range tests {
		if got := cfg.MatchRoute(tt.method, tt.path); got == nil || got.Name != tt.want {
			t.Errorf("MatchRoute(%s %s) = %v, want %s", tt.method, tt.path, got, tt.want)
		}
	}

	cfg.Default = nil
	if got := cfg.MatchRoute("GET", "/other"); got != nil {
		t.Errorf("without a default, unmatched paths should be unlimited, got %v", got.Name)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReloader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, minimal)
	var results []bool
	r, err := NewReloader(path, slog.New(slog.NewTextHandler(io.Discard, nil)), func(ok bool) { results = append(results, ok) })
	if err != nil {
		t.Fatal(err)
	}

	// A valid change to a route limit is applied; a static change is ignored.
	writeFile(t, path, strings.Replace(minimal, "limit: 10", "limit: 99", 1)+"listen: \":1234\"\n")
	if applied, err := r.Reload(); !applied || err != nil {
		t.Fatalf("Reload() = %v, %v", applied, err)
	}
	if got := r.Current().Routes[0].Limit; got != 99 {
		t.Fatalf("limit after reload = %d, want 99", got)
	}
	if got := r.Current().Listen; got != ":8080" {
		t.Fatalf("listen changed to %q by a hot reload", got)
	}

	// An invalid file keeps the previous config.
	writeFile(t, path, "upstream: [broken")
	if applied, err := r.Reload(); applied || err == nil {
		t.Fatalf("Reload() of a broken file = %v, %v", applied, err)
	}
	if got := r.Current().Routes[0].Limit; got != 99 {
		t.Fatalf("broken reload replaced the config (limit %d)", got)
	}
	if len(results) != 2 || !results[0] || results[1] {
		t.Fatalf("reload results = %v, want [true false]", results)
	}
}

func TestWatchPicksUpFileChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, minimal)
	r, err := NewReloader(path, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	go r.Watch(ctx, 10*time.Millisecond)

	writeFile(t, path, strings.Replace(minimal, "limit: 10", "limit: 7", 1))
	deadline := time.Now().Add(2 * time.Second)
	for r.Current().Routes[0].Limit != 7 {
		if time.Now().After(deadline) {
			t.Fatal("watcher did not apply the changed file within 2s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
