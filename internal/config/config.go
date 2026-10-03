// Package config loads, validates and compiles the gateway configuration.
// The file may be YAML or JSON (JSON is valid YAML, so one parser handles both).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/srujanmalakpata/floodgate/internal/degrade"
	"github.com/srujanmalakpata/floodgate/internal/limiter"
)

// KeyBy selects what identifies a client for a route.
type KeyBy string

const (
	// KeyByIP always uses the client IP. It is the default.
	KeyByIP KeyBy = "ip"
	// KeyByAPIKey uses the API-key header and falls back to the client IP when it
	// is absent. The gateway does not check that a key is real, so a client can
	// send a new key on every request and get a fresh budget each time. Use it
	// only when an upstream or edge rejects unknown keys, or set Route.IPLimit
	// as a per-IP backstop.
	KeyByAPIKey KeyBy = "api_key"
)

// Config is the on-disk configuration.
type Config struct {
	// Listen is the proxy address; AdminListen serves /healthz, /readyz and /metrics.
	Listen      string `yaml:"listen"`
	AdminListen string `yaml:"admin_listen"`
	Upstream    string `yaml:"upstream"`

	Backend  Backend  `yaml:"backend"`
	Key      Key      `yaml:"key"`
	Routes   []Route  `yaml:"routes"`
	Default  *Route   `yaml:"default"`
	Reload   Reload   `yaml:"reload"`
	Shutdown Shutdown `yaml:"shutdown"`
	LogLevel string   `yaml:"log_level"`
}

// Backend selects where limiter state lives.
type Backend struct {
	Type          string         `yaml:"type"` // "memory" or "redis"
	Redis         Redis          `yaml:"redis"`
	FailurePolicy degrade.Policy `yaml:"failure_policy"`
	Breaker       Breaker        `yaml:"breaker"`
}

// Redis connection settings.
type Redis struct {
	Addr      string        `yaml:"addr"`
	DB        int           `yaml:"db"`
	KeyPrefix string        `yaml:"key_prefix"`
	Timeout   time.Duration `yaml:"timeout"`
	// PasswordEnv names an environment variable holding the password, so the
	// secret never lives in the config file or a ConfigMap.
	PasswordEnv string `yaml:"password_env"`
	// TLS enables TLS to Redis (e.g. ElastiCache with in-transit encryption).
	TLS bool `yaml:"tls"`
}

// Breaker settings for the Redis circuit breaker.
type Breaker struct {
	FailureThreshold int           `yaml:"failure_threshold"`
	Cooldown         time.Duration `yaml:"cooldown"`
}

// Key settings for identifying clients.
type Key struct {
	Header string `yaml:"header"`
	// TrustedProxyHops is how many reverse proxies (e.g. a load balancer) sit in
	// front of the gateway and append to X-Forwarded-For. 0 means use the TCP peer.
	TrustedProxyHops int `yaml:"trusted_proxy_hops"`
}

// Route is one rate-limited path prefix.
type Route struct {
	Name       string            `yaml:"name"`
	PathPrefix string            `yaml:"path_prefix"`
	Methods    []string          `yaml:"methods"`
	Algorithm  limiter.Algorithm `yaml:"algorithm"`
	Limit      int64             `yaml:"limit"`
	Window     time.Duration     `yaml:"window"`
	KeyBy      KeyBy             `yaml:"key_by"`
	// IPLimit (optional, key_by api_key only) caps requests per client IP per
	// Window across all API keys, with the same algorithm. It stops one client
	// from multiplying its budget by rotating made-up keys.
	IPLimit int64 `yaml:"ip_limit"`
}

// Rule returns the limiter rule for the route.
func (r Route) Rule() limiter.Rule {
	return limiter.Rule{Algorithm: r.Algorithm, Limit: r.Limit, Window: r.Window}
}

// IPRule returns the per-IP backstop rule and whether the route has one.
func (r Route) IPRule() (limiter.Rule, bool) {
	if r.KeyBy != KeyByAPIKey || r.IPLimit <= 0 {
		return limiter.Rule{}, false
	}
	return limiter.Rule{Algorithm: r.Algorithm, Limit: r.IPLimit, Window: r.Window}, true
}

// Reload settings. Interval is the file-poll period (0 disables polling; SIGHUP still works).
type Reload struct {
	Interval time.Duration `yaml:"interval"`
}

