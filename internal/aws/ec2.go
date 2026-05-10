package aws

import (
	"context"
	"fmt"
	"log"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// AZSubnetMap map từ AZ sang SubnetID
type AZSubnetMap map[string]string

// EC2Client quản lý spot và on-demand instnace
type EC2Client struct {
	ec2Client    *ec2.Client
	instanceType string
	az           string
	subnetID     string
	keyName      string
	amiID        string
	azSubnets    AZSubnetMap
}

// NewEC2Client khởi tạo EC2 client với 1 AZ
func NewEC2Client(cfg aws.Config, instanceType, az, subnetID, keyName, amiID string) *EC2Client {
	return &EC2Client{
		ec2Client:    ec2.NewFromConfig(cfg),
		instanceType: instanceType,
		az:           az,
		subnetID:     subnetID,
		amiID:        amiID,
		azSubnets:    AZSubnetMap{az: subnetID},
	}
}

// NewEC2ClientMultiAZ khởi tạo EC2 client với nhiều AZ
func NewEC2ClientMultiAZ(cfg aws.Config, instanceType string, azSubnets AZSubnetMap, amiID string) *EC2Client {
	var defaultAZ, defaultSubnet string
	for az, subnet := range azSubnets {
		defaultAZ = az
		defaultSubnet = subnet
		break
	}
	return &EC2Client{
		ec2Client:    ec2.NewFromConfig(cfg),
		instanceType: instanceType,
		az:           defaultAZ,
		subnetID:     defaultSubnet,
		amiID:        amiID,
		azSubnets:    azSubnets,
	}
}

// SetAZ cập nhật AZ và subnet hiện tại — gọi trước khi request instances
func (c *EC2Client) SetAZ(az string) error {
	subnet, ok := c.azSubnets[az]
	if !ok {
		return fmt.Errorf("unknown AZ: %s", az)
	}
	c.az = az
	c.subnetID = subnet
	log.Printf("[ec2] switched to AZ=%s subnet=%s", az, subnet)
	return nil
}

// InstanceCount là số lượng instances đang chạy
type InstanceCounts struct {
	Spot     int
	OnDemand int
}

// GetRunningInstances đếm số spot và on-demand instances đang chạy
func (c *EC2Client) GetRunningInstances(ctx context.Context) (InstanceCounts, error) {
	output, err := c.ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("instance-state-name"),
				Values: []string{"running", "pending"},
			},
			{
				Name:   aws.String("instance-type"),
				Values: []string{c.instanceType},
			},
		},
	})
	if err != nil {
		return InstanceCounts{}, fmt.Errorf("DescribeInstances: %w", err)
	}

	counts := InstanceCounts{}
	for _, reservation := range output.Reservations {
		for _, instance := range reservation.Instances {
			// Spot instance có SpotInstanceRequestId
			if instance.SpotInstanceRequestId != nil {
				counts.Spot++
			} else {
				counts.OnDemand++
			}
		}
	}
	return counts, nil
}

// RequestSpot request thêm N spot instances
func (c *EC2Client) RequestSpot(ctx context.Context, count int) error {
	log.Printf("[debug] RequestSpot amiID=%q subnetID=%q instanceType=%q", c.amiID, c.subnetID, c.instanceType)
	_, err := c.ec2Client.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId:      aws.String(c.amiID),
		InstanceType: types.InstanceType(c.instanceType),
		MinCount:     aws.Int32(int32(count)),
		MaxCount:     aws.Int32(int32(count)),
		SubnetId:     aws.String(c.subnetID),
		// InstanceMarketOptions = spot
		InstanceMarketOptions: &types.InstanceMarketOptionsRequest{
			MarketType: types.MarketTypeSpot,
			SpotOptions: &types.SpotMarketOptions{
				SpotInstanceType: types.SpotInstanceTypeOneTime,
			},
		},
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeInstance,
				Tags: []types.Tag{
					{Key: aws.String("ManagedBy"), Value: aws.String("spot-rl-controller")},
					{Key: aws.String("InstanceKind"), Value: aws.String("spot")},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("RequestSpot(%d): %w", count, err)
	}
	return nil
}

// RequestOnDemand request thêm N on-demand instances
func (c *EC2Client) RequestOnDemand(ctx context.Context, count int) error {
	_, err := c.ec2Client.RunInstances(ctx, &ec2.RunInstancesInput{
		ImageId:      aws.String(c.amiID),
		InstanceType: types.InstanceType(c.instanceType),
		MinCount:     aws.Int32(int32(count)),
		MaxCount:     aws.Int32(int32(count)),
		SubnetId:     aws.String(c.subnetID),
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeInstance,
				Tags: []types.Tag{
					{Key: aws.String("ManagedBy"), Value: aws.String("spot-rl-controller")},
					{Key: aws.String("InstanceKind"), Value: aws.String("on-demand")},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("RequestOnDemand(%d): %w", count, err)
	}
	return nil
}

// TerminateByKind terminate N instances theo loại ("spot" hoặc "on-demand")
func (c *EC2Client) TerminateByKind(ctx context.Context, kind string, count int) error {
	// Tìm instances theo tag InstanceKind
	output, err := c.ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{Name: aws.String("instance-state-name"), Values: []string{"running", "pending"}},
			{Name: aws.String("tag:ManagedBy"), Values: []string{"spot-rl-controller"}},
			{Name: aws.String("tag:InstanceKind"), Values: []string{kind}},
		},
	})

	if err != nil {
		return fmt.Errorf("DescribeInstances: %w", err)

	}

	// Collect instance IDs, lấy tối đa `count` cái
	var ids []string
	for _, r := range output.Reservations {
		for _, inst := range r.Instances {
			ids = append(ids, aws.ToString(inst.InstanceId))
			if len(ids) >= count {
				break
			}
		}
		if len(ids) >= count {
			break
		}
	}

	if len(ids) == 0 {
		return nil // không có gì để terminate
	}

	_, err = c.ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: ids,
	})
	if err != nil {
		return fmt.Errorf("TerminateInstances(%s): %w", kind, err)
	}
	return nil
}

// GetRunningInstanceByTypeAZ đếm spot/on-demand theo instanceType cụ thể
func (c *EC2Client) GetRunningInstancesByTypeAZ(ctx context.Context, instanceType, az string) (InstanceCounts, error) {
	output, err := c.ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{Name: aws.String("instance-state-name"), Values: []string{"running", "pending"}},
			{Name: aws.String("availability-zone"), Values: []string{"az"}},
			{Name: aws.String("tag:ManagedBy"), Values: []string{"spot-rl-controller"}},
		},
	})
	if err != nil {
		return InstanceCounts{}, fmt.Errorf("DescribeInstances(%s/%s): %w", instanceType, az, err)
	}

	counts := InstanceCounts{}
	for _, reservation := range output.Reservations {
		for _, instance := range reservation.Instances {
			if instance.SpotInstanceRequestId != nil {
				counts.Spot++
			} else {
				counts.OnDemand++
			}
		}
	}
	return counts, nil
}

//
