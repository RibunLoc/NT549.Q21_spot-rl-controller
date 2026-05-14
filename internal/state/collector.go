// State collector — build 92-feature vector từ AWS API.
// Layout khớp envs/spot_orchestrator_env.py:_get_observation():
//
//	[0..29]   Top-3 cheapest combos × 10 features
//	[30..32]  Multi-AZ aggregated
//	[33..35]  Multi-Type aggregated
//	[36..38]  Infrastructure
//	[39..44]  Workload (pending, running, forecast, queue_wait, avg_cpu_demand, avg_ram_demand)
//	[45..47]  Time
//	[48..52]  Current state
//	[53..61]  Extra context
//	[62..76]  Per-pool CPU util (15)
//	[77..91]  Per-pool RAM util (15)
package state

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	awsclient "spot-rl-controller/internal/aws"
	"spot-rl-controller/pkg/types"
)

// Normalization bounds — khớp env Python.
const (
	maxPriceRatio    = 2.0   // price/OD ratio max
	maxVCPUDollar    = 80.0  // vcpu/$ — c5.2xlarge spot ~80 vCPU/$
	maxBaseRate      = 0.1   // 10% base interrupt
	maxPriceCV       = 0.5
	maxInstances     = 20.0
	maxVCPU          = 160.0
	maxJobs          = 100.0
	maxJobsForecast  = 300.0 // = maxJobs * 3 (horizon)
	maxQueueWait     = 10.0
	maxCostRate      = 5.0 // $/hour cluster
	rollingPriceLen  = 24  // for price trend feature
	pendingHistoryLen = 5
	interruptHistoryLen = 10
)

// Pool combo entry để sort top-3.
type comboEntry struct {
	typeIdx, azIdx int
	priceRatio     float64 // price / OD
	interrupt      float64
	vcpuPerDollar  float64
	baseRate       float64
	baselineSavings float64
	spsScore       float64
	priceCV        float64
	rebalance      float64 // 0/1
	termination    float64 // 0/1
}

// Collector accumulates rolling state across steps.
type Collector struct {
	ec2 *awsclient.EC2Client
	sqs *awsclient.SQSClient

	episodeSteps int
	startTime    time.Time

	// Rolling buffers cho trend features
	pendingHistory   []int     // last 5 pending counts
	interruptHistory []int     // last 10 interrupts/step
	cheapestPrices   []float64 // last 24 cheapest spot prices (for trend)

	// Cumulative
	totalCost            float64
	cumulativeBaselineOD float64

	// SLA EMA
	slaHealth float64

	// Last step diagnostics — expose qua LastStateMetrics()
	lastMetrics StateMetrics
}

