package main

import (
	"context"
	"log"
	"os"
	"strconv"

	awsclient "spot-rl-controller/internal/aws"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/joho/godotenv"
)

func main() {
	_ = godotenv.Load("configs/.env")

	// Đọc số jobs từ argument, mặc định 20
	count := 20
	if len(os.Args) > 1 {
		if n, err := strconv.Atoi(os.Args[1]); err == nil {
			count = n
		}
	}

	awsCfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		log.Fatalf("AWS config: %v", err)
	}

	sqsClient := awsclient.NewSQSClient(awsCfg, os.Getenv("SQS_QUEUE_URL"))

	log.Printf("Sending %d jobs to queue...", count)
	if err := sqsClient.SendJobs(context.Background(), count); err != nil {
		log.Fatalf("SendJobs: %v", err)
	}
	log.Printf("Done! %d jobs sent.", count)
}
