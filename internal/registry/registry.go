// Package registry — track instance-id ↔ Jenkins agent name mapping.
// Populated khi Spot instance boot (UserData tags EC2 với JenkinsAgentName).
// Controller dùng để tìm agent cần drain trước khi terminate.
package registry

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// InstanceInfo metadata của 1 Spot instance được controller quản lý.
type InstanceInfo struct {
	InstanceID  string
	AgentName   string // Jenkins agent name = "spot-agent-<instance-id>"
	AZ          string
	LaunchTime  time.Time
	InstanceType string
}

// Registry giữ danh sách Spot instances đang chạy, đồng bộ từ EC2 tags.
type Registry struct {
	mu        sync.RWMutex
	instances map[string]*InstanceInfo // key: instance-id
	ec2Client *ec2.Client
}

func New(cfg aws.Config) *Registry {
	return &Registry{
		instances: make(map[string]*InstanceInfo),
		ec2Client: ec2.NewFromConfig(cfg),
	}
}

// Sync đồng bộ registry từ EC2 tags — gọi mỗi loop iteration.
// Chỉ load instances có tag ManagedBy=spot-rl-controller và JenkinsAgentName set.
func (r *Registry) Sync(ctx context.Context) error {
	output, err := r.ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{Name: aws.String("instance-state-name"), Values: []string{"running", "pending"}},
			{Name: aws.String("tag:ManagedBy"), Values: []string{"spot-rl-controller"}},
			{Name: aws.String("tag:JenkinsRole"), Values: []string{"agent"}},
		},
	})
	if err != nil {
		return fmt.Errorf("DescribeInstances: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Rebuild map từ EC2 response
	fresh := make(map[string]*InstanceInfo)
	for _, res := range output.Reservations {
		for _, inst := range res.Instances {
			id := aws.ToString(inst.InstanceId)
			az := aws.ToString(inst.Placement.AvailabilityZone)
			instType := string(inst.InstanceType)

			agentName := getTag(inst.Tags, "JenkinsAgentName")
			if agentName == "" {
				// Instance chưa boot xong / chưa tag — giữ từ registry cũ nếu có
				if prev, ok := r.instances[id]; ok {
					fresh[id] = prev
				}
				continue
			}

			launch := time.Time{}
			if inst.LaunchTime != nil {
				launch = *inst.LaunchTime
			}

			fresh[id] = &InstanceInfo{
				InstanceID:   id,
				AgentName:    agentName,
				AZ:           az,
				LaunchTime:   launch,
				InstanceType: instType,
			}
		}
	}

	removed := len(r.instances) - len(fresh)
	added := 0
	for id := range fresh {
		if _, ok := r.instances[id]; !ok {
			added++
		}
	}
	r.instances = fresh
	if added > 0 || removed > 0 {
		log.Printf("[registry] sync: total=%d added=%d removed=%d", len(fresh), added, removed)
	}
	return nil
}

// OldestSpotAgentExcludingAZ trả về agent name của Spot instance cũ nhất
// KHÔNG thuộc targetAZ — đó là instance cần migrate ra.
// Trả về "" nếu không tìm thấy (caller skip drain).
func (r *Registry) OldestSpotAgentExcludingAZ(targetAZ string) (instanceID, agentName string) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var oldest *InstanceInfo
	for _, info := range r.instances {
		if info.AZ == targetAZ {
			continue // đây là AZ đích, không phải AZ nguồn
		}
		if oldest == nil || info.LaunchTime.Before(oldest.LaunchTime) {
			oldest = info
		}
	}

	if oldest == nil {
		return "", ""
	}
	return oldest.InstanceID, oldest.AgentName
}

// AgentForInstance trả về agent name của instance-id cụ thể.
func (r *Registry) AgentForInstance(instanceID string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if info, ok := r.instances[instanceID]; ok {
		return info.AgentName, true
	}
	return "", false
}

// Remove xóa instance khỏi registry sau khi terminate.
func (r *Registry) Remove(instanceID string) {
	r.mu.Lock()
	delete(r.instances, instanceID)
	r.mu.Unlock()
}

// Count trả về số instances đang track.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.instances)
}

func getTag(tags []types.Tag, key string) string {
	for _, t := range tags {
		if aws.ToString(t.Key) == key {
			return aws.ToString(t.Value)
		}
	}
	return ""
}
