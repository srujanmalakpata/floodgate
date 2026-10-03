// Package app wires configuration, limiter backends, the proxy and the admin
// endpoints together and owns the process lifecycle (reload, graceful shutdown).
package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/srujanmalakpata/floodgate/internal/breaker"
	"github.com/srujanmalakpata/floodgate/internal/clock"
	"github.com/srujanmalakpata/floodgate/internal/config"
	"github.com/srujanmalakpata/floodgate/internal/degrade"
	"github.com/srujanmalakpata/floodgate/internal/gateway"
	"github.com/srujanmalakpata/floodgate/internal/limiter"
	"github.com/srujanmalakpata/floodgate/internal/limiter/memory"
	"github.com/srujanmalakpata/floodgate/internal/limiter/redisstore"
	"github.com/srujanmalakpata/floodgate/internal/metrics"
)

// Options configures New.
type Options struct {
	ConfigPath string
	Logger     *slog.Logger        // defaults to slog.Default()
	LogLevel   *slog.LevelVar      // if set, adjusted to the config's log_level
	Clock      clock.Clock         // defaults to the real clock
	Getenv     func(string) string // defaults to os.Getenv
	Version    string              // build version, reported in the startup log
}

// App is one gateway process.
type App struct {
	log      *slog.Logger
	reloader *config.Reloader
	metrics  *metrics.Metrics
	limiter  limiter.Limiter
	backend  string
	mem      *memory.Store // in-memory store (primary or local fallback)
	redis    *redis.Client // nil for the memory backend
	breaker  *breaker.Breaker
	proxy    http.Handler
	ready    atomic.Bool
	version  string
}

// New loads the configuration and builds every component. It does not listen.
func New(opts Options) (*App, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	a := &App{log: opts.Logger, version: opts.Version}

	var reloadResult func(ok bool) // set once metrics exist
	rl, err := config.NewReloader(opts.ConfigPath, opts.Logger, func(ok bool) { reloadResult(ok) })
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	a.reloader = rl
	cfg := rl.Current()
	if opts.LogLevel != nil {
		var lvl slog.Level
		if err := lvl.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
			return nil, fmt.Errorf("log_level: %w", err)
		}
		opts.LogLevel.Set(lvl)
	}

	a.mem = memory.New(opts.Clock, memory.DefaultShards)
	a.metrics = metrics.New(func() float64 { return float64(a.mem.Len()) })
	reloadResult = func(ok bool) {
		result := "success"
		if !ok {
			result = "failure"
		}
		a.metrics.ConfigReloads.WithLabelValues(result).Inc()
	}

	switch cfg.Backend.Type {
	case "redis":
		a.backend = "redis"
		redis.SetLogger(redisLogger{a.log})
		rc := cfg.Backend.Redis
		var tlsCfg *tls.Config
		if rc.TLS {
			tlsCfg = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		a.redis = redis.NewClient(&redis.Options{
			TLSConfig:    tlsCfg,
			Addr:         rc.Addr,
			DB:           rc.DB,
			Password:     passwordFrom(rc.PasswordEnv, opts.Getenv),
			DialTimeout:  rc.Timeout * 4,
			ReadTimeout:  rc.Timeout,
			WriteTimeout: rc.Timeout,
			// Without this go-redis ignores context deadlines for socket I/O and
			// the per-call budget in degrade.Limiter would not bound dial plus
			// write plus read. (A cancellation without a deadline, such as a
			// client disconnect, still does not interrupt a read in go-redis.)
			ContextTimeoutEnabled: true,
			MaxRetries:            -1, // the breaker and failure policy handle failures; retries only add latency
		})
		bc := cfg.Backend.Breaker
		a.breaker = breaker.New(opts.Clock, bc.FailureThreshold, bc.Cooldown, func(from, to breaker.State) {
			a.metrics.BreakerState.Set(float64(to))
			a.log.Warn("redis circuit breaker state change", "from", from.String(), "to", to.String())
		})
		guarded, err := degrade.New(
			redisstore.New(a.redis, opts.Clock, rc.KeyPrefix), a.mem, a.breaker,
			cfg.Backend.FailurePolicy, rc.Timeout,
			degrade.Hooks{
				BackendError: func(error) { a.metrics.BackendErrors.Inc() },
				Degraded:     func(p degrade.Policy) { a.metrics.Degraded.WithLabelValues(string(p)).Inc() },
			})
		if err != nil {
			return nil, err
		}
		a.limiter = guarded
	default:
		a.backend = "memory"
		a.limiter = a.mem
	}

	upstream, err := url.Parse(cfg.Upstream)
	if err != nil {
		return nil, fmt.Errorf("upstream: %w", err)
	}
	a.proxy = accessLog(a.log, gateway.NewHandler(rl.Current, a.limiter, a.backend,
		gateway.NewProxy(upstream, a.metrics, a.log), a.metrics, a.log))
	a.ready.Store(true)
	return a, nil
}

