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
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"spot-rl-controller/internal/action"
	awsclient "spot-rl-controller/internal/aws"
	"spot-rl-controller/internal/inference"
	"spot-rl-controller/internal/jenkins"
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
		LoopInterval: time.Duration(getInt("LOOP_INTERVAL_SEC", 300)) * time.Second,
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
	sqsClient := awsclient.NewSQSClient(awsCfg, cfg.SqsQueueUrl)
	pricingClient := awsclient.NewPricingClient(awsCfg, defaultType, types.AZNames[0])

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
	executor := action.NewExecutor(ec2Client, jenkinsClient, instanceRegistry)
	guard := safety.New(safety.DefaultLimits())

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log.Printf("Control loop started (interval: %s, shadow=%v)", cfg.LoopInterval, cfg.ShadowMode)

	ticker := time.NewTicker(cfg.LoopInterval)
	defer ticker.Stop()

	// Run 1 step ngay khi start (không đợi tick đầu).
	if err := runStep(ctx, collector, model, executor, guard, pricingClient, ec2Client, sqsClient, cfg); err != nil {
		log.Printf("[ERROR] initial step: %v", err)
	}

	for {
		select {
		case <-ctx.Done():
			log.Println("Shutting down gracefully...")
			return
		case <-ticker.C:
			if err := runStep(ctx, collector, model, executor, guard, pricingClient, ec2Client, sqsClient, cfg); err != nil {
				log.Printf("[ERROR] step: %v", err)
			}
		}
	}
}

// runStep: 1 vòng lặp collect → predict → safety → execute.
func runStep(
	ctx context.Context,
	collector *state.Collector,
	model *inference.Model,
	executor *action.Executor,
	guard *safety.Guard,
	pricing *awsclient.PricingClient,
	ec2 *awsclient.EC2Client,
	sqs *awsclient.SQSClient,
	cfg Config,
) error {
	t0 := time.Now()

	// 1. Fetch pool data từ AWS — 15 pools (5 types × 3 AZs)
	pools, err := fetchAllPools(ctx, pricing, ec2)
	if err != nil {
		return fmt.Errorf("fetch pools: %w", err)
	}

	// 2. Workload từ SQS
	depth, err := sqs.GetQueueDepth(ctx)
	if err != nil {
		log.Printf("[sqs] failed: %v — assuming 0 jobs", err)
		depth.Pending = 0
		depth.Running = 0
	}

	// 3. Build state vector
	s, err := collector.Collect(ctx, pools, depth.Pending, depth.Running, 0)
	if err != nil {
		return fmt.Errorf("collect: %w", err)
	}
	log.Printf("[state] pending=%d running=%d spot=%d od=%d cost=$%.3f/h",
		s.PendingJobs, s.RunningJobs, s.NumSpot, s.NumOnDemand, s.HourlyCost)

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

	// 5. Safety check
	res := guard.Allow(act, s.NumSpot+s.NumOnDemand, s.HourlyCost)
	if !res.Allowed {
		log.Printf("[safety] BLOCKED: %s", res.Reason)
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

	log.Printf("[step] OK in %s", time.Since(t0))
	return nil
}

// fetchAllPools đọc price + interrupt + count cho 15 pools.
// TODO: parallelize với goroutines + waitgroup khi có thời gian.
func fetchAllPools(
	ctx context.Context,
	pricing *awsclient.PricingClient,
	ec2 *awsclient.EC2Client,
) ([types.NPools]types.PoolInfo, error) {
	var pools [types.NPools]types.PoolInfo
	for ti, instType := range types.InstanceTypes {
		for ai, az := range types.AZNames {
			idx := ti*types.NAZs + ai
			price, err := pricing.GetSpotPrice(ctx, instType, az)
			if err != nil {
				price = pricing.GetOnDemandPrice(instType)
			}
			counts, _ := ec2.GetRunningInstancesByTypeAZ(ctx, instType, az)

			pools[idx] = types.PoolInfo{
				TypeIdx:       ti,
				AZIdx:         ai,
				SpotPrice:     price,
				OnDemandPrice: types.OnDemandPrices[ti],
				InterruptProb: estimateInterrupt(price, types.OnDemandPrices[ti]),
				SpotCount:     counts.Spot,
				OnDemandCount: counts.OnDemand,
				VCPUPerInst:   types.InstanceVCPUs[ti],
				// Placeholders cho v14 features — TODO: populate từ AWS Spot Advisor / EventBridge
				BaseRate:        defaultBaseRate(instType),
				BaselineSavings: 0.7,                       // ~70% savings typical
				SPSScore:        0.7,                       // neutral default
				PriceCV24h:      0.1,
				RebalanceFlag:   false,
				TerminationFlag: false,
				CPUUtil:         0.5, // TODO: lấy từ CloudWatch
				RAMUtil:         0.5,
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