// StateMetrics là các giá trị diagnostic từ step cuối — dùng để record Prometheus metrics.
type StateMetrics struct {
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

// LastStateMetrics trả về diagnostics từ step vừa Collect() — gọi sau Collect().
func (c *Collector) LastStateMetrics() StateMetrics { return c.lastMetrics }

func NewCollector(ec2 *awsclient.EC2Client, sqs *awsclient.SQSClient, episodeSteps int) *Collector {
	return &Collector{
		ec2:          ec2,
		sqs:          sqs,
		episodeSteps: episodeSteps,
		startTime:    time.Now(),
		slaHealth:    1.0,
	}
}

// Collect đọc AWS state và build 90-feature vector.
//
// Caller cung cấp pools đã được fetch song song (price + interrupt + util).
// Hàm này chỉ build vector — không tự gọi AWS để dễ test.

// SLAHealth trả về SLA health score hiện tại [0, 1].
func (c *Collector) SLAHealth() float64 { return c.slaHealth }

func (c *Collector) Collect(
	ctx context.Context,
	pools [types.NPools]types.PoolInfo,
	pendingJobs, runningJobs int,
	stepInterrupts int,
	buildsLastHour int,
) (types.State, error) {
	feats := make([]float32, 0, types.StateDim)

	// 1) Top-3 cheapest combos × 10 features = 30
	combos := buildCombos(pools)
	sort.Slice(combos, func(i, j int) bool {
		return combos[i].priceRatio < combos[j].priceRatio
	})
	for i := 0; i < types.NTopK; i++ {
		var cb comboEntry
		if i < len(combos) {
			cb = combos[i]
		}
		feats = append(feats,
			clamp01F(cb.priceRatio/maxPriceRatio),
			clamp01F(cb.interrupt),
			clamp01F(cb.vcpuPerDollar/maxVCPUDollar),
			float32(cb.azIdx)/float32(types.NAZs-1),
			clamp01F(cb.baseRate/maxBaseRate),
			clamp01F(cb.baselineSavings),
			clamp01F(cb.spsScore),
			clamp01F(cb.priceCV/maxPriceCV),
			float32(cb.rebalance),
			float32(cb.termination),
		)
	}

	// 2) Multi-AZ aggregated: az_price_spread, cheapest_az_id, az_concentration
	azAvgPrice := make([]float64, types.NAZs)
	azCounts := make([]int, types.NAZs)
	for ti := 0; ti < types.NTypes; ti++ {
		for ai := 0; ai < types.NAZs; ai++ {
			p := pools[ti*types.NAZs+ai]
			azAvgPrice[ai] += priceRatio(p)
			azCounts[ai] += p.SpotCount + p.OnDemandCount
		}
	}
	cheapestAZ, cheapestVal, maxVal := 0, math.MaxFloat64, 0.0
	for ai := 0; ai < types.NAZs; ai++ {
		azAvgPrice[ai] /= float64(types.NTypes)
		if azAvgPrice[ai] < cheapestVal {
			cheapestVal = azAvgPrice[ai]
			cheapestAZ = ai
		}
		if azAvgPrice[ai] > maxVal {
			maxVal = azAvgPrice[ai]
		}
	}
	azSpread := maxVal - cheapestVal
	totalInst := 0
	maxAZCount := 0
	for _, c := range azCounts {
		totalInst += c
		if c > maxAZCount {
			maxAZCount = c
		}
	}
	azConc := 0.0
	if totalInst > 0 {
		azConc = float64(maxAZCount) / float64(totalInst)
	}
	feats = append(feats,
		clamp01F(azSpread),
		float32(cheapestAZ)/float32(types.NAZs-1),
		clamp01F(azConc),
	)

	// 3) Multi-Type aggregated: best_type_price_rank, worst_interrupt_rank, best_vcpu_ratio
	typePriceAvg := make([]float64, types.NTypes)
	typeInterruptAvg := make([]float64, types.NTypes)
	typeVCPURatioAvg := make([]float64, types.NTypes)
	for ti := 0; ti < types.NTypes; ti++ {
		for ai := 0; ai < types.NAZs; ai++ {
			p := pools[ti*types.NAZs+ai]
			typePriceAvg[ti] += priceRatio(p)
			typeInterruptAvg[ti] += p.InterruptProb
			if p.SpotPrice > 0 {
				typeVCPURatioAvg[ti] += float64(p.VCPUPerInst) / p.SpotPrice
			}
		}
		typePriceAvg[ti] /= float64(types.NAZs)
		typeInterruptAvg[ti] /= float64(types.NAZs)
		typeVCPURatioAvg[ti] /= float64(types.NAZs)
	}
	bestTypeRank := argMin(typePriceAvg)
	worstInterRank := argMax(typeInterruptAvg)
	bestVCPUType := argMax(typeVCPURatioAvg)
	feats = append(feats,
		float32(bestTypeRank)/float32(types.NTypes-1),
		float32(worstInterRank)/float32(types.NTypes-1),
		clamp01F(typeVCPURatioAvg[bestVCPUType]/maxVCPUDollar),
	)

	// 4) Infrastructure: num_spot, num_od, total_vcpu
	totalSpot, totalOD, totalVCPU := 0, 0, 0
	hourlyCost := 0.0
	for _, p := range pools {
		totalSpot += p.SpotCount
		totalOD += p.OnDemandCount
		totalVCPU += (p.SpotCount + p.OnDemandCount) * p.VCPUPerInst
		hourlyCost += float64(p.SpotCount) * p.SpotPrice
		hourlyCost += float64(p.OnDemandCount) * p.OnDemandPrice
	}
	feats = append(feats,
		clamp01F(float64(totalSpot)/maxInstances),
		clamp01F(float64(totalOD)/maxInstances),
		clamp01F(float64(totalVCPU)/maxVCPU),
	)

	// 5) Workload: pending, running, forecast_1h, queue_wait, avg_cpu_demand, avg_ram_demand
	c.pendingHistory = pushTrim(c.pendingHistory, pendingJobs, pendingHistoryLen)
	// forecast_1h: dùng build arrival rate 1h qua × hourly profile ratio (giờ tới / giờ hiện tại)
	// Fallback về pending*1.2 nếu chưa có Jenkins history
	vnLoc, _ := time.LoadLocation("Asia/Ho_Chi_Minh")
	now2 := time.Now().In(vnLoc)
	curHour := now2.Hour()
	nextHour := (curHour + 1) % 24
	// Khớp HOURLY_PROFILE trong envs/workload_generator.py
	hourlyProfile := [24]float64{
		0.3, 0.2, 0.2, 0.2, 0.3, 0.5,
		0.7, 0.9, 1.2, 1.5, 1.6, 1.4,
		1.2, 1.5, 1.8, 2.0, 1.8, 1.5,
		1.2, 1.0, 0.8, 0.6, 0.5, 0.4,
	}
	var forecast float64
	if buildsLastHour > 0 {
		ratio := hourlyProfile[nextHour] / math.Max(hourlyProfile[curHour], 0.1)
		forecast = float64(buildsLastHour) * ratio
	} else {
		forecast = float64(pendingJobs) * 1.2
	}
	avgWait := 0.0
	if len(c.pendingHistory) > 0 {
		s := 0
		for _, v := range c.pendingHistory {
			s += v
		}
		avgWait = float64(s) / float64(len(c.pendingHistory))
	}
	// avg_cpu_demand / avg_ram_demand: proxy từ utilization hiện tại của fleet.
	// Khi không có job scheduler real, dùng avg CPU util của running instances.
	avgCPUUtil := 0.0
	avgRAMUtil := 0.0
	activeCnt := 0
	for _, p := range pools {
		if p.SpotCount+p.OnDemandCount > 0 {
			avgCPUUtil += p.CPUUtil
			avgRAMUtil += p.RAMUtil
			activeCnt++
		}
	}
	if activeCnt > 0 {
		avgCPUUtil /= float64(activeCnt)
		avgRAMUtil /= float64(activeCnt)
	} else {
		avgCPUUtil = 0.2 // default: light workload
		avgRAMUtil = 0.0625 // 1GB / 16GB max
	}
	feats = append(feats,
		clamp01F(float64(pendingJobs)/maxJobs),
		clamp01F(float64(runningJobs)/maxJobs),
		clamp01F(forecast/maxJobsForecast),
		clamp01F(avgWait/maxQueueWait),
		clamp01F(avgCPUUtil),  // avg_cpu_demand [25]
		clamp01F(avgRAMUtil),  // avg_ram_demand [26]
	)

	// 6) Time: hour, day, progress — dùng Asia/Ho_Chi_Minh khớp training data
	now := time.Now().In(vnLoc)
	progress := math.Min(time.Since(c.startTime).Seconds()/(float64(c.episodeSteps)*900.0), 1.0) // 15min/step
	feats = append(feats,
		float32(now.Hour())/23.0,
		float32(now.Weekday())/6.0,
		float32(progress),
	)

	// 7) Current state: avg_spot_price, avg_interrupt, spot_ratio, cost_rate, sla_health
	avgSpotP, avgInter, activePools := 0.0, 0.0, 0
	for _, p := range pools {
		if p.SpotCount > 0 || p.OnDemandCount > 0 {
			avgSpotP += p.SpotPrice
			avgInter += p.InterruptProb
			activePools++
		}
	}
	if activePools > 0 {
		avgSpotP /= float64(activePools)
		avgInter /= float64(activePools)
	}
	maxOD := 0.0
	for _, od := range types.OnDemandPrices {
		if od > maxOD {
			maxOD = od
		}
	}
	spotRatio := 0.0
	if totalSpot+totalOD > 0 {
		spotRatio = float64(totalSpot) / float64(totalSpot+totalOD)
	}
	c.totalCost += hourlyCost
	// SLA EMA: drop nếu pending tăng đột biến
	if len(c.pendingHistory) >= 2 {
		prev := c.pendingHistory[len(c.pendingHistory)-2]
		if pendingJobs > prev*2 && pendingJobs > 10 {
			c.slaHealth = math.Max(c.slaHealth-0.05, 0.0)
		} else {
			c.slaHealth = math.Min(c.slaHealth+0.01, 1.0)
		}
	}
	feats = append(feats,
		clamp01F(avgSpotP/maxOD),
		clamp01F(avgInter),
		float32(spotRatio),
		clamp01F(hourlyCost/maxCostRate),
		clamp01F(c.slaHealth),
	)

	// 8) Extra context (9 features) [33-41 in Python]
	// [33] budget_spent_ratio: total_cost / cumulative_baseline_OD (current fleet × OD)
	currentBaseline := 0.0
	for _, p := range pools {
		currentBaseline += float64(p.SpotCount+p.OnDemandCount) * p.OnDemandPrice
	}
	c.cumulativeBaselineOD += currentBaseline
	budgetRatio := 0.0
	if c.cumulativeBaselineOD > 1e-6 {
		budgetRatio = c.totalCost / c.cumulativeBaselineOD
	}
	feats = append(feats, clamp01F(math.Min(budgetRatio, 2.0)/2.0))

	// [34] idle_spot_vcpu_ratio
	totalSpotVCPU := 0
	for _, p := range pools {
		totalSpotVCPU += p.SpotCount * p.VCPUPerInst
	}
	idleSpotVCPU := math.Max(0, float64(totalSpotVCPU-runningJobs))
	feats = append(feats, clamp01F(idleSpotVCPU/maxVCPU))

	// [35] workload_trend
	trend := 0.0
	if len(c.pendingHistory) >= 2 {
		old := c.pendingHistory[0]
		cur := c.pendingHistory[len(c.pendingHistory)-1]
		denom := math.Max(float64(old), 1)
		trend = math.Tanh(float64(cur-old) / denom)
	}
	feats = append(feats, clamp01F((trend+1.0)/2.0))

	// [36] interrupt_streak_rate
	c.interruptHistory = pushTrim(c.interruptHistory, stepInterrupts, interruptHistoryLen)
	interStreak := 0
	for _, x := range c.interruptHistory {
		if x > 0 {
			interStreak++
		}
	}
	rate := 0.0
	if len(c.interruptHistory) > 0 {
		rate = float64(interStreak) / float64(len(c.interruptHistory))
	}
	feats = append(feats, clamp01F(rate))

	// [37] running_pool_price_ratio
	avgRunningPrice, runningPools := 0.0, 0
	for _, p := range pools {
		if p.SpotCount > 0 {
			avgRunningPrice += p.SpotPrice
			runningPools++
		}
	}
	if runningPools > 0 && maxOD > 0 {
		feats = append(feats, clamp01F(avgRunningPrice/float64(runningPools)/maxOD))
	} else {
		feats = append(feats, 0.0)
	}

	// [38] price_trend_top1: cheapest pool price trend
	cheapest := combos[0].priceRatio
	c.cheapestPrices = pushTrimF(c.cheapestPrices, cheapest, rollingPriceLen)
	priceTrend := 0.5
	if len(c.cheapestPrices) >= 2 {
		old := c.cheapestPrices[0]
		cur := c.cheapestPrices[len(c.cheapestPrices)-1]
		if old > 1e-6 {
			t := math.Tanh((cur - old) / old)
			priceTrend = (t + 1.0) / 2.0
		}
	}
	feats = append(feats, clamp01F(priceTrend))

	// [39] cheaper_spot_available: có pool spot rẻ hơn 15% pool đang chạy không?
	cheaperAvail := float32(0)
	if runningPools > 0 {
		runAvg := avgRunningPrice / float64(runningPools)
		for _, p := range pools {
			if p.SpotPrice > 0 && p.SpotPrice < runAvg*0.85 {
				cheaperAvail = 1.0
				break
			}
		}
	}
	feats = append(feats, cheaperAvail)

	// [40] od_spot_price_gap
	odSpotGap := 0.0
	gapCnt := 0
	for _, p := range pools {
		if p.SpotPrice > 0 && p.OnDemandPrice > 0 {
			odSpotGap += (p.OnDemandPrice - p.SpotPrice) / p.OnDemandPrice
			gapCnt++
		}
	}
	if gapCnt > 0 {
		odSpotGap /= float64(gapCnt)
	}
	feats = append(feats, clamp01F(odSpotGap))

	// [41] sla_risk_score: pending high + interrupt rate high → risk
	slaRisk := 0.0
	if pendingJobs > 0 && totalVCPU > 0 {
		slaRisk = math.Min(float64(pendingJobs)/float64(totalVCPU+1), 1.0)
	}
	slaRisk = 0.7*slaRisk + 0.3*rate // weighted
	feats = append(feats, clamp01F(slaRisk))

	// 9) Per-pool CPU utilization (15)
	for ti := 0; ti < types.NTypes; ti++ {
		for ai := 0; ai < types.NAZs; ai++ {
			p := pools[ti*types.NAZs+ai]
			feats = append(feats, clamp01F(p.CPUUtil))
		}
	}

	// 10) Per-pool RAM utilization (15)
	for ti := 0; ti < types.NTypes; ti++ {
		for ai := 0; ai < types.NAZs; ai++ {
			p := pools[ti*types.NAZs+ai]
			feats = append(feats, clamp01F(p.RAMUtil))
		}
	}

	if len(feats) != types.StateDim {
		return types.State{}, fmt.Errorf("state vector length %d != %d", len(feats), types.StateDim)
	}

	// Tính workload trend để expose metrics
	wTrend := 0.0
	if len(c.pendingHistory) >= 2 {
		old := c.pendingHistory[0]
		cur := c.pendingHistory[len(c.pendingHistory)-1]
		wTrend = math.Tanh(float64(cur-old) / math.Max(float64(old), 1))
	}
	spotRatioM := 0.0
	if totalSpot+totalOD > 0 {
		spotRatioM = float64(totalSpot) / float64(totalSpot+totalOD)
	}
	budgetRatioM := 0.0
	if c.cumulativeBaselineOD > 1e-6 {
		budgetRatioM = c.totalCost / c.cumulativeBaselineOD
	}
	c.lastMetrics = StateMetrics{
		ForecastJobs:         forecast,
		BuildsLastHour:       buildsLastHour,
		WorkloadTrend:        wTrend,
		InterruptStreakRate:   rate,
		BudgetSpentRatio:     budgetRatioM,
		SpotRatio:            spotRatioM,
		AZSpread:             azSpread,
		CheaperSpotAvailable: float64(cheaperAvail),
		SLARiskScore:         slaRisk,
	}

	var s types.State
	copy(s.Vector[:], feats)
	s.NumSpot = totalSpot
	s.NumOnDemand = totalOD
	s.PendingJobs = pendingJobs
	s.RunningJobs = runningJobs
	s.HourlyCost = hourlyCost
	s.Timestamp = now
	return s, nil
}

// buildCombos build 15 pool entries với features cần cho top-3.
func buildCombos(pools [types.NPools]types.PoolInfo) []comboEntry {
	out := make([]comboEntry, 0, types.NPools)
	for ti := 0; ti < types.NTypes; ti++ {
		for ai := 0; ai < types.NAZs; ai++ {
			p := pools[ti*types.NAZs+ai]
			vcpuPerD := 0.0
			if p.SpotPrice > 0 {
				vcpuPerD = float64(p.VCPUPerInst) / p.SpotPrice
			}
			rebal := 0.0
			if p.RebalanceFlag {
				rebal = 1.0
			}
			term := 0.0
			if p.TerminationFlag {
				term = 1.0
			}
			out = append(out, comboEntry{
				typeIdx:         ti,
				azIdx:           ai,
				priceRatio:      priceRatio(p),
				interrupt:       p.InterruptProb,
				vcpuPerDollar:   vcpuPerD,
				baseRate:        p.BaseRate,
				baselineSavings: p.BaselineSavings,
				spsScore:        p.SPSScore,
				priceCV:         p.PriceCV24h,
				rebalance:       rebal,
				termination:     term,
			})
		}
	}
	return out
}

// priceRatio = spot_price / OD_price (clipped 0..2).
func priceRatio(p types.PoolInfo) float64 {
	if p.OnDemandPrice <= 0 {
		return 1.0
	}
	r := p.SpotPrice / p.OnDemandPrice
	if r < 0 {
		r = 0
	}
	if r > 2.0 {
		r = 2.0
	}
	return r
}

func clamp01F(v float64) float32 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return float32(v)
}

func argMin(s []float64) int {
	idx := 0
	for i := 1; i < len(s); i++ {
		if s[i] < s[idx] {
			idx = i
		}
	}
	return idx
}

func argMax(s []float64) int {
	idx := 0
	for i := 1; i < len(s); i++ {
		if s[i] > s[idx] {
			idx = i
		}
	}
	return idx
}

func pushTrim(buf []int, v, max int) []int {
	buf = append(buf, v)
	if len(buf) > max {
		buf = buf[len(buf)-max:]
	}
	return buf
}

func pushTrimF(buf []float64, v float64, max int) []float64 {
	buf = append(buf, v)
	if len(buf) > max {
		buf = buf[len(buf)-max:]
	}
	return buf
}
