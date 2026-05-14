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

// poolLabels là label keys cho per-pool GaugeVec.
var poolLabels = []string{"instance_type", "az"}

var (
	// ── Fleet composition ────────────────────────────────────────────────────
	SpotInstances = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "spot_instances",
		Help: "Total running spot instances across all pools",
	})
	OnDemandInstances = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "ondemand_instances",
		Help: "Total running on-demand instances across all pools",
	})

	// ── Workload ─────────────────────────────────────────────────────────────
	PendingJobs = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "pending_jobs",
		Help: "Number of jobs waiting in queue",
	})
	RunningJobs = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "running_jobs",
		Help: "Number of jobs currently running on agents",
	})

	// ── Cost ──────────────────────────────────────────────────────────────────
	HourlyCost = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "hourly_cost_dollars",
		Help: "Current fleet hourly cost in USD",
	})

	// ── SLA ───────────────────────────────────────────────────────────────────
	SLAHealth = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "sla_health",
		Help: "SLA health score [0-1], EMA of compliance rate",
	})

	// ── Agent decision ────────────────────────────────────────────────────────
	QValue = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "q_value",
		Help: "Q-value of the selected action in last step",
	})
	ActionTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns, Name: "action_total",
		Help: "Total actions executed, labeled by operation name",
	}, []string{"op"})

	// ── Safety ────────────────────────────────────────────────────────────────
	SafetyBlockedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: ns, Name: "safety_blocked_total",
		Help: "Total actions blocked by safety guard",
	})

	// ── Step performance ──────────────────────────────────────────────────────
	StepDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: ns, Name: "step_duration_seconds",
		Help:    "Duration of each control loop step in seconds",
		Buckets: []float64{0.1, 0.5, 1.0, 2.0, 5.0, 10.0, 30.0},
	})

	// ── Shadow mode ───────────────────────────────────────────────────────────
	ShadowMode = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "shadow_mode",
		Help: "1 if running in shadow mode (decisions logged only), 0 if live",
	})

	// ── Per-pool state features (15 pools × instance_type + az labels) ────────
	// Dùng để monitoring state đầu vào model và export dataset training sau.

	PoolSpotPrice = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: ns, Name: "pool_spot_price_dollars",
		Help: "Current spot price $/hr for each pool",
	}, poolLabels)

	PoolPriceRatio = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: ns, Name: "pool_price_ratio",
		Help: "spot_price / od_price ratio [0-2] for each pool",
	}, poolLabels)

	PoolInterruptProb = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: ns, Name: "pool_interrupt_prob",
		Help: "Estimated interruption probability [0-1] for each pool",
	}, poolLabels)

	PoolSpotCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: ns, Name: "pool_spot_count",
		Help: "Running spot instance count per pool",
	}, poolLabels)

	PoolOnDemandCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: ns, Name: "pool_ondemand_count",
		Help: "Running on-demand instance count per pool",
	}, poolLabels)

	PoolCPUUtil = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: ns, Name: "pool_cpu_util",
		Help: "Average CPU utilization [0-1] per pool (CloudWatch)",
	}, poolLabels)

	PoolRAMUtil = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: ns, Name: "pool_ram_util",
		Help: "Average RAM utilization [0-1] per pool (CloudWatch agent)",
	}, poolLabels)

	PoolSPSScore = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: ns, Name: "pool_sps_score",
		Help: "Spot Placement Score [0-1] per pool (DescribeSpotPlacementScores)",
	}, poolLabels)

	PoolPriceCV = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: ns, Name: "pool_price_cv_24h",
		Help: "Price coefficient of variation over 24h per pool (std/mean)",
	}, poolLabels)

	// ── Aggregated state features (từ collector) ──────────────────────────────
	ForecastJobs = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "forecast_jobs_1h",
		Help: "Forecasted job arrivals in next 1 hour (Jenkins history × hourly profile ratio)",
	})
	BuildsLastHour = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "builds_last_hour",
		Help: "Actual Jenkins build count in last 1 hour",
	})
	WorkloadTrend = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "workload_trend",
		Help: "Workload trend [-1,1]: tanh((cur-old)/old) over pending history",
	})
	InterruptStreakRate = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "interrupt_streak_rate",
		Help: "Fraction of recent steps with at least 1 interrupt [0-1]",
	})
	BudgetSpentRatio = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "budget_spent_ratio",
		Help: "total_cost / cumulative_baseline_OD — cost efficiency vs pure OD",
	})
	SpotRatio = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "spot_ratio",
		Help: "spot / (spot + od) instance ratio [0-1]",
	})
	AZSpread = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "az_price_spread",
		Help: "Max - min average spot price ratio across AZs",
	})
	CheaperSpotAvailable = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "cheaper_spot_available",
		Help: "1 if a pool exists with spot price >15% cheaper than current running pools",
	})
	SLARiskScore = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns, Name: "sla_risk_score",
		Help: "SLA risk: weighted(pending/vcpu + interrupt_streak_rate)",
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
		PoolSpotPrice,
		PoolPriceRatio,
		PoolInterruptProb,
		PoolSpotCount,
		PoolOnDemandCount,
		PoolCPUUtil,
		PoolRAMUtil,
		PoolSPSScore,
		PoolPriceCV,
		ForecastJobs,
		BuildsLastHour,
		WorkloadTrend,
		InterruptStreakRate,
		BudgetSpentRatio,
		SpotRatio,
		AZSpread,
		CheaperSpotAvailable,
		SLARiskScore,
	)
}

