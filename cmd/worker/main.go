package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	awsclient "spot-rl-controller/internal/aws"
	"spot-rl-controller/internal/workload"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/joho/godotenv"
)

type Config struct {
	SqsQueueUrl  string
	LoopInterval time.Duration
}

// Load config
func loadConfig() Config {
	getEnv := func(key, fallback string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return fallback
	}

	return Config{
		SqsQueueUrl:  getEnv("SQS_QUEUE_URL", ""),
		LoopInterval: time.Second,
	}
}

func main() {
	log.SetFlags(log.Ltime | log.Lshortfile)
	_ = godotenv.Load("configs/.env")

	log.Println("=== worker consume messages SQS starting ===")
	cfg := loadConfig()
	awscfg, _ := config.LoadDefaultConfig(context.Background())
	sqsClient := awsclient.NewSQSClient(awscfg, cfg.SqsQueueUrl)

	ticker := time.NewTicker(cfg.LoopInterval)
	ctx := context.Background()

	for {
		select {
		case <-ctx.Done():
			log.Println("Shutting down gracefully...")
			return
		case <-ticker.C:
			var job workload.Job
			output, err := sqsClient.ReceiveMessage(ctx)
			if err != nil {
				log.Printf("[warn] ReceiveMessage: %v", err)
				continue
			}
			if len(output.Messages) == 0 {
				continue // queue rỗng, poll tiếp
			}
			msg := output.Messages[0] // lấy message đầu tiên
			if err := json.Unmarshal([]byte(*msg.Body), &job); err != nil {
				log.Printf("[warn] unmarshal: %v", err)
				continue
			}

			log.Printf("[worker] processing job=%s type=%s duration=%d", job.JobID, job.Type, job.DurationSec)
			time.Sleep(time.Duration(job.DurationSec) * time.Second) // thời gian chờ xử lý job
			sqsClient.DeleteMessage(ctx, *msg.ReceiptHandle)
			log.Printf("[worker] done job=%s", job.JobID)
		}
	}
}
