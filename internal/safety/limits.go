// Safety guardrails — hard limits độc lập với agent.
// Mọi action phải pass qua Allow() trước khi tới executor.
// Nếu có gì sai (agent bug, model lỗi, AWS spike), layer này chặn thiệt hại.
package safety

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"
	"time"

	"spot-rl-controller/pkg/types"
)

// Defaults phù hợp demo seminar (~$50-100/tháng budget).
type Limits struct {
	MaxFleetSize       int           // hard cap tổng instances
	MaxActionsPerHour  int           // rate limit
	MaxHourlyCost      float64       // circuit breaker $/hour
	MinDecisionInterval time.Duration // tối thiểu giữa 2 actions
	KillSwitchPath     string        // file flag → disable agent
}

func DefaultLimits() Limits {
	minInterval := 5 * time.Minute
	if v := os.Getenv("SAFETY_MIN_INTERVAL_SEC"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			minInterval = time.Duration(secs) * time.Second
		}
	}

	maxFleet := 10
	if v := os.Getenv("SAFETY_MAX_FLEET"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxFleet = n
		}
	}

	maxCost := 5.0
	if v := os.Getenv("SAFETY_MAX_HOURLY"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			maxCost = f
		}
	}

	return Limits{
		MaxFleetSize:        maxFleet,
		MaxActionsPerHour:   30,
		MaxHourlyCost:       maxCost,
		MinDecisionInterval: minInterval,
		KillSwitchPath:      "/etc/spot-rl/killswitch",
	}
}

// Result của 1 safety check.
type Result struct {
	Allowed bool
	Reason  string // empty nếu allowed
}

// Guard tracks rate + cost windows.
type Guard struct {
	limits Limits

	mu             sync.Mutex
	actionsInWindow []time.Time // timestamps of actions trong 1h window
	lastActionAt   time.Time
}

func New(limits Limits) *Guard {
	return &Guard{limits: limits}
}

// Allow kiểm tra action có được phép thực thi không.
//
// fleetSize: tổng instances hiện tại (spot + on-demand)
// hourlyCost: $/hour hiện tại của fleet
//
// Trả Result{Allowed:false, Reason:...} nếu vi phạm 1 trong 4 limit.
func (g *Guard) Allow(action types.Action, fleetSize int, hourlyCost float64) Result {
	g.mu.Lock()
	defer g.mu.Unlock()

	// 1) Kill switch — file flag tồn tại → block all
	if g.killSwitchActive() {
		return Result{false, "kill switch active (" + g.limits.KillSwitchPath + ")"}
	}

	// 2) Cost circuit breaker — chặn mọi PROVISION nếu cost đã vượt cap
	if hourlyCost >= g.limits.MaxHourlyCost {
		if isCreate(action) {
			return Result{false, fmt.Sprintf("hourly cost $%.2f >= cap $%.2f",
				hourlyCost, g.limits.MaxHourlyCost)}
		}
	}

	// 3) Fleet size cap — chặn PROVISION nếu fleet đã max
	if isCreate(action) && fleetSize >= g.limits.MaxFleetSize {
		return Result{false, fmt.Sprintf("fleet size %d >= cap %d",
			fleetSize, g.limits.MaxFleetSize)}
	}

	// 4) Rate limit — drop expired entries, check count
	now := time.Now()
	cutoff := now.Add(-time.Hour)
	g.actionsInWindow = filterAfter(g.actionsInWindow, cutoff)
	if len(g.actionsInWindow) >= g.limits.MaxActionsPerHour {
		return Result{false, fmt.Sprintf("rate limit: %d actions in last hour >= cap %d",
			len(g.actionsInWindow), g.limits.MaxActionsPerHour)}
	}

	// 5) Min interval — chặn action quá gần lần trước
	if !g.lastActionAt.IsZero() && now.Sub(g.lastActionAt) < g.limits.MinDecisionInterval {
		return Result{false, fmt.Sprintf("min interval: only %.1fs since last action",
			now.Sub(g.lastActionAt).Seconds())}
	}

	return Result{Allowed: true}
}

// MarkExecuted ghi nhận action đã thực thi — gọi từ controller sau khi executor chạy xong.
func (g *Guard) MarkExecuted(action types.Action) {
	g.mu.Lock()
	defer g.mu.Unlock()

	d := action.Decode()
	// HOLD không tốn rate budget
	if d.Op == types.OpHold {
		return
	}

	now := time.Now()
	g.actionsInWindow = append(g.actionsInWindow, now)
	g.lastActionAt = now
	log.Printf("[safety] action %s executed (window: %d/%d)",
		d.OpName, len(g.actionsInWindow), g.limits.MaxActionsPerHour)
}

// killSwitchActive — file tồn tại → kill switch.
func (g *Guard) killSwitchActive() bool {
	if g.limits.KillSwitchPath == "" {
		return false
	}
	_, err := os.Stat(g.limits.KillSwitchPath)
	return err == nil
}

// isCreate xác định op có tạo capacity mới không.
func isCreate(action types.Action) bool {
	d := action.Decode()
	return d.Op == types.OpProvisionSpot ||
		d.Op == types.OpProvisionOnDemand ||
		d.Op == types.OpReserveCapacity
}

func filterAfter(ts []time.Time, cutoff time.Time) []time.Time {
	out := ts[:0]
	for _, t := range ts {
		if t.After(cutoff) {
			out = append(out, t)
		}
	}
	return out
}
