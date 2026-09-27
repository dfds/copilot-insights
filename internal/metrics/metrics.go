package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics holds all Prometheus collectors for copilot-insights. Construct it
// once per process; promauto registers on the default registry and panics on a
// second registration.
type Metrics struct {
	CollectorRuns            *prometheus.CounterVec
	CollectorDuration        *prometheus.HistogramVec
	CollectorLastSuccess     *prometheus.GaugeVec
	CollectorItems           *prometheus.GaugeVec
	GitHubRequests           *prometheus.CounterVec
	GitHubRateLimitWaits     *prometheus.CounterVec
	GitHubAuthFallback       *prometheus.CounterVec
	GitHubRateLimitRemaining *prometheus.GaugeVec
	AuthRejections           prometheus.Counter
}

func NewMetrics() *Metrics {
	return NewMetricsWith(prometheus.DefaultRegisterer)
}

// NewMetricsWith registers on reg.
func NewMetricsWith(reg prometheus.Registerer) *Metrics {
	f := promauto.With(reg)
	return &Metrics{
		CollectorRuns: f.NewCounterVec(prometheus.CounterOpts{
			Name: "copilot_insights_collector_runs_total",
			Help: "Collector runs by terminal status (ok, partial, error)",
		}, []string{"collector", "status"}),
		CollectorDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "copilot_insights_collector_duration_seconds",
			Help:    "Duration of a collector run",
			Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600, 1200, 2400, 3600},
		}, []string{"collector"}),
		CollectorLastSuccess: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "copilot_insights_collector_last_success_timestamp",
			Help: "Unix time of the collector's last ok or partial run",
		}, []string{"collector"}),
		CollectorItems: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "copilot_insights_collector_items",
			Help: "Rows written by the collector's last run",
		}, []string{"collector"}),
		GitHubRequests: f.NewCounterVec(prometheus.CounterOpts{
			Name: "copilot_insights_github_requests_total",
			Help: "GitHub API requests by endpoint family, credential and status code",
		}, []string{"family", "auth", "code"}),
		GitHubRateLimitWaits: f.NewCounterVec(prometheus.CounterOpts{
			Name: "copilot_insights_github_ratelimit_waits_total",
			Help: "Sleeps caused by GitHub rate limiting or server errors",
		}, []string{"reason"}),
		GitHubAuthFallback: f.NewCounterVec(prometheus.CounterOpts{
			Name: "copilot_insights_github_auth_fallback_total",
			Help: "Org requests retried with the PAT after the App token was refused",
		}, []string{"family"}),
		GitHubRateLimitRemaining: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "copilot_insights_github_ratelimit_remaining",
			Help: "Last seen X-RateLimit-Remaining per credential",
		}, []string{"client"}),
		AuthRejections: f.NewCounter(prometheus.CounterOpts{
			Name: "copilot_insights_auth_rejections_total",
			Help: "Rejected inbound API requests",
		}),
	}
}
