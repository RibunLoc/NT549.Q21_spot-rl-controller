// spot-rl-controller — agent quyết định action mỗi N phút,
// pass safety guardrails, gọi EC2 API.
//
// Architecture:
//
//	┌─────────────┐  collect  ┌──────────┐  predict ┌────────┐
//	│  AWS APIs   │ ────────▶ │Collector │ ───────▶ │ Model  │
//	└─────────────┘           └──────────┘          └────┬───┘
//	                                                      │ action
//	                                                      ▼
//	                                              ┌──────────────┐
//	                                              │ Safety guard │
//	                                              └──────┬───────┘
//	                                                     │ allowed
//	                                                     ▼
//	                                              ┌──────────────┐
//	                                              │   Executor   │
//	                                              └──────────────┘
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"spot-rl-controller/internal/action"
	awsclient "spot-rl-controller/internal/aws"
	"spot-rl-controller/internal/inference"
	"spot-rl-controller/internal/jenkins"
	"spot-rl-controller/internal/metrics"
	"spot-rl-controller/internal/registry"
	"spot-rl-controller/internal/safety"
	"spot-rl-controller/internal/state"
	"spot-rl-controller/pkg/types"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/joho/godotenv"
)

// Config từ env vars.
type Config struct {
	ModelPath    string
	AMIID        string
	LoopInterval time.Duration
	EpisodeSteps int
	SqsQueueUrl  string
	AZSubnets    awsclient.AZSubnetMap
	ShadowMode   bool // true = log decisions, không execute
	JenkinsURL   string
	JenkinsUser  string
	JenkinsToken string
	MetricsAddr  string // HTTP addr cho /metrics, e.g. ":9090"
}

func loadConfig() Config {
	getEnv := func(key, fallback string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return fallback
	}
	getInt := func(key string, fb int) int {
		if v := os.Getenv(key); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				return n
			}
		}
		return fb
	}
	getBool := func(key string, fb bool) bool {
		v := os.Getenv(key)
		return v == "1" || v == "true" || v == "yes" || (v == "" && fb)
	}
	return Config{
		ModelPath:    getEnv("MODEL_PATH", "models/dqn_stable.onnx"),
		AMIID:        getEnv("AMI_ID", ""),
		LoopInterval: time.Duration(getInt("LOOP_INTERVAL_SEC", 900)) * time.Second,
		EpisodeSteps: getInt("EPISODE_STEPS", 672),
		SqsQueueUrl:  getEnv("SQS_QUEUE_URL", ""),
		AZSubnets: awsclient.AZSubnetMap{
			types.AZNames[0]: getEnv("SUBNET_AZ_A", ""),
			types.AZNames[1]: getEnv("SUBNET_AZ_B", ""),
			types.AZNames[2]: getEnv("SUBNET_AZ_C", ""),
		},
		ShadowMode:   getBool("SHADOW_MODE", true),
		JenkinsURL:   getEnv("JENKINS_URL", ""),
		JenkinsUser:  getEnv("JENKINS_USER", ""),
		JenkinsToken: getEnv("JENKINS_TOKEN", ""),
		MetricsAddr:  getEnv("METRICS_ADDR", ":9091"),
	}
}

