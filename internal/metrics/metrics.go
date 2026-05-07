// Package metrics exposes Prometheus instrumentation for the docstore HTTP
// server. The intent is "observable enough to operate" — broad HTTP coverage
// from the middleware, a small set of high-signal domain counters, and the
// stock process / Go runtime collectors that come free with the client lib.
//
// Cardinality is the watchword. HTTP labels use Go 1.22+ ServeMux route
// patterns rather than raw paths so a request bursting through every repo
// does not create one time-series per repo. Domain metrics keep their label
// sets fixed and small.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry bundles a private Prometheus registry with strongly-typed handles
// for every metric the server emits. Callers obtain handles via the methods,
// not by re-registering against the global default registry.
type Registry struct {
	reg *prometheus.Registry

	httpRequests    *prometheus.CounterVec
	httpDuration    *prometheus.HistogramVec
	httpInFlight    prometheus.Gauge

	commits         prometheus.Counter
	ciJobs          *prometheus.CounterVec
	secretOps       *prometheus.CounterVec
	kmsOps          *prometheus.CounterVec
}

// New constructs a Registry and registers every metric. Process and Go
// runtime collectors are registered on the same private registry so the
// /metrics endpoint exports them too.
func New() *Registry {
	r := prometheus.NewRegistry()
	r.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	r.MustRegister(collectors.NewGoCollector())

	m := &Registry{reg: r}

	m.httpRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "docstore",
			Subsystem: "http",
			Name:      "requests_total",
			Help:      "Total HTTP requests by method, ServeMux route pattern, and response code.",
		},
		[]string{"method", "pattern", "code"},
	)
	m.httpDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "docstore",
			Subsystem: "http",
			Name:      "request_duration_seconds",
			Help:      "HTTP request duration in seconds by method and route pattern.",
			Buckets:   prometheus.ExponentialBuckets(0.001, 2, 14), // 1ms .. ~16s
		},
		[]string{"method", "pattern"},
	)
	m.httpInFlight = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "docstore",
		Subsystem: "http",
		Name:      "in_flight_requests",
		Help:      "In-flight HTTP requests currently being served.",
	})
	m.commits = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "docstore",
		Name:      "commits_total",
		Help:      "Total successful commits across all repos.",
	})
	m.ciJobs = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "docstore",
			Subsystem: "ci",
			Name:      "jobs_total",
			Help:      "CI job lifecycle transitions, labeled by terminal status.",
		},
		[]string{"status"}, // queued, claimed, passed, failed
	)
	m.secretOps = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "docstore",
			Subsystem: "secrets",
			Name:      "operations_total",
			Help:      "Repo-secret operations by name. Never carries the value or the secret name.",
		},
		[]string{"operation"}, // created, updated, deleted, accessed
	)
	m.kmsOps = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "docstore",
			Subsystem: "kms",
			Name:      "operations_total",
			Help:      "KMS Encrypt/Decrypt calls by result.",
		},
		[]string{"op", "result"}, // op: encrypt|decrypt, result: ok|error
	)
	r.MustRegister(m.httpRequests, m.httpDuration, m.httpInFlight,
		m.commits, m.ciJobs, m.secretOps, m.kmsOps)
	return m
}

// Handler returns the http.Handler that serves /metrics. The handler exposes
// only this Registry's metrics — the global default registry is not used so
// transitive deps that auto-register on it don't pollute the export.
func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{
		// EnableOpenMetrics turns on exemplars + native histograms when the
		// scraper requests OpenMetrics format; harmless when it doesn't.
		EnableOpenMetrics: true,
	})
}

// Middleware wraps next to record HTTP request count, duration, and in-flight
// gauge. Labels: method, route pattern, status code. The pattern is read
// after next.ServeHTTP returns because Go's mux only sets r.Pattern during
// routing.
//
// Requests that don't match a registered pattern are bucketed under "other"
// to keep cardinality bounded.
func (r *Registry) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.httpInFlight.Inc()
		defer r.httpInFlight.Dec()

		start := time.Now()
		sw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, req)

		pattern := req.Pattern
		if pattern == "" {
			pattern = "other"
		}
		method := req.Method

		r.httpRequests.WithLabelValues(method, pattern, strconv.Itoa(sw.status)).Inc()
		r.httpDuration.WithLabelValues(method, pattern).Observe(time.Since(start).Seconds())
	})
}

// CommitsTotal returns the counter handle for successful commits.
func (r *Registry) CommitsTotal() prometheus.Counter { return r.commits }

// CIJobs returns the counter for CI job status transitions.
func (r *Registry) CIJobs() *prometheus.CounterVec { return r.ciJobs }

// SecretOps returns the counter for repo-secret operations.
func (r *Registry) SecretOps() *prometheus.CounterVec { return r.secretOps }

// KMSOps returns the counter for KMS Encrypt/Decrypt calls.
func (r *Registry) KMSOps() *prometheus.CounterVec { return r.kmsOps }

// statusRecorder is a tiny ResponseWriter wrapper that captures the status
// code so the middleware can label the metric correctly. It is NOT a full
// http.Hijacker / http.Flusher proxy — those interfaces are not needed by
// the handlers we expose.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.wrote {
		// http.ResponseWriter forbids multiple WriteHeader calls; mirror
		// stdlib's behaviour and ignore the second one rather than panic.
		return
	}
	s.status = code
	s.wrote = true
	s.ResponseWriter.WriteHeader(code)
}

// Write implements io.Writer. The standard library promotes Write to call
// WriteHeader(200) implicitly when none has been set; we mirror that so the
// captured status reflects the success path.
func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.wrote = true
	}
	return s.ResponseWriter.Write(b)
}
