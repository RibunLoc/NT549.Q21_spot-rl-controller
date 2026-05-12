// Package metrics — Prometheus metrics cho spot-rl-controller.
// Expose qua HTTP /metrics, scrape bởi Prometheus trên droplet monitoring.
// /metrics được bảo vệ bằng Bearer token — set qua env METRICS_TOKEN.
package metrics

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const ns = "spot_rl"

var (
	// Fleet composition
	SpotInstances = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "spot_instances",
		Help: "Number of running spot instances managed by controller",
	})
	OnDemandInstances = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "ondemand_instances",
		Help: "Number of running on-demand instances managed by controller",
	})

	// Workload
	PendingJobs = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "pending_jobs",
		Help: "Number of jobs waiting in queue",
	})
	RunningJobs = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "running_jobs",
		Help: "Number of jobs currently running on agents",
	})

	// Cost
	HourlyCost = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "hourly_cost_dollars",
		Help: "Current fleet hourly cost in USD",
	})

	// SLA
	SLAHealth = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "sla_health",
		Help: "SLA health score [0-1], EMA of compliance rate",
	})

	// Agent decision
	QValue = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "q_value",
		Help: "Q-value of the selected action in last step",
	})
	ActionTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "action_total",
		Help: "Total number of actions executed, labeled by operation name",
	}, []string{"op"})

	// Safety
	SafetyBlockedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: ns, Name: "safety_blocked_total",
		Help: "Total number of actions blocked by safety guard",
	})

	// Step performance
	StepDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: ns, Name: "step_duration_seconds",
		Help:    "Duration of each control loop step in seconds",
		Buckets: []float64{0.1, 0.5, 1.0, 2.0, 5.0, 10.0, 30.0},
	})

	// Shadow mode indicator
	ShadowMode = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "shadow_mode",
		Help: "1 if running in shadow mode (decisions logged only), 0 if live",
	})
)

// Register đăng ký tất cả metrics vào Prometheus default registry.
func Register() {
	prometheus.MustRegister(
		SpotInstances,
		OnDemandInstances,
		PendingJobs,
		RunningJobs,
		HourlyCost,
		SLAHealth,
		QValue,
		ActionTotal,
		SafetyBlockedTotal,
		StepDuration,
		ShadowMode,
	)
}

// Handler trả về HTTP handler cho /metrics endpoint với Bearer token auth.
// token rỗng → không auth (local dev). Token lấy từ env METRICS_TOKEN ở caller.
func Handler(token string) http.Handler {
	inner := promhttp.Handler()
	if token == "" {
		return inner
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		parts := strings.SplitN(auth, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") ||
			subtle.ConstantTimeCompare([]byte(parts[1]), []byte(token)) != 1 {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		inner.ServeHTTP(w, r)
	})
}