func main() {
	log.SetFlags(log.Ltime | log.Lshortfile)
	inference.SetSharedLibraryPath("./onnxruntime.dll")
	_ = godotenv.Load("configs/.env")

	log.Println("=== spot-rl-controller starting ===")
	cfg := loadConfig()
	log.Printf("Config: model=%s shadow=%v interval=%s episode_steps=%d",
		cfg.ModelPath, cfg.ShadowMode, cfg.LoopInterval, cfg.EpisodeSteps)

	// Prometheus metrics
	metrics.Register()
	if cfg.ShadowMode {
		metrics.ShadowMode.Set(1)
	}
	metricsToken := os.Getenv("METRICS_TOKEN")
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", metrics.Handler(metricsToken))
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		if metricsToken == "" {
			log.Printf("[metrics] WARNING: METRICS_TOKEN not set — /metrics is unauthenticated")
		}
		log.Printf("Metrics server listening on %s", cfg.MetricsAddr)
		if err := http.ListenAndServe(cfg.MetricsAddr, mux); err != nil {
			log.Printf("[metrics] server error: %v", err)
		}
	}()

	// Load ONNX model
	model, err := inference.New(cfg.ModelPath)
	if err != nil {
		log.Fatalf("Load model: %v", err)
	}
	defer model.Close()
	log.Println("Model loaded OK")

	// AWS clients
	awsCfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		log.Fatalf("AWS config: %v", err)
	}
	// EC2: dùng instance type đầu tiên làm default, executor SetAZ trước mỗi op.
	defaultType := types.InstanceTypes[0]
	ec2Client := awsclient.NewEC2ClientMultiAZ(awsCfg, defaultType, cfg.AZSubnets, cfg.AMIID)
	if ltID := os.Getenv("WORKER_LAUNCH_TEMPLATE"); ltID != "" {
		ec2Client.SetLaunchTemplate(ltID)
	}
	sqsClient := awsclient.NewSQSClient(awsCfg, cfg.SqsQueueUrl)
	pricingClient := awsclient.NewPricingClient(awsCfg, defaultType, types.AZNames[0])
	cwClient := awsclient.NewCloudWatchClient(awsCfg)

	// Jenkins + InstanceRegistry (optional — nil-safe nếu không cấu hình)
	var jenkinsClient *jenkins.Client
	var instanceRegistry *registry.Registry
	if cfg.JenkinsURL != "" {
		jenkinsClient = jenkins.NewClient(cfg.JenkinsURL, cfg.JenkinsUser, cfg.JenkinsToken)
		instanceRegistry = registry.New(awsCfg)
		log.Printf("Jenkins client: %s (user=%s)", cfg.JenkinsURL, cfg.JenkinsUser)
	} else {
		log.Println("Jenkins not configured — drain disabled")
	}

	collector := state.NewCollector(ec2Client, sqsClient, cfg.EpisodeSteps)
	executor := action.NewExecutor(ec2Client, pricingClient, jenkinsClient, instanceRegistry)
	guard := safety.New(safety.DefaultLimits())

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log.Printf("Control loop started (interval: %s, shadow=%v)", cfg.LoopInterval, cfg.ShadowMode)

	ticker := time.NewTicker(cfg.LoopInterval)
	defer ticker.Stop()

	// Run 1 step ngay khi start (không đợi tick đầu).
	if err := runStep(ctx, collector, model, executor, guard, pricingClient, ec2Client, sqsClient, jenkinsClient, cwClient, cfg); err != nil {
		log.Printf("[ERROR] initial step: %v", err)
	}

	for {
		select {
		case <-ctx.Done():
			log.Println("Shutting down gracefully...")
			return
		case <-ticker.C:
			if err := runStep(ctx, collector, model, executor, guard, pricingClient, ec2Client, sqsClient, jenkinsClient, cwClient, cfg); err != nil {
				log.Printf("[ERROR] step: %v", err)
			}
		}
	}
}

// runStep: 1 vòng lặp collect → predict → safety → execute.
func workloadSource(j *jenkins.Client) string {
	if j != nil {
		return "jenkins"
	}
	return "sqs"
}

func runStep(
	ctx context.Context,
	collector *state.Collector,
	model *inference.Model,
	executor *action.Executor,
	guard *safety.Guard,
	pricing *awsclient.PricingClient,
	ec2 *awsclient.EC2Client,
	sqs *awsclient.SQSClient,
	j *jenkins.Client,
	cw *awsclient.CloudWatchClient,
	cfg Config,
) error {
	t0 := time.Now()
	defer func() { metrics.StepDuration.Observe(time.Since(t0).Seconds()) }()

	// 1. Fetch pool data từ AWS — 15 pools (5 types × 3 AZs)
	pools, err := fetchAllPools(ctx, pricing, ec2, cw)
	if err != nil {
		return fmt.Errorf("fetch pools: %w", err)
	}

	// 2. Workload — ưu tiên Jenkins queue nếu có, fallback về SQS
	var depth jenkins.QueueDepth
	if j != nil {
		depth, err = j.GetQueueDepth(ctx)
		if err != nil {
			log.Printf("[jenkins] queue failed: %v — falling back to SQS", err)
			sqsDepth, sqsErr := sqs.GetQueueDepth(ctx)
			if sqsErr != nil {
				log.Printf("[sqs] failed: %v — assuming 0 jobs", sqsErr)
			} else {
				depth.Pending = sqsDepth.Pending
				depth.Running = sqsDepth.Running
			}
		}
	} else {
		sqsDepth, sqsErr := sqs.GetQueueDepth(ctx)
		if sqsErr != nil {
			log.Printf("[sqs] failed: %v — assuming 0 jobs", sqsErr)
		} else {
			depth.Pending = sqsDepth.Pending
			depth.Running = sqsDepth.Running
		}
	}
	log.Printf("[workload] pending=%d running=%d (source=%s)",
		depth.Pending, depth.Running, workloadSource(j))

	// 3. Build state vector
	s, err := collector.Collect(ctx, pools, depth.Pending, depth.Running, 0)
	if err != nil {
		return fmt.Errorf("collect: %w", err)
	}
	log.Printf("[state] pending=%d running=%d spot=%d od=%d cost=$%.3f/h",
		s.PendingJobs, s.RunningJobs, s.NumSpot, s.NumOnDemand, s.HourlyCost)

	// Record fleet + workload metrics
	metrics.SpotInstances.Set(float64(s.NumSpot))
	metrics.OnDemandInstances.Set(float64(s.NumOnDemand))
	metrics.PendingJobs.Set(float64(s.PendingJobs))
	metrics.RunningJobs.Set(float64(s.RunningJobs))
	metrics.HourlyCost.Set(s.HourlyCost)
	metrics.SLAHealth.Set(collector.SLAHealth())

	// 4. Build action mask — chỉ argmax trên actions hợp lệ
	mask := state.BuildActionMask(pools, depth.Pending, depth.Running)

	// 5. Inference
	act, decoded, qValues, err := model.Predict(s, &mask)
	if err != nil {
		return fmt.Errorf("predict: %w", err)
	}
	maxQ := qValues[int(act)]
	log.Printf("[decision] action=%d op=%s type=%s az=%s Q=%.3f",
		int(act), decoded.OpName, decoded.InstanceType, decoded.AZ, maxQ)
	metrics.QValue.Set(float64(maxQ))

	// 5. Safety check
	res := guard.Allow(act, s.NumSpot+s.NumOnDemand, s.HourlyCost)
	if !res.Allowed {
		log.Printf("[safety] BLOCKED: %s", res.Reason)
		metrics.SafetyBlockedTotal.Inc()
		return nil
	}

	// 6. Shadow mode: log only, không execute
	if cfg.ShadowMode {
		log.Printf("[shadow] would execute: %s (skipped — shadow mode)", decoded.OpName)
		return nil
	}

	// 7. Execute
	if err := executor.Execute(ctx, act, s.PendingJobs); err != nil {
		return fmt.Errorf("execute: %w", err)
	}
	guard.MarkExecuted(act)
	metrics.ActionTotal.WithLabelValues(decoded.OpName).Inc()

	log.Printf("[step] OK in %s", time.Since(t0))
	return nil
}

