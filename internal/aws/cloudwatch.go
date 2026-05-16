package aws

import (
	"context"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
)

// PoolUtil holds CPU and RAM utilization for one (instanceType, AZ) pool.
// HasData = false khi CloudWatch chưa có datapoint nào (vd: instance vừa tạo
// chưa đủ 5 phút). Caller phải dùng default thay vì coi như util = 0.
type PoolUtil struct {
	CPUUtil    float64 // 0..1
	RAMUtil    float64 // 0..1
	HasCPUData bool
	HasRAMData bool
}

// CloudWatchClient fetches per-pool CPU and RAM utilization.
type CloudWatchClient struct {
	cw *cloudwatch.Client
}

func NewCloudWatchClient(cfg aws.Config) *CloudWatchClient {
	return &CloudWatchClient{cw: cloudwatch.NewFromConfig(cfg)}
}

// GetPoolUtilization queries CloudWatch for avg CPUUtilization and mem_used_percent
// for all running instances tagged with ManagedBy=spot-rl-controller,
// grouped by instance_type and availability_zone.
//
// Returns a map keyed by "instanceType/az" → PoolUtil.
// On any error the map will be empty and the caller should fall back to defaults.
func (c *CloudWatchClient) GetPoolUtilization(
	ctx context.Context,
	instanceTypes []string,
	azNames []string,
	lookbackMinutes int,
) map[string]PoolUtil {
	result := make(map[string]PoolUtil, len(instanceTypes)*len(azNames))

	end := time.Now()
	start := end.Add(-time.Duration(lookbackMinutes) * time.Minute)
	period := int32(lookbackMinutes * 60)

	for _, itype := range instanceTypes {
		for _, az := range azNames {
			key := itype + "/" + az
			// Khớp với config CloudWatch Agent trong Terraform/user_data/jenkins_agent.sh:
			//   cpu_usage_active (totalcpu=true) và mem_used_percent
			// Cả hai đều push lên namespace CWAgent với interval 60s.
			cpu, cpuOK := c.queryMetricRaw(ctx, itype, az, "cpu_usage_active", start, end, period)
			ram, ramOK := c.queryMetricRaw(ctx, itype, az, "mem_used_percent", start, end, period)
			result[key] = PoolUtil{
				CPUUtil:    cpu / 100.0,
				RAMUtil:    ram / 100.0,
				HasCPUData: cpuOK,
				HasRAMData: ramOK,
			}
		}
	}
	return result
}

// queryMetricRaw returns (avgValue, ok) of a CloudWatch metric for the
// given (instanceType, az) dimension over [start, end] with the specified period.
// Returns (0, false) khi metric chưa có datapoint nào (vd: instance mới tạo).
func (c *CloudWatchClient) queryMetricRaw(
	ctx context.Context,
	instanceType, az, metricName string,
	start, end time.Time,
	period int32,
) (float64, bool) {
	// Tất cả metric custom đều push qua CloudWatch Agent (namespace CWAgent)
	// với interval 60s. Đặc biệt cpu_usage_active (thay cho EC2 default
	// CPUUtilization vốn chỉ có datapoint mỗi 5 phút ở basic monitoring,
	// gây lag khi instance mới tạo).
	namespace := "CWAgent"

	out, err := c.cw.GetMetricStatistics(ctx, &cloudwatch.GetMetricStatisticsInput{
		Namespace:  aws.String(namespace),
		MetricName: aws.String(metricName),
		StartTime:  aws.Time(start),
		EndTime:    aws.Time(end),
		Period:     aws.Int32(period),
		Statistics: []cwtypes.Statistic{cwtypes.StatisticAverage},
		Dimensions: []cwtypes.Dimension{
			{Name: aws.String("InstanceType"), Value: aws.String(instanceType)},
			{Name: aws.String("AvailabilityZone"), Value: aws.String(az)},
		},
	})
	if err != nil {
		log.Printf("[cloudwatch] %s/%s %s: %v", instanceType, az, metricName, err)
		return 0, false
	}
	if len(out.Datapoints) == 0 {
		return 0, false
	}
	// Take the latest datapoint
	latest := out.Datapoints[0]
	for _, dp := range out.Datapoints[1:] {
		if dp.Timestamp.After(*latest.Timestamp) {
			latest = dp
		}
	}
	if latest.Average == nil {
		return 0, false
	}
	return *latest.Average, true
}
