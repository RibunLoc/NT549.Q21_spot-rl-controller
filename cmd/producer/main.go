package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	awsclient "spot-rl-controller/internal/aws"
	"spot-rl-controller/internal/workload"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/joho/godotenv"
)

type Config struct {
	SqsQueueUrl     string
	BaseArrivalRate float64
	LoopInterval    time.Duration
}

// hàm load config
func loadConfig() Config {
	getEnv := func(key, fallback string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return fallback
	}

	getFloat := func(key string, fb float64) float64 {
		if v := os.Getenv(key); v != "" {
			result, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return fb
			}
			return result
		}

		return fb
	}

	return Config{
		SqsQueueUrl:     getEnv("SQS_QUEUE_URL", ""),
		BaseArrivalRate: getFloat("BASE_ARRIVAL_RATE", 2.0),
		LoopInterval:    15 * time.Second,
	}

}

func main() {
	log.SetFlags(log.Ltime | log.Lshortfile)
	_ = godotenv.Load("configs/.env")

	log.Println("=== sending job messages to SQS starting ===")
	cfg := loadConfig()
	gen := workload.NewGenerator(cfg.BaseArrivalRate, time.Now().UnixNano())
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
			jobs := gen.Step()
			for _, job := range jobs {
				data, err := json.Marshal(job)
				if err == nil {
					if err := sqsClient.SendMessage(context.Background(), string(data)); err != nil {
						log.Printf("[warn] SendMessage failed: %v", err)
					}
				}
			}
			if len(jobs) > 0 {
				log.Printf("[producer] step=%d, sent=%d jobs", gen.CurrentStep(), len(jobs))
			}
		}
	}
}
