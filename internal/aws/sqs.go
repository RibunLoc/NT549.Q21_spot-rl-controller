package aws

import (
	"context"
	"fmt"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// SQSClient đọc thông tin queue để lấy workload state
type SQSClient struct {
	sqsClient *sqs.Client
	queueURL  string
}

// NewSQSClient khởi tạo SQS client
func NewSQSClient(cfg aws.Config, queueURL string) *SQSClient {
	return &SQSClient{
		sqsClient: sqs.NewFromConfig(cfg),
		queueURL:  queueURL,
	}
}

// QueueDepth là sos messages trong queue
type QueueDepth struct {
	Pending int // ApproximateNumberOfMessages - chờ xử lý
	Running int // ApproximateNumberOfMessages - đang xử lý
}

// GetQueueDepth đọc số messages từ SQS
func (s *SQSClient) GetQueueDepth(ctx context.Context) (QueueDepth, error) {
	output, err := s.sqsClient.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(s.queueURL),
		AttributeNames: []sqstypes.QueueAttributeName{
			sqstypes.QueueAttributeNameApproximateNumberOfMessages,
			sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
		},
	})
	if err != nil {
		return QueueDepth{}, fmt.Errorf("GetQueueAttributes: %w", err)
	}

	attrs := output.Attributes
	pending, _ := strconv.Atoi(attrs[string(sqstypes.QueueAttributeNameApproximateNumberOfMessages)])
	running, _ := strconv.Atoi(attrs[string(sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible)])

	return QueueDepth{
		Pending: pending,
		Running: running,
	}, nil
}

// SendJobs gửi N messages vào queue (demo/test)
func (s *SQSClient) SendJobs(ctx context.Context, count int) error {
	for i := 0; i < count; i++ {
		_, err := s.sqsClient.SendMessage(ctx, &sqs.SendMessageInput{
			QueueUrl:    aws.String(s.queueURL),
			MessageBody: aws.String(fmt.Sprintf(`{"jobs_id": %d, "type": "batch"}`, i)),
		})
		if err != nil {
			return fmt.Errorf("SendMessage job %d: %w", i, err)
		}
	}
	return nil
}

// SendMessage gửi messages
func (s *SQSClient) SendMessage(ctx context.Context, body string) error {
	_, err := s.sqsClient.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    aws.String(s.queueURL),
		MessageBody: aws.String(body),
	})
	if err != nil {
		return fmt.Errorf("SendMessage error: %w", err)
	}
	return nil
}

// Receive Message
func (s *SQSClient) ReceiveMessage(ctx context.Context) (*sqs.ReceiveMessageOutput, error) {
	return s.sqsClient.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(s.queueURL),
		MaxNumberOfMessages: 1,
		WaitTimeSeconds:     5, // long polling
	})
}

// Delete Message
func (s *SQSClient) DeleteMessage(ctx context.Context, receiptHandle string) error {
	_, err := s.sqsClient.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(s.queueURL),
		ReceiptHandle: aws.String(receiptHandle),
	})
	if err != nil {
		return fmt.Errorf("DeleteMessage error: %w", err)
	}
	return nil
}