// fetchAllPools đọc price + interrupt + count + CloudWatch util cho 15 pools.
func fetchAllPools(
	ctx context.Context,
	pricing *awsclient.PricingClient,
	ec2 *awsclient.EC2Client,
	cw *awsclient.CloudWatchClient,
) ([types.NPools]types.PoolInfo, error) {
	// Fetch CloudWatch util once for all pools (1 API call per pool, batched)
	// lookback = 15 min (1 step) so we always get the latest window
	instanceTypes := make([]string, len(types.InstanceTypes))
	for i, t := range types.InstanceTypes {
		instanceTypes[i] = t
	}
	azNames := make([]string, len(types.AZNames))
	for i, az := range types.AZNames {
		azNames[i] = az
	}
	utilMap := cw.GetPoolUtilization(ctx, instanceTypes, azNames, 15)

	var pools [types.NPools]types.PoolInfo
	for ti, instType := range types.InstanceTypes {
		for ai, az := range types.AZNames {
			idx := ti*types.NAZs + ai
			price, err := pricing.GetSpotPrice(ctx, instType, az)
			if err != nil {
				price = pricing.GetOnDemandPrice(instType)
			}
			counts, _ := ec2.GetRunningInstancesByTypeAZ(ctx, instType, az)

			util := utilMap[instType+"/"+az]
			cpuUtil := util.CPUUtil
			ramUtil := util.RAMUtil
			if cpuUtil == 0 && ramUtil == 0 {
				cpuUtil = 0.5
				ramUtil = 0.5
			}

			// baseline_savings = 1 - long_term_spot_ratio (0.35) = 0.65 — khớp instance_catalog.py
			const baselineSavings = 0.65

			// PriceCV: tính từ 24h spot price history (std/mean)
			priceCV := pricing.GetPriceCV(ctx, instType, az)

			// SPS: gọi DescribeSpotPlacementScores, fallback 0.8
			sps := pricing.GetSpotPlacementScore(ctx, instType, az)

			pools[idx] = types.PoolInfo{
				TypeIdx:         ti,
				AZIdx:           ai,
				SpotPrice:       price,
				OnDemandPrice:   types.OnDemandPrices[ti],
				InterruptProb:   estimateInterrupt(price, types.OnDemandPrices[ti]),
				SpotCount:       counts.Spot,
				OnDemandCount:   counts.OnDemand,
				VCPUPerInst:     types.InstanceVCPUs[ti],
				BaseRate:        defaultBaseRate(instType),
				BaselineSavings: baselineSavings,
				SPSScore:        sps,
				PriceCV24h:      priceCV,
				RebalanceFlag:   false,
				TerminationFlag: false,
				CPUUtil:         cpuUtil,
				RAMUtil:         ramUtil,
			}
		}
	}
	return pools, nil
}

// estimateInterrupt — fallback nếu không có Spot Advisor data.
// Giá càng cao so OD → AWS ít cần lấy → interrupt thấp.
func estimateInterrupt(spot, od float64) float64 {
	if od <= 0 {
		return 0.05
	}
	ratio := spot / od
	p := 0.05 + (1-ratio)*0.1
	if p < 0.01 {
		p = 0.01
	}
	if p > 0.5 {
		p = 0.5
	}
	return p
}

func defaultBaseRate(instType string) float64 {
	// Khớp instance_catalog.py base_interrupt_rate
	rates := map[string]float64{
		"m5.large":   0.02,
		"c5.xlarge":  0.02,
		"r5.large":   0.03,
		"m5.xlarge":  0.02,
		"c5.2xlarge": 0.02,
	}
	if r, ok := rates[instType]; ok {
		return r
	}
	return 0.03
}