func passwordFrom(env string, getenv func(string) string) string {
	if env == "" {
		return ""
	}
	return getenv(env)
}

// Config returns the live configuration.
func (a *App) Config() *config.Config { return a.reloader.Current() }

// Reload re-reads the configuration file (SIGHUP).
func (a *App) Reload() error {
	_, err := a.reloader.Reload()
	return err
}

// ProxyHandler is the public, rate-limited handler.
func (a *App) ProxyHandler() http.Handler { return a.proxy }

// AdminHandler serves /healthz, /readyz and /metrics on the admin port.
func (a *App) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	// Liveness: the process is up and serving HTTP. Deliberately does not
	// check Redis, otherwise a Redis outage would restart every replica.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	// Readiness: false while draining. Redis being down does not make the
	// gateway unready either, because the failure policy still answers requests;
	// the breaker state is reported for humans.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		var b strings.Builder
		status := http.StatusOK
		if a.ready.Load() {
			b.WriteString("ready\n")
		} else {
			status = http.StatusServiceUnavailable
			b.WriteString("draining\n")
		}
		fmt.Fprintf(&b, "backend: %s\n", a.backend)
		if a.breaker != nil {
			fmt.Fprintf(&b, "redis_breaker: %s\n", a.breaker.State())
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(b.String()))
	})
	mux.Handle("GET /metrics", a.metrics.Handler())
	return mux
}

// Serve runs the proxy and admin servers until ctx is cancelled. Shutdown marks
// readiness false, waits drain_delay for load balancers to stop routing traffic,
// then closes listeners and waits up to shutdown.timeout for in-flight requests.
// The Redis client closes after draining.
func (a *App) Serve(ctx context.Context, proxyLn, adminLn net.Listener) error {
	cfg := a.Config()
	proxySrv := &http.Server{Handler: a.proxy, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second}
	adminSrv := &http.Server{Handler: a.AdminHandler(), ReadHeaderTimeout: 5 * time.Second}

	bgCtx, stopBackground := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.mem.RunJanitor(bgCtx, 30*time.Second) }()
	go func() { defer wg.Done(); a.reloader.Watch(bgCtx, cfg.Reload.Interval) }()

	errc := make(chan error, 2)
	go func() { errc <- serve(proxySrv, proxyLn) }()
	go func() { errc <- serve(adminSrv, adminLn) }()
	a.log.Info("gateway started", "version", a.version, "proxy", proxyLn.Addr().String(),
		"admin", adminLn.Addr().String(), "backend", a.backend, "upstream", cfg.Upstream)

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errc:
		a.log.Error("server failed", "err", serveErr)
	}

	a.ready.Store(false)
	if serveErr == nil && cfg.Shutdown.DrainDelay > 0 {
		a.log.Info("draining", "delay", cfg.Shutdown.DrainDelay.String())
		time.Sleep(cfg.Shutdown.DrainDelay)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Shutdown.Timeout)
	defer cancel()
	err := errors.Join(serveErr, proxySrv.Shutdown(shutdownCtx), adminSrv.Shutdown(shutdownCtx))
	stopBackground()
	wg.Wait()
	if a.redis != nil {
		err = errors.Join(err, a.redis.Close())
	}
	a.log.Info("gateway stopped", "err", err)
	return err
}

func serve(s *http.Server, ln net.Listener) error {
	if err := s.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// accessLog writes one structured log line per request. Rejections (429, and
// the 503s of a fail-closed limiter) are logged at Debug: under a flood, one
// Info line per rejected request would turn the attack into a logging DoS.
// They are still counted in rlgw_requests_total.
func accessLog(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		level := slog.LevelInfo
		if rec.status == http.StatusTooManyRequests || rec.status == http.StatusServiceUnavailable {
			level = slog.LevelDebug
		}
		if !log.Enabled(r.Context(), level) {
			return
		}
		log.LogAttrs(r.Context(), level, "request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Duration("duration", time.Since(start)),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController (used by ReverseProxy for flushing) reach the real writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// redisLogger routes go-redis's internal logging (dial retries etc.) to slog at
// debug level; failures are already surfaced through metrics and the breaker.
type redisLogger struct{ log *slog.Logger }

func (r redisLogger) Printf(ctx context.Context, format string, v ...any) {
	r.log.DebugContext(ctx, "go-redis: "+fmt.Sprintf(format, v...))
}