// Shutdown settings. DrainDelay keeps serving (while /readyz reports not ready)
// so load balancers stop sending traffic before the listener closes.
type Shutdown struct {
	DrainDelay time.Duration `yaml:"drain_delay"`
	Timeout    time.Duration `yaml:"timeout"`
}

// Load reads, parses, defaults and validates the file at path.
func Load(path string) (*Config, []byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	cfg, err := Parse(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, raw, nil
}

// Parse decodes YAML or JSON, rejects unknown fields, applies defaults and validates.
func Parse(raw []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	setDefault(&c.Listen, ":8080")
	setDefault(&c.AdminListen, ":9090")
	setDefault(&c.Backend.Type, "memory")
	setDefault(&c.Backend.Redis.KeyPrefix, "rlgw")
	setDefault(&c.Key.Header, "X-API-Key")
	setDefault(&c.LogLevel, "info")
	if c.Backend.FailurePolicy == "" {
		c.Backend.FailurePolicy = degrade.FailLocal
	}
	if c.Backend.Redis.Timeout == 0 {
		c.Backend.Redis.Timeout = 50 * time.Millisecond
	}
	if c.Backend.Breaker.FailureThreshold == 0 {
		c.Backend.Breaker.FailureThreshold = 5
	}
	if c.Backend.Breaker.Cooldown == 0 {
		c.Backend.Breaker.Cooldown = 5 * time.Second
	}
	if c.Shutdown.Timeout == 0 {
		c.Shutdown.Timeout = 15 * time.Second
	}
	for i := range c.Routes {
		c.Routes[i].applyDefaults()
	}
	if c.Default != nil {
		setDefault(&c.Default.Name, "default")
		setDefault(&c.Default.PathPrefix, "/") // Validate rejects any other value
		c.Default.applyDefaults()
	}
}

func (r *Route) applyDefaults() {
	if r.Algorithm == "" {
		r.Algorithm = limiter.TokenBucket
	}
	if r.KeyBy == "" {
		r.KeyBy = KeyByIP
	}
	for i, m := range r.Methods {
		r.Methods[i] = strings.ToUpper(m)
	}
}

func setDefault(s *string, v string) {
	if *s == "" {
		*s = v
	}
}

// Validate reports every problem it finds, joined into one error.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if u, err := url.Parse(c.Upstream); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		add("upstream: must be an absolute http(s) URL, got %q", c.Upstream)
	}
	switch c.Backend.Type {
	case "memory":
	case "redis":
		if c.Backend.Redis.Addr == "" {
			add("backend.redis.addr: required when backend.type is redis")
		}
	default:
		add("backend.type: want memory or redis, got %q", c.Backend.Type)
	}
	// Checked for every backend so a typo does not surface only when someone
	// later switches to redis.
	if !c.Backend.FailurePolicy.Valid() {
		add("backend.failure_policy: want open, closed or local, got %q", c.Backend.FailurePolicy)
	}
	// Zero means "use the default" (applied before Validate), so only
	// negative values reach these checks. A negative Redis timeout would fail
	// every call and hold the breaker open forever.
	positive := []struct {
		name string
		d    time.Duration
	}{
		{"backend.redis.timeout", c.Backend.Redis.Timeout},
		{"backend.breaker.cooldown", c.Backend.Breaker.Cooldown},
		{"shutdown.timeout", c.Shutdown.Timeout},
	}
	for _, p := range positive {
		if p.d <= 0 {
			add("%s: must be > 0, got %s", p.name, p.d)
		}
	}
	if c.Backend.Breaker.FailureThreshold < 1 {
		add("backend.breaker.failure_threshold: must be >= 1")
	}
	if c.Shutdown.DrainDelay < 0 {
		add("shutdown.drain_delay: must be >= 0")
	}
	if c.Key.TrustedProxyHops < 0 {
		add("key.trusted_proxy_hops: must be >= 0")
	}
	if c.Reload.Interval < 0 {
		add("reload.interval: must be >= 0")
	}
	if d := c.Default; d != nil {
		// The default route catches everything the routes do not; a narrower
		// default would silently be ignored, so reject it instead.
		if d.PathPrefix != "/" {
			add("default: path_prefix is not allowed (the default route covers every path)")
		}
		if len(d.Methods) > 0 {
			add("default: methods is not allowed (the default route covers every method)")
		}
	}

	seen := map[string]bool{}
	routes := c.Routes
	if c.Default != nil {
		routes = append(append([]Route(nil), routes...), *c.Default)
	}
	for i, r := range routes {
		where := fmt.Sprintf("routes[%d]", i)
		if r.Name != "" {
			where = fmt.Sprintf("route %q", r.Name)
		}
		switch {
		case r.Name == "":
			add("%s: name is required", where)
		case strings.Contains(r.Name, ":"):
			// ':' separates the parts of limiter keys (route:ipcap:ip:1.2.3.4);
			// a name containing it could collide with another route's keys.
			add("%s: name must not contain ':'", where)
		case seen[r.Name]:
			add("%s: duplicate name", where)
		}
		seen[r.Name] = true
		if !strings.HasPrefix(r.PathPrefix, "/") {
			add("%s: path_prefix must start with /", where)
		}
		if err := r.Rule().Validate(); err != nil {
			add("%s: %w", where, err)
		}
		if r.KeyBy != KeyByAPIKey && r.KeyBy != KeyByIP {
			add("%s: key_by: want api_key or ip, got %q", where, r.KeyBy)
		}
		switch {
		case r.IPLimit < 0:
			add("%s: ip_limit must be >= 0", where)
		case r.IPLimit > 0 && r.KeyBy != KeyByAPIKey:
			add("%s: ip_limit only applies to key_by api_key (key_by ip already limits per IP)", where)
		case r.IPLimit > 0:
			ipRule, _ := r.IPRule()
			if err := ipRule.Validate(); err != nil {
				add("%s: ip_limit: %w", where, err)
			}
		}
	}
	return errors.Join(errs...)
}

