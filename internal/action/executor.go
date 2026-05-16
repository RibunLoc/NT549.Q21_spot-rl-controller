// Decode action [0, 121) thành (op, instanceType, az) và execute via AWS API.
// Tất cả action đều phải pass qua safety layer trước khi tới đây.
package action

import (
	"context"
	"fmt"
	"log"
	"time"

	awsclient "spot-rl-controller/internal/aws"
	"spot-rl-controller/internal/jenkins"
	"spot-rl-controller/internal/registry"
	"spot-rl-controller/pkg/types"
)

// instanceTypes / azNames — mirror của pkg/types để build price map di rebalance.
var (
	rebalanceInstanceTypes = types.InstanceTypes
	rebalanceAZNames       = types.AZNames
)

// SCALE_STEP — số instance thay đổi mỗi action. Đặt 1 cho stable demo.
const SCALE_STEP = 1

// DrainTimeout — thời gian tối đa đợi Jenkins agent drain jobs xong khi
// controller chủ động RELEASE/CONVERT/REBALANCE. Đặt đủ lâu để build/test
// job CI/CD chạy xong (thông thường 5-30 phút), tránh kill job oan.
//
// Lưu ý: khi AWS chủ động interrupt Spot, chỉ có 2-min warning từ AWS — case
// đó được xử lý bởi handler riêng (interrupt listener), không qua hàm này.
const DrainTimeout = 30 * time.Minute

// Executor wrap EC2 client + Jenkins client + instance registry.
type Executor struct {
	ec2      *awsclient.EC2Client
	pricing  *awsclient.PricingClient
	jenkins  *jenkins.Client    // nil nếu không dùng Jenkins
	registry *registry.Registry // nil nếu không dùng registry
}

func NewExecutor(ec2 *awsclient.EC2Client, pricing *awsclient.PricingClient, j *jenkins.Client, reg *registry.Registry) *Executor {
	return &Executor{ec2: ec2, pricing: pricing, jenkins: j, registry: reg}
}

// Execute thực thi 1 action đã pass safety check.
//
// Flow:
//  1. Decode flat action → (op, type, az)
//  2. HOLD / no-op trên pending=0 → skip sớm
//  3. Switch EC2 client sang AZ tương ứng
//  4. Dispatch theo op
func (e *Executor) Execute(ctx context.Context, action types.Action, pendingJobs int) error {
	d := action.Decode()
	log.Printf("[action] action=%d op=%s type=%s az=%s",
		int(action), d.OpName, d.InstanceType, d.AZ)

	// HOLD: no-op (nhưng vẫn log).
	if d.Op == types.OpHold {
		log.Printf("[action] HOLD")
		return nil
	}

	// Guard: không tạo capacity khi không có job pending.
	// PROVISION_SPOT, PROVISION_ONDEMAND, RESERVE_CAPACITY đều tạo instance mới.
	isCreate := d.Op == types.OpProvisionSpot ||
		d.Op == types.OpProvisionOnDemand ||
		d.Op == types.OpReserveCapacity
	if pendingJobs == 0 && isCreate {
		log.Printf("[action] skipped %s — no pending jobs", d.OpName)
		return nil
	}

	e.ec2.SetInstanceType(d.InstanceType)
	if err := e.ec2.SetAZ(d.AZ); err != nil {
		return fmt.Errorf("set AZ %s: %w", d.AZ, err)
	}

	switch d.Op {
	case types.OpProvisionSpot:
		return wrap(d.OpName, e.ec2.RequestSpot(ctx, SCALE_STEP))

	case types.OpProvisionOnDemand:
		return wrap(d.OpName, e.ec2.RequestOnDemand(ctx, SCALE_STEP))

	case types.OpReleaseSpot:
		return e.drainAndRelease(ctx, "spot", d.InstanceType, d.AZ)

	case types.OpReleaseOnDemand:
		return e.drainAndRelease(ctx, "on-demand", d.InstanceType, d.AZ)

	case types.OpConvertToOnDemand:
		// Spot → OD: drain + terminate spot trước, request OD sau (cùng pool).
		if err := e.drainAndRelease(ctx, "spot", d.InstanceType, d.AZ); err != nil {
			return fmt.Errorf("%s: %w", d.OpName, err)
		}
		return wrap(d.OpName, e.ec2.RequestOnDemand(ctx, SCALE_STEP))

	case types.OpConvertToSpot:
		// OD → Spot: drain + terminate OD trước, request spot sau (cùng pool).
		if err := e.drainAndRelease(ctx, "on-demand", d.InstanceType, d.AZ); err != nil {
			return fmt.Errorf("%s: %w", d.OpName, err)
		}
		return wrap(d.OpName, e.ec2.RequestSpot(ctx, SCALE_STEP))

	case types.OpRebalanceSpot:
		// Migrate Spot: provision pool mới → drain Jenkins agent cũ → terminate cũ.
		// Thứ tự quan trọng: KHÔNG terminate trước khi agent drain xong
		// vì sẽ kill jobs đang chạy → SLA violation.
		return e.rebalanceSpot(ctx, d)

	case types.OpReserveCapacity:
		// Pre-allocate OD khi interrupt risk cao — gọi RequestOnDemand.
		return wrap(d.OpName, e.ec2.RequestOnDemand(ctx, SCALE_STEP))

	default:
		return fmt.Errorf("unknown op: %d", d.Op)
	}
}

