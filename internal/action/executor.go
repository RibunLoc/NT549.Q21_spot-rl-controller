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

// SCALE_STEP — số instance thay đổi mỗi action. Đặt 1 cho stable demo.
const SCALE_STEP = 1

// DrainTimeout — thời gian tối đa đợi Jenkins agent drain jobs xong.
// 2-min AWS interrupt warning → đặt 90s để còn buffer terminate instance.
const DrainTimeout = 90 * time.Second

// Executor wrap EC2 client + Jenkins client + instance registry.
type Executor struct {
	ec2      *awsclient.EC2Client
	jenkins  *jenkins.Client   // nil nếu không dùng Jenkins
	registry *registry.Registry // nil nếu không dùng registry
}

func NewExecutor(ec2 *awsclient.EC2Client, j *jenkins.Client, reg *registry.Registry) *Executor {
	return &Executor{ec2: ec2, jenkins: j, registry: reg}
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

	if err := e.ec2.SetAZ(d.AZ); err != nil {
		return fmt.Errorf("set AZ %s: %w", d.AZ, err)
	}

	switch d.Op {
	case types.OpProvisionSpot:
		return wrap(d.OpName, e.ec2.RequestSpot(ctx, SCALE_STEP))

	case types.OpProvisionOnDemand:
		return wrap(d.OpName, e.ec2.RequestOnDemand(ctx, SCALE_STEP))

	case types.OpReleaseSpot:
		return wrap(d.OpName, e.ec2.TerminateByKind(ctx, "spot", SCALE_STEP))

	case types.OpReleaseOnDemand:
		return wrap(d.OpName, e.ec2.TerminateByKind(ctx, "on-demand", SCALE_STEP))

	case types.OpConvertToOnDemand:
		// Spot → OD: terminate spot trước, request OD sau (cùng AZ).
		if err := e.ec2.TerminateByKind(ctx, "spot", SCALE_STEP); err != nil {
			return fmt.Errorf("%s terminate spot: %w", d.OpName, err)
		}
		return wrap(d.OpName, e.ec2.RequestOnDemand(ctx, SCALE_STEP))

	case types.OpConvertToSpot:
		// OD → Spot: terminate OD trước, request spot sau (cùng AZ).
		if err := e.ec2.TerminateByKind(ctx, "on-demand", SCALE_STEP); err != nil {
			return fmt.Errorf("%s terminate OD: %w", d.OpName, err)
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

// rebalanceSpot thực hiện migrate Spot sang pool rẻ hơn theo 3 bước:
//
//	Bước 1: Provision Spot mới ở pool đích (AZ trong action)
//	Bước 2: Drain Jenkins agent cũ (đợi jobs finish, tối đa DrainTimeout)
//	Bước 3: Terminate Spot cũ sau khi drain xong
//
// Nếu không có Jenkins client → fallback terminate-then-provision (không drain).
func (e *Executor) rebalanceSpot(ctx context.Context, d types.DecodedAction) error {
	log.Printf("[rebalance] migrate spot → %s/%s", d.InstanceType, d.AZ)

	// ── Bước 1: Provision spot mới ở pool đích ──────────
	log.Printf("[rebalance] step1: provision new spot at %s", d.AZ)
	if err := e.ec2.RequestSpot(ctx, SCALE_STEP); err != nil {
		return fmt.Errorf("rebalance provision: %w", err)
	}
	log.Printf("[rebalance] step1: done — new spot provisioned")

	// ── Bước 2: Drain Jenkins agent cũ ──────────────────
	if e.jenkins != nil {
		// Tìm agent đang chạy ở AZ cũ (agent name = instance ID, tag bởi UserData)
		// Trong demo: agent name = hostname của instance cũ
		// Controller biết tên agent vì đã track khi provision
		agentName, err := e.findOldestSpotAgentExcluding(ctx, d.AZ)
		if err != nil {
			log.Printf("[rebalance] step2: cannot find old agent: %v — skip drain", err)
		} else if agentName != "" {
			log.Printf("[rebalance] step2: draining agent %s (timeout=%s)", agentName, DrainTimeout)
			if err := e.jenkins.DrainAgent(ctx, agentName, DrainTimeout); err != nil {
				// Drain timeout → vẫn terminate nhưng log warning
				log.Printf("[rebalance] step2: drain warning: %v — proceeding with terminate", err)
			} else {
				log.Printf("[rebalance] step2: agent %s drained successfully", agentName)
			}
		}
	} else {
		log.Printf("[rebalance] step2: no Jenkins client — skipping drain")
	}

	// ── Bước 3: Terminate spot cũ ────────────────────────
	// SetAZ lại về AZ nguồn để terminate đúng pool
	// Hiện tại: terminate 1 spot ở AZ hiện tại (đã set ở Execute trước khi gọi)
	log.Printf("[rebalance] step3: terminate old spot")
	if err := e.ec2.TerminateByKind(ctx, "spot", SCALE_STEP); err != nil {
		return fmt.Errorf("rebalance terminate: %w", err)
	}
	log.Printf("[rebalance] step3: done — migrate complete")

	// Cleanup: xóa agent node cũ khỏi Jenkins
	if e.jenkins != nil {
		agentName, _ := e.findOldestSpotAgentExcluding(ctx, d.AZ)
		if agentName != "" {
			if err := e.jenkins.DeleteAgent(ctx, agentName); err != nil {
				log.Printf("[rebalance] cleanup: delete agent %s: %v", agentName, err)
			}
		}
	}

	return nil
}

// findOldestSpotAgentExcluding tìm Jenkins agent name của Spot instance
// cũ nhất KHÔNG thuộc targetAZ (đó là instance cần migrate ra).
// Dùng InstanceRegistry được sync từ EC2 tags (JenkinsAgentName).
func (e *Executor) findOldestSpotAgentExcluding(ctx context.Context, targetAZ string) (string, error) {
	if e.registry == nil {
		return "", nil
	}
	// Sync registry để có data mới nhất trước khi chọn target
	if err := e.registry.Sync(ctx); err != nil {
		log.Printf("[executor] registry sync warn: %v", err)
	}
	_, agentName := e.registry.OldestSpotAgentExcludingAZ(targetAZ)
	return agentName, nil
}

func wrap(opName string, err error) error {
	if err != nil {
		return fmt.Errorf("%s failed: %w", opName, err)
	}
	return nil
}
