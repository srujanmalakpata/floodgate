// Package gateway is the HTTP layer: it matches a route, asks the limiter, sets
// the standard rate-limit headers and either rejects or forwards the request.
package gateway

import (
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/srujanmalakpata/floodgate/internal/config"
	"github.com/srujanmalakpata/floodgate/internal/limiter"
	"github.com/srujanmalakpata/floodgate/internal/metrics"
)

// Handler is the rate-limiting front of the reverse proxy.
type Handler struct {
	cfg     func() *config.Config // live config (hot-reloadable)
	limiter limiter.Limiter
	backend string // metric label: "memory" or "redis"
	next    http.Handler
	metrics *metrics.Metrics
	log     *slog.Logger
}

// NewHandler returns a Handler that forwards admitted requests to next.
func NewHandler(cfg func() *config.Config, l limiter.Limiter, backend string, next http.Handler, m *metrics.Metrics, log *slog.Logger) *Handler {
	return &Handler{cfg: cfg, limiter: l, backend: backend, next: next, metrics: m, log: log}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Match and forward the same canonical path. Otherwise "//login" or
	// "/x/../login" would miss the /login limit while an upstream that
	// normalises paths still serves them as /login.
	r = withCleanPath(r)
	cfg := h.cfg()
	route := cfg.MatchRoute(r.Method, r.URL.Path)
	if route == nil {
		h.metrics.Requests.WithLabelValues("", "unlimited").Inc()
		h.next.ServeHTTP(w, r)
		return
	}

	// Each check must admit the request. The per-IP backstop runs first so a
	// client rotating made-up API keys is stopped before it creates new
	// per-key state. It counts attempts, not admissions: a request the per-key
	// rule then denies has still spent an IP-cap token (see DESIGN.md).
	id := ClientKey(r, route, cfg.Key)
	checks := make([]check, 0, 2)
	if ipRule, ok := route.IPRule(); ok && strings.HasPrefix(id, "key:") {
		checks = append(checks, check{route.Name + ":ipcap:ip:" + ClientIP(r, cfg.Key.TrustedProxyHops), ipRule})
	}
	checks = append(checks, check{route.Name + ":" + id, route.Rule()})

	results := make([]result, 0, len(checks))
	for _, c := range checks {
		start := time.Now()
		d, err := h.limiter.Allow(r.Context(), c.key, c.rule)
		h.metrics.LimiterDuration.WithLabelValues(h.backend).Observe(time.Since(start).Seconds())
		if err != nil {
			// Only reachable when the client disconnected mid-decision.
			h.metrics.Requests.WithLabelValues(route.Name, "error").Inc()
			h.log.Debug("limiter error", "route", route.Name, "err", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		results = append(results, result{c.rule, d})
		if !d.Allowed {
			break
		}
	}

	d := setRateLimitHeaders(w.Header(), results)
	switch {
	case d.Allowed:
		h.metrics.Requests.WithLabelValues(route.Name, "allowed").Inc()
		h.next.ServeHTTP(w, r)
	case d.Unavailable:
		// Fail-closed: we could not check the shared limit, so this is a
		// temporary server-side condition, not the client exceeding its quota.
		h.metrics.Requests.WithLabelValues(route.Name, "unavailable").Inc()
		writeError(w, http.StatusServiceUnavailable, "rate limiter unavailable", d.RetryAfter)
	default:
		h.metrics.Requests.WithLabelValues(route.Name, "limited").Inc()
		// Debug, not Info: under a flood, one log line per rejection is its own DoS.
		h.log.Debug("rate limited", "route", route.Name, "method", r.Method, "path", r.URL.Path,
			"retry_after", d.RetryAfter)
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded", d.RetryAfter)
	}
}

// check is one limiter call a request must pass.
type check struct {
	key  string
	rule limiter.Rule
}

// result is the decision one check returned.
type result struct {
	rule     limiter.Rule
	decision limiter.Decision
}

// cleanPath returns the canonical form of an absolute URL path: repeated
// slashes, "." and ".." segments are resolved, and a trailing slash is kept
// (the same rules as net/http.ServeMux).
func cleanPath(p string) string {
	if p == "" || p[0] != '/' {
		return p
	}
	c := path.Clean(p)
	if c != "/" && strings.HasSuffix(p, "/") {
		c += "/"
	}
	return c
}

// withCleanPath returns r, or a shallow copy whose URL carries the canonical
// path. RawPath is dropped so the proxy forwards exactly the path that was
// matched; this also resolves encoded forms such as "/x/..%2Flogin".
func withCleanPath(r *http.Request) *http.Request {
	c := cleanPath(r.URL.Path)
	if c == r.URL.Path {
		return r
	}
	r2 := r.WithContext(r.Context())
	u := *r.URL
	u.Path, u.RawPath = c, ""
	r2.URL = &u
	return r2
}

// setRateLimitHeaders writes the IETF httpapi draft headers
// (RateLimit-Limit / -Remaining / -Reset, RateLimit-Policy) for the checks
// that ran and returns the decision the response is based on.
//
// When every check allowed the request, the headers describe the most
// restrictive one (fewest remaining), so a client pacing itself by
// RateLimit-Remaining sees 0 exactly when its next request would be denied.
// The reset is the latest of all checks, because the client only has its full
// quota back once every limit has reset. When a check denied the request, it
// is the last result and the headers describe it. RateLimit-Policy lists every
// policy that applied.
func setRateLimitHeaders(h http.Header, results []result) limiter.Decision {
	last := results[len(results)-1]
	tightest := last
	var reset time.Duration
	policies := make([]string, 0, len(results))
	for _, res := range results {
		if res.decision.Allowed && res.decision.Remaining < tightest.decision.Remaining {
			tightest = res
		}
		reset = max(reset, res.decision.ResetAfter)
		policies = append(policies, strconv.FormatInt(res.rule.Limit, 10)+";w="+strconv.FormatInt(ceilSeconds(res.rule.Window), 10))
	}
	if !last.decision.Allowed {
		tightest = last // the denying check decides
		reset = last.decision.ResetAfter
	}
	d := tightest.decision
	h.Set("RateLimit-Limit", strconv.FormatInt(d.Limit, 10))
	h.Set("RateLimit-Remaining", strconv.FormatInt(d.Remaining, 10))
	h.Set("RateLimit-Reset", strconv.FormatInt(ceilSeconds(reset), 10))
	h.Set("RateLimit-Policy", strings.Join(policies, ", "))
	return d
}

type errorBody struct {
	Error             string `json:"error"`
	RetryAfterSeconds int64  `json:"retry_after_seconds"`
}

func writeError(w http.ResponseWriter, status int, msg string, retry time.Duration) {
	// Retry-After is whole seconds; round up and never send 0, which would invite an immediate retry.
	secs := max(ceilSeconds(retry), 1)
	w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: msg, RetryAfterSeconds: secs})
}

func ceilSeconds(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return int64(math.Ceil(d.Seconds()))
}

// NewProxy returns a reverse proxy to upstream that preserves the inbound
// X-Forwarded-For chain and counts upstream failures.
func NewProxy(upstream *url.URL, m *metrics.Metrics, log *slog.Logger) *httputil.ReverseProxy {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 100
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.Header["X-Forwarded-For"] = pr.In.Header["X-Forwarded-For"]
			pr.SetURL(upstream)
			pr.SetXForwarded()
		},
		Transport: transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return // client went away; nothing to report
			}
			m.UpstreamErrors.Inc()
			log.Warn("upstream error", "method", r.Method, "path", r.URL.Path, "err", err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}
}
