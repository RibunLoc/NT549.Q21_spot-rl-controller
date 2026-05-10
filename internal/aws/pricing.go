// Lấy giá spt thực từ AWS EC2 API
package aws

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// PricingClient lấy spot price history từ AWS
type PricingClient struct {
	ec2Client    *ec2.Client
	instanceType string
	az           string // availability zone
}

// NewPricingClient khởi tạo client với AWS config đã có sẵn
func NewPricingClient(cfg aws.Config, instanceType, az string) *PricingClient {
	return &PricingClient{
		ec2Client:    ec2.NewFromConfig(cfg),
		instanceType: instanceType,
		az:           az,
	}
}

// SpotPricePoint là một điểm giá tại một thời điểm
type SpotPricePoint struct {
	Price     float64
	Timestamp time.Time
}

// GetCurrentPrice lấy giá spot hiện tại (giá mới)
func (p *PricingClient) GetCurrentPrice(ctx context.Context) (float64, error) {
	history, err := p.GetPriceHistory(ctx, 1)
	if err != nil {
		return 0, err
	}
	if len(history) == 0 {
		return 0, fmt.Errorf("no spot price data available for %s in %s", p.instanceType, p.az)
	}
	return history[0].Price, nil
}

// GetPriceHistory lấy N giờ gần nhất của spot price history
func (p *PricingClient) GetPriceHistory(ctx context.Context, hours int) ([]SpotPricePoint, error) {
	startTime := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)

	output, err := p.ec2Client.DescribeSpotPriceHistory(ctx, &ec2.DescribeSpotPriceHistoryInput{
		InstanceTypes:       []types.InstanceType{types.InstanceType(p.instanceType)},
		AvailabilityZone:    aws.String(p.az),
		ProductDescriptions: []string{"Linux/UNIX"},
		StartTime:           aws.Time(startTime),
	})
	if err != nil {
		return nil, fmt.Errorf("DescribeSpotPriceHistory: %w", err)
	}

	// Parse và sort theo thời gian mới nhất
	points := make([]SpotPricePoint, 0, len(output.SpotPriceHistory))
	for _, item := range output.SpotPriceHistory {
		price, err := strconv.ParseFloat(aws.ToString(item.SpotPrice), 64)
		if err != nil {
			continue
		}
		points = append(points, SpotPricePoint{
			Price:     price,
			Timestamp: aws.ToTime(item.Timestamp),
		})
	}

	sort.Slice(points, func(i, j int) bool {
		return points[i].Timestamp.After(points[j].Timestamp)
	})

	return points, nil
}

// CheapestAZ so sánh giá spot tất cả AZ, trả về AZ rẻ nhất và giá hiện tại
func (p *PricingClient) CheapestAZ(ctx context.Context, azList []string) (string, float64, error) {
	cheapestAZ := azList[0]
	cheapestPrice := math.MaxFloat64

	for _, az := range azList {
		output, err := p.ec2Client.DescribeSpotPriceHistory(ctx, &ec2.DescribeSpotPriceHistoryInput{
			InstanceTypes:       []types.InstanceType{types.InstanceType(p.instanceType)},
			AvailabilityZone:    aws.String(az),
			ProductDescriptions: []string{"Linux/UNIX"},
			StartTime:           aws.Time(time.Now().UTC().Add(-1 * time.Hour)),
		})
		if err != nil || len(output.SpotPriceHistory) == 0 {
			continue
		}
		price, err := strconv.ParseFloat(aws.ToString(output.SpotPriceHistory[0].SpotPrice), 64)
		if err != nil {
			continue
		}
		if price < cheapestPrice {
			cheapestPrice = price
			cheapestAZ = az
		}
	}

	if cheapestPrice == math.MaxFloat64 {
		return "", 0, fmt.Errorf("no spot price data for any AZ")
	}
	return cheapestAZ, cheapestPrice, nil
}

// GetPriceStats tính mean và volatility từ lịch sử giá
func (p *PricingClient) GetPriceStats(history []SpotPricePoint, window int) (mean, volatility float64) {
	if len(history) == 0 {
		return 0, 0
	}

	n := window
	if n > len(history) {
		n = len(history)
	}

	// Mean
	sum := 0.0
	for i := 0; i < n; i++ {
		sum += history[i].Price
	}
	mean = sum / float64(n)

	// Volatility (std dev)
	if n < 2 {
		return mean, 0
	}
	variance := 0.0
	for i := 0; i < n; i++ {
		diff := history[i].Price - mean
		variance += diff * diff
	}
	volatility = variance / float64(n)
	return mean, volatility
}

// GetSpotPrice lấy giá spot hiện tại cho instanceType + az bất kỳ
func (p *PricingClient) GetSpotPrice(ctx context.Context, instanceType, az string) (float64, error) {
	output, err := p.ec2Client.DescribeSpotPriceHistory(ctx, &ec2.DescribeSpotPriceHistoryInput{
		InstanceTypes:       []types.InstanceType{types.InstanceType(instanceType)},
		AvailabilityZone:    aws.String(az),
		ProductDescriptions: []string{"Linux/UNIX"},
		StartTime:           aws.Time(time.Now().UTC().Add(-1 * time.Hour)),
	})
	if err != nil {
		return 0, fmt.Errorf("GetSpotPrice(%s/%s): %w", instanceType, az, err)
	}

	if len(output.SpotPriceHistory) == 0 {
		return 0, fmt.Errorf("no spot price for %s in %s", instanceType, az)
	}
	price, err := strconv.ParseFloat(aws.ToString(output.SpotPriceHistory[0].SpotPrice), 64)
	if err != nil {
		return 0, fmt.Errorf("parse price: %w", err)
	}
	return price, nil
}

// GetOnDemandPrice trả về giá trị on-demand ($/hr)
func (p *PricingClient) GetOnDemandPrice(instanceType string) float64 {
	odPrices := map[string]float64{
		"m5.large":   0.120,
		"c5.xlarge":  0.196,
		"r5.large":   0.152,
		"m5.xlarge":  0.240,
		"c5.2xlarge": 0.392,
	}
	if price, ok := odPrices[instanceType]; ok {
		return price
	}
	return 0.10
}