// drainAndRelease drain Jenkins agent tại pool (instanceType, az) rồi terminate instance đó.
// Đây là quy trình chuẩn cho RELEASE_SPOT, RELEASE_ONDEMAND, CONVERT ops:
//  1. Sync registry → tìm agent cũ nhất trong pool
//  2. Drain agent (mark offline + đợi idle, tối đa DrainTimeout)
//  3. TerminateByPool — terminate đúng instance đó
//  4. DeleteAgent — xóa node khỏi Jenkins UI
func (e *Executor) drainAndRelease(ctx context.Context, kind, instanceType, az string) error {
	log.Printf("[release] drain+terminate %s in %s/%s", kind, instanceType, az)

	// Tìm agent cũ nhất trong pool để drain đúng worker
	var agentName string
	if e.registry != nil && e.jenkins != nil {
		if err := e.registry.Sync(ctx); err != nil {
			log.Printf("[release] registry sync warn: %v", err)
		}
		_, agentName = e.registry.OldestAgentInPool(instanceType, az)
	}

	// Drain trước khi terminate — tránh kill jobs đang chạy
	if e.jenkins != nil && agentName != "" {
		log.Printf("[release] draining agent %s (timeout=%s)", agentName, DrainTimeout)
		if err := e.jenkins.DrainAgent(ctx, agentName, DrainTimeout); err != nil {
			// Timeout vẫn tiếp tục terminate — log warning
			log.Printf("[release] drain warning: %v — proceeding with terminate", err)
		} else {
			log.Printf("[release] agent %s drained OK", agentName)
		}
	} else {
		log.Printf("[release] skip drain (jenkins=%v agent=%q)", e.jenkins != nil, agentName)
	}

	// Terminate đúng pool
	if err := e.ec2.TerminateByPool(ctx, kind, instanceType, az, SCALE_STEP); err != nil {
		return fmt.Errorf("drainAndRelease terminate: %w", err)
	}

	// Xóa node khỏi Jenkins UI
	if e.jenkins != nil && agentName != "" {
		if err := e.jenkins.DeleteAgent(ctx, agentName); err != nil {
			log.Printf("[release] delete agent %s: %v", agentName, err)
		}
	}

	log.Printf("[release] done — %s/%s %s released", instanceType, az, kind)
	return nil
}

