// Package metrics defines the gateway's Prometheus metrics on a private registry
// so multiple gateways in one process do not register duplicate collectors.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds every collector the gateway exports.
type Metrics struct {
	Registry *prometheus.Registry

	Requests        *prometheus.CounterVec   // route, decision
	LimiterDuration *prometheus.HistogramVec // backend
	BackendErrors   prometheus.Counter
	Degraded        *prometheus.CounterVec // policy
	BreakerState    prometheus.Gauge
	ConfigReloads   *prometheus.CounterVec // result
	UpstreamErrors  prometheus.Counter
	TrackedKeys     prometheus.GaugeFunc
}

// New registers all collectors. trackedKeys may be nil.
func New(trackedKeys func() float64) *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		Registry: reg,
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "rlgw_requests_total",
			Help: "Requests seen by the gateway, by route and rate-limit decision.",
		}, []string{"route", "decision"}),
		LimiterDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "rlgw_limiter_duration_seconds",
			Help:    "Time spent deciding whether to admit a request.",
			Buckets: []float64{.00005, .0001, .00025, .0005, .001, .0025, .005, .01, .025, .05, .1},
		}, []string{"backend"}),
		BackendErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "rlgw_backend_errors_total",
			Help: "Failed calls to the shared limiter backend (Redis).",
		}),
		Degraded: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "rlgw_degraded_decisions_total",
			Help: "Decisions made by the failure policy because the shared backend was unavailable.",
		}, []string{"policy"}),
		BreakerState: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "rlgw_breaker_state",
			Help: "Redis circuit breaker state: 0 closed, 1 half-open, 2 open.",
		}),
		ConfigReloads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "rlgw_config_reloads_total",
			Help: "Configuration reload attempts by result.",
		}, []string{"result"}),
		UpstreamErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "rlgw_upstream_errors_total",
			Help: "Requests that failed to reach the upstream.",
		}),
	}
	reg.MustRegister(m.Requests, m.LimiterDuration, m.BackendErrors, m.Degraded,
		m.BreakerState, m.ConfigReloads, m.UpstreamErrors,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	if trackedKeys != nil {
		m.TrackedKeys = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "rlgw_memory_tracked_keys",
			Help: "Keys currently held by the in-memory limiter.",
		}, trackedKeys)
		reg.MustRegister(m.TrackedKeys)
	}
	return m
}

// Handler serves the registry in the Prometheus exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{Registry: m.Registry})
}
