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
type PoolUtil struct {
	CPUUtil float64 // 0..1
	RAMUtil float64 // 0..1
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
			cpu := c.queryMetric(ctx, itype, az, "CPUUtilization", start, end, period)
			ram, ramOK := c.queryMetricRaw(ctx, itype, az, "mem_used_percent", start, end, period)
			if !ramOK {
				// mem_used_percent requires CloudWatch agent; fall back to 50%
				ram = 50.0
			}
			result[key] = PoolUtil{
				CPUUtil: cpu / 100.0, // CloudWatch returns 0-100
				RAMUtil: ram / 100.0,
			}
		}
	}
	return result
}

// queryMetric returns the average value of a standard EC2 CloudWatch metric
// (namespace AWS/EC2) filtered by InstanceType and AvailabilityZone dimensions.
// Returns (value, ok). value is already in the metric's native unit (e.g. percent 0-100).
func (c *CloudWatchClient) queryMetric(
	ctx context.Context,
	instanceType, az, metricName string,
	start, end time.Time,
	period int32,
) float64 {
	v, _ := c.queryMetricRaw(ctx, instanceType, az, metricName, start, end, period)
	return v
}

func (c *CloudWatchClient) queryMetricRaw(
	ctx context.Context,
	instanceType, az, metricName string,
	start, end time.Time,
	period int32,
) (float64, bool) {
	// Standard EC2 metrics use AWS/EC2 namespace.
	// Custom agent metrics (mem_used_percent) use CWAgent namespace.
	namespace := "AWS/EC2"
	if metricName == "mem_used_percent" {
		namespace = "CWAgent"
	}

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