// rebalanceSpot thực hiện migrate Spot từ pool đắt nhất → pool đích (type+az trong action).
// Khớp với train: pools[src].spot_count -= 1, pools[dst].spot_count += 1.
//
//	Bước 1: Build price map → tìm src pool có giá cao nhất (không phải dst)
//	Bước 2: Provision Spot mới ở pool đích
//	Bước 3: Drain Jenkins agent ở src pool (đợi jobs finish, tối đa DrainTimeout)
//	Bước 4: Terminate Spot ở đúng src pool
func (e *Executor) rebalanceSpot(ctx context.Context, d types.DecodedAction) error {
	log.Printf("[rebalance] migrate spot → dst=%s/%s", d.InstanceType, d.AZ)

	// ── Bước 1: Tìm src pool (đắt nhất, không phải dst) ─
	srcType, srcAZ, srcAgent := e.findMostExpensiveSrcPool(ctx, d.InstanceType, d.AZ)
	if srcType == "" {
		// Không có pool khác → fallback: terminate cùng pool dst (scale-in thay migrate)
		log.Printf("[rebalance] no src pool found — fallback to scale-in at dst pool")
		srcType, srcAZ = d.InstanceType, d.AZ
	}
	log.Printf("[rebalance] src pool: %s/%s agent=%s", srcType, srcAZ, srcAgent)

	// ── Bước 2: Snapshot agents online trước khi provision ─
	var agentsBefore map[string]struct{}
	if e.jenkins != nil {
		snap, err := e.jenkins.ListOnlineAgentNames(ctx)
		if err != nil {
			log.Printf("[rebalance] step2: snapshot agents warn: %v", err)
		} else {
			agentsBefore = snap
		}
	}

	// ── Bước 3: Provision spot mới ở pool đích ──────────
	log.Printf("[rebalance] step3: provision new spot at %s/%s", d.InstanceType, d.AZ)
	if err := e.ec2.RequestSpot(ctx, SCALE_STEP); err != nil {
		return fmt.Errorf("rebalance provision: %w", err)
	}
	log.Printf("[rebalance] step3: done — new spot provisioned")

	// ── Bước 4: Đợi dst agent online trước khi drain src ─
	// Instance mới cần ~2-3 phút boot + join Jenkins.
	// Không drain src trước khi dst online → tránh khoảng trống không có worker → SLA drop.
	if e.jenkins != nil && agentsBefore != nil {
		log.Printf("[rebalance] step4: waiting for new dst agent to come online (timeout=5m)")
		newAgent, err := e.jenkins.WaitAgentOnline(ctx, agentsBefore, 5*time.Minute)
		if err != nil {
			// Timeout → vẫn tiếp tục drain nhưng log warning
			log.Printf("[rebalance] step4: wait agent warn: %v — proceeding anyway", err)
		} else {
			log.Printf("[rebalance] step4: dst agent %q online OK", newAgent)
		}
	}

	// ── Bước 5: Drain Jenkins agent ở src pool ──────────
	if e.jenkins != nil && srcAgent != "" {
		log.Printf("[rebalance] step5: draining agent %s (timeout=%s)", srcAgent, DrainTimeout)
		if err := e.jenkins.DrainAgent(ctx, srcAgent, DrainTimeout); err != nil {
			log.Printf("[rebalance] step5: drain warning: %v — proceeding with terminate", err)
		} else {
			log.Printf("[rebalance] step5: agent %s drained OK", srcAgent)
		}
	} else {
		log.Printf("[rebalance] step5: skip drain (jenkins=%v agent=%q)", e.jenkins != nil, srcAgent)
	}

	// ── Bước 6: Terminate spot ở đúng src pool ──────────
	log.Printf("[rebalance] step6: terminate spot at src %s/%s", srcType, srcAZ)
	if err := e.ec2.TerminateByPool(ctx, "spot", srcType, srcAZ, SCALE_STEP); err != nil {
		return fmt.Errorf("rebalance terminate: %w", err)
	}
	log.Printf("[rebalance] step6: done — migrate complete")

	// Cleanup: xóa node khỏi Jenkins UI
	if e.jenkins != nil && srcAgent != "" {
		if err := e.jenkins.DeleteAgent(ctx, srcAgent); err != nil {
			log.Printf("[rebalance] cleanup: delete agent %s: %v", srcAgent, err)
		}
		if e.registry != nil {
			// Lấy instance ID từ registry trước khi xóa
			// (agentName đã biết, tìm ngược để remove)
			if err := e.registry.Sync(ctx); err == nil {
				// Registry.Remove cần instance ID — bỏ qua nếu không tìm được,
				// Sync lần sau sẽ tự clean up instance đã terminate.
			}
		}
	}

	return nil
}

// findMostExpensiveSrcPool tìm pool có spot price cao nhất (không phải dst pool).
// Build price map từ PricingClient → gọi registry.MostExpensiveSpotPool.
func (e *Executor) findMostExpensiveSrcPool(ctx context.Context, dstType, dstAZ string) (instanceType, az, agentName string) {
	if e.registry == nil || e.pricing == nil {
		return "", "", ""
	}
	if err := e.registry.Sync(ctx); err != nil {
		log.Printf("[executor] registry sync warn: %v", err)
	}

	// Build spot price map cho tất cả pools đang có instance
	prices := make(map[string]float64)
	for _, t := range rebalanceInstanceTypes {
		for _, a := range rebalanceAZNames {
			p, err := e.pricing.GetSpotPrice(ctx, t, a)
			if err == nil {
				prices[t+"/"+a] = p
			}
		}
	}

	return e.registry.MostExpensiveSpotPool(dstType, dstAZ, prices)
}

func wrap(opName string, err error) error {
	if err != nil {
		return fmt.Errorf("%s failed: %w", opName, err)
	}
	return nil
}