// MatchRoute returns the most specific route for method and path: the longest
// matching path_prefix whose methods (if any) include method. It falls back to
// Default and returns nil when nothing applies (the request is not limited).
//
// Prefixes match whole path segments: "/login" matches "/login" and
// "/login/x" but not "/login-help", and "/api/" also matches "/api".
// Matching is case-sensitive, like Go's ServeMux.
func (c *Config) MatchRoute(method, path string) *Route {
	var (
		best    *Route
		bestLen = -1
	)
	for i := range c.Routes {
		r := &c.Routes[i]
		p := strings.TrimSuffix(r.PathPrefix, "/")
		if !matchesPrefix(path, p) || !r.allowsMethod(method) {
			continue
		}
		if len(p) > bestLen {
			best, bestLen = r, len(p)
		}
	}
	if best == nil {
		return c.Default
	}
	return best
}

// matchesPrefix reports whether path is p or lies below it. p has no trailing
// slash ("" is the root and matches every path).
func matchesPrefix(path, p string) bool {
	return path == p || strings.HasPrefix(path, p+"/")
}

// allowsMethod reports whether the route applies to method. A route listing
// GET also covers HEAD, because servers answer HEAD with the GET handler.
func (r *Route) allowsMethod(method string) bool {
	if len(r.Methods) == 0 {
		return true
	}
	for _, m := range r.Methods {
		if m == method || (m == http.MethodGet && method == http.MethodHead) {
			return true
		}
	}
	return false
}

// StaticDiff lists settings that differ between c and next but only take
// effect after a restart (listeners, upstream, backend, shutdown).
func (c *Config) StaticDiff(next *Config) []string {
	var diff []string
	check := func(name string, changed bool) {
		if changed {
			diff = append(diff, name)
		}
	}
	check("listen", c.Listen != next.Listen)
	check("admin_listen", c.AdminListen != next.AdminListen)
	check("upstream", c.Upstream != next.Upstream)
	check("backend", c.Backend != next.Backend)
	check("shutdown", c.Shutdown != next.Shutdown)
	check("reload", c.Reload != next.Reload)
	check("log_level", c.LogLevel != next.LogLevel)
	sort.Strings(diff)
	return diff
}

// WithReloadable returns a copy of c with next's hot-reloadable settings
// (routes, default route and key extraction) applied.
func (c *Config) WithReloadable(next *Config) *Config {
	out := *c
	out.Routes = next.Routes
	out.Default = next.Default
	out.Key = next.Key
	return &out
}