// PoolSnapshot là dữ liệu 1 pool cần để record metrics — tách khỏi types để tránh import cycle.
type PoolSnapshot struct {
	InstanceType  string
	AZ            string
	SpotPrice     float64
	ODPrice       float64
	InterruptProb float64
	SpotCount     int
	OnDemandCount int
	CPUUtil       float64
	RAMUtil       float64
	SPSScore      float64
	PriceCV24h    float64
}

// RecordPoolMetrics cập nhật tất cả per-pool GaugeVec. Gọi mỗi step sau khi fetch pools.
func RecordPoolMetrics(pools []PoolSnapshot) {
	for _, p := range pools {
		l := prometheus.Labels{"instance_type": p.InstanceType, "az": p.AZ}
		PoolSpotPrice.With(l).Set(p.SpotPrice)
		ratio := 0.0
		if p.ODPrice > 0 {
			ratio = p.SpotPrice / p.ODPrice
		}
		PoolPriceRatio.With(l).Set(ratio)
		PoolInterruptProb.With(l).Set(p.InterruptProb)
		PoolSpotCount.With(l).Set(float64(p.SpotCount))
		PoolOnDemandCount.With(l).Set(float64(p.OnDemandCount))
		PoolCPUUtil.With(l).Set(p.CPUUtil)
		PoolRAMUtil.With(l).Set(p.RAMUtil)
		PoolSPSScore.With(l).Set(p.SPSScore)
		PoolPriceCV.With(l).Set(p.PriceCV24h)
	}
}

// StateSnapshot là aggregated state features từ collector — để record metrics.
type StateSnapshot struct {
	ForecastJobs         float64
	BuildsLastHour       int
	WorkloadTrend        float64
	InterruptStreakRate   float64
	BudgetSpentRatio     float64
	SpotRatio            float64
	AZSpread             float64
	CheaperSpotAvailable float64
	SLARiskScore         float64
}

// RecordStateMetrics cập nhật aggregated state metrics. Gọi mỗi step sau Collect().
func RecordStateMetrics(s StateSnapshot) {
	ForecastJobs.Set(s.ForecastJobs)
	BuildsLastHour.Set(float64(s.BuildsLastHour))
	WorkloadTrend.Set(s.WorkloadTrend)
	InterruptStreakRate.Set(s.InterruptStreakRate)
	BudgetSpentRatio.Set(s.BudgetSpentRatio)
	SpotRatio.Set(s.SpotRatio)
	AZSpread.Set(s.AZSpread)
	CheaperSpotAvailable.Set(s.CheaperSpotAvailable)
	SLARiskScore.Set(s.SLARiskScore)
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
