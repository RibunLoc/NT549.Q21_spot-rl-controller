// Định nghĩa các struct/type dùng chung — đồng bộ với Python env (v14).
// State: 90 features, Action: 121 (8 ops × 15 pools + HOLD).
package types

import "time"

// Dimensions — khớp với envs/instance_catalog.py + envs/action_schema.py
const (
	NTypes = 5
	NAZs   = 3

	NPoolOps      = 8                     // 8 pool-targeted ops (excl. HOLD)
	NOps          = 9                     // PROVISION_SPOT...HOLD
	NPools        = NTypes * NAZs         // 15
	NPoolActions  = NPoolOps * NPools     // 120
	HoldAction    = NPoolActions          // 120 (flat index of HOLD)
	ActionDim     = NPoolActions + 1      // 121
	NTopK         = 3                     // top-3 cheapest combos
	NTopKFeatures = 10                    // 10 features per combo
	NExtraCtx     = 9                     // extra context features [33-41]
	NUtilFeatures = NPools                // CPU + RAM = 15 each
	StateDim      = NTopK*NTopKFeatures + // 30
		3 + // Multi-AZ
		3 + // Multi-Type
		3 + // Infrastructure
		6 + // Workload (pending, running, forecast, queue_wait, avg_cpu_demand, avg_ram_demand)
		3 + // Time
		5 + // Current
		NExtraCtx + // 9
		2*NUtilFeatures // 30 (CPU + RAM)
		// Total: 30 + 3 + 3 + 3 + 6 + 3 + 5 + 9 + 30 = 92
)

// Instance type names — khớp INSTANCE_TYPES Python.
var InstanceTypes = [NTypes]string{
	"m5.large",
	"c5.xlarge",
	"r5.large",
	"m5.xlarge",
	"c5.2xlarge",
}

// VCPU per instance type — khớp catalog.
var InstanceVCPUs = [NTypes]int{2, 4, 2, 4, 8}

// On-demand price per type ($/hour, ap-southeast-1).
var OnDemandPrices = [NTypes]float64{0.12, 0.196, 0.152, 0.24, 0.392}

// AZ names — adjust theo region triển khai. Default ap-southeast-1.
var AZNames = [NAZs]string{
	"ap-southeast-1a",
	"ap-southeast-1b",
	"ap-southeast-1c",
}

// Op constants — khớp Operation enum trong action_schema.py.
const (
	OpProvisionSpot     = 0
	OpProvisionOnDemand = 1
	OpReleaseSpot       = 2
	OpReleaseOnDemand   = 3
	OpConvertToOnDemand = 4
	OpConvertToSpot     = 5
	OpRebalanceSpot     = 6
	OpReserveCapacity   = 7
	OpHold              = 8
)

var OpNames = [NOps]string{
	"PROVISION_SPOT",
	"PROVISION_ONDEMAND",
	"RELEASE_SPOT",
	"RELEASE_ONDEMAND",
	"CONVERT_TO_ONDEMAND",
	"CONVERT_TO_SPOT",
	"REBALANCE_SPOT",
	"RESERVE_CAPACITY",
	"HOLD",
}

// DecodeAction giải mã flat action [0, 121) → (op, typeIdx, azIdx).
// Khớp envs/action_schema.py decode_action():
//
//	if action == HOLD_ACTION (120): return HOLD, 0, 0
//	op_idx   = action / N_POOLS         (15)
//	pool_idx = action % N_POOLS
//	type_idx = pool_idx / N_AZS         (3)
//	az_idx   = pool_idx % N_AZS
func DecodeAction(action int) (op, typeIdx, azIdx int) {
	if action == HoldAction {
		return OpHold, 0, 0
	}
	if action < 0 || action >= NPoolActions {
		// invalid — caller phải guard. Trả HOLD để safe default.
		return OpHold, 0, 0
	}
	op = action / NPools
	pool := action % NPools
	typeIdx = pool / NAZs
	azIdx = pool % NAZs
	return
}

// Action là composite action 0..120.
type Action int

// DecodedAction là kết quả decode.
type DecodedAction struct {
	Op           int
	TypeIdx      int
	AZIdx        int
	OpName       string
	InstanceType string
	AZ           string
}

// Decode giải mã Action.
func (a Action) Decode() DecodedAction {
	op, typeIdx, azIdx := DecodeAction(int(a))
	d := DecodedAction{
		Op:     op,
		OpName: OpNames[op],
	}
	if op == OpHold {
		d.InstanceType = "-"
		d.AZ = "-"
	} else {
		d.TypeIdx = typeIdx
		d.AZIdx = azIdx
		d.InstanceType = InstanceTypes[typeIdx]
		d.AZ = AZNames[azIdx]
	}
	return d
}

// PoolKey định danh 1 pool (typeIdx × azIdx).
type PoolKey struct {
	TypeIdx int
	AZIdx   int
}

// PoolInfo dữ liệu thu thập 1 pool — input cho state collector.
type PoolInfo struct {
	TypeIdx       int
	AZIdx         int
	SpotPrice     float64 // $/hour
	OnDemandPrice float64 // $/hour
	InterruptProb float64 // [0, 1]
	SpotCount     int
	OnDemandCount int
	VCPUPerInst   int

	// Two-signal architecture (v14)
	BaseRate        float64 // static base interrupt rate
	BaselineSavings float64 // static — % savings vs OD theo Spot Advisor
	SPSScore        float64 // dynamic Spot Placement Score [0, 1]
	PriceCV24h      float64 // dynamic — price coefficient of variation 24h
	RebalanceFlag   bool    // event — rebalance recommendation
	TerminationFlag bool    // event — termination notice 2-min warning
	CPUUtil         float64 // [0, 1]
	RAMUtil         float64 // [0, 1]
}

// State chứa raw vector 90 chiều — feed thẳng vào ONNX model.
// Thứ tự khớp _get_observation() trong spot_orchestrator_env.py.
type State struct {
	Vector [StateDim]float32
	// Metadata cho logging — không feed vào model
	NumSpot     int
	NumOnDemand int
	PendingJobs int
	RunningJobs int
	HourlyCost  float64
	Timestamp   time.Time
}

// ToSlice trả về slice để truyền vào ONNX runtime.
func (s State) ToSlice() []float32 {
	out := make([]float32, StateDim)
	copy(out, s.Vector[:])
	return out
}

// Decision là output của 1 inference cycle.
type Decision struct {
	Action     Action
	Decoded    DecodedAction
	QValues    [ActionDim]float32
	MaxQ       float32 // Q của action được chọn
	Confidence float32 // softmax(Q)[action] — proxy cho certainty
	State      State
	Timestamp  time.Time
}

// Metrics cho dashboard / Prometheus.
type Metrics struct {
	Action            Action
	DecodedAction     DecodedAction
	SpotInstances     int
	OnDemandInstances int
	EstimatedCost     float64
	Allowed           bool   // safety layer cho phép?
	BlockedReason     string // nếu Allowed=false
	Timestamp         time.Time
}
