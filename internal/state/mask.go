// Action masking — port từ envs/spot_orchestrator_env.py:get_action_mask()
// Đảm bảo controller chỉ chọn actions hợp lệ với state hiện tại.
package state

import (
	"spot-rl-controller/pkg/types"
)

const (
	maxInstancesLimit = 20
	maxPerAZLimit     = 10
)

// BuildActionMask trả về mask [121] — true = action hợp lệ.
// Logic khớp env Python:
//   PROVISION_SPOT/OD: pool full HOẶC capacity saturated
//   RELEASE_SPOT/OD:   count tương ứng = 0
//   CONVERT_TO_OD:     pool no spot
//   CONVERT_TO_SPOT:   pool no OD HOẶC spot_price >= od_price
//   REBALANCE_SPOT:    pool no spot HOẶC at_capacity
//   RESERVE_CAPACITY:  at_capacity HOẶC no interrupt/SLA risk
//   HOLD:              luôn hợp lệ
func BuildActionMask(
	pools [types.NPools]types.PoolInfo,
	pendingJobs, runningJobs int,
	forecastJobs float64,
) [types.ActionDim]bool {
	var mask [types.ActionDim]bool
	for i := range mask {
		mask[i] = true
	}

	totalInstances := 0
	totalVCPU := 0
	azCounts := [types.NAZs]int{}
	for _, p := range pools {
		totalInstances += p.SpotCount + p.OnDemandCount
		totalVCPU += (p.SpotCount + p.OnDemandCount) * p.VCPUPerInst
		azCounts[p.AZIdx] += p.SpotCount + p.OnDemandCount
	}
	if totalVCPU < 1 {
		totalVCPU = 1
	}

	needed := pendingJobs + runningJobs
	// Capacity guard: block PROVISION khi không có nhu cầu thực sự.
	// Dùng forecast để cho phép pre-warm khi spike sắp đến.
	// - needed == 0 AND forecast < 5: không có job hiện tại và không dự báo spike
	// - totalVCPU đủ 1.5x needed: đã over-provisioned
	// - totalInstances >= 3 và không có job: fleet thừa
	noCurrentDemand := needed == 0 && forecastJobs < 5.0
	capacitySaturated := noCurrentDemand ||
		(totalVCPU >= int(float64(needed)*1.5) && needed > 0 && totalInstances > 0) ||
		(totalInstances >= 3 && needed == 0)

	for ti := 0; ti < types.NTypes; ti++ {
		odPrice := types.OnDemandPrices[ti]
		for ai := 0; ai < types.NAZs; ai++ {
			pool := pools[ti*types.NAZs+ai]
			poolFull := totalInstances >= maxInstancesLimit || azCounts[ai] >= maxPerAZLimit

			// PROVISION
			if poolFull || capacitySaturated {
				mask[encodeAction(types.OpProvisionSpot, ti, ai)] = false
				mask[encodeAction(types.OpProvisionOnDemand, ti, ai)] = false
			}

			// RELEASE
			vcpuPerInst := types.InstanceVCPUs[ti]
			capacityAfterRelease := totalVCPU - vcpuPerInst
			if pool.SpotCount == 0 || capacityAfterRelease < runningJobs {
				mask[encodeAction(types.OpReleaseSpot, ti, ai)] = false
			}
			if pool.OnDemandCount == 0 || capacityAfterRelease < runningJobs {
				mask[encodeAction(types.OpReleaseOnDemand, ti, ai)] = false
			}

			// CONVERT_TO_OD: cần có spot
			if pool.SpotCount == 0 {
				mask[encodeAction(types.OpConvertToOnDemand, ti, ai)] = false
			}

			// CONVERT_TO_SPOT: cần có OD và spot rẻ hơn OD
			if pool.OnDemandCount == 0 || pool.SpotPrice >= odPrice {
				mask[encodeAction(types.OpConvertToSpot, ti, ai)] = false
			}

			// REBALANCE_SPOT: cần có spot ở pool này (source) và không at_capacity
			if pool.SpotCount == 0 || poolFull {
				mask[encodeAction(types.OpRebalanceSpot, ti, ai)] = false
			}

			// RESERVE_CAPACITY: block khi at_capacity hoặc không có interrupt risk
			if poolFull || pool.InterruptProb < 0.3 {
				mask[encodeAction(types.OpReserveCapacity, ti, ai)] = false
			}
		}
	}

	// HOLD luôn hợp lệ — đảm bảo không bao giờ all-False
	mask[types.HoldAction] = true
	return mask
}

func encodeAction(op, typeIdx, azIdx int) int {
	if op == types.OpHold {
		return types.HoldAction
	}
	return op*types.NPools + typeIdx*types.NAZs + azIdx
}
