package main

import (
	"context"
	"log"
	"os"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"external-service-log/internal/archiver"
)

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	ctx := context.Background()
	mongoURI := getEnv("MONGO_URI", "mongodb://localhost:27017")
	mongoDBName := getEnv("MONGO_DB_NAME", "service_logs")

	client, err := mongo.Connect(options.Client().ApplyURI(mongoURI))
	if err != nil {
		log.Fatalf("archiver: connect to mongo: %v", err)
	}
	defer func() { _ = client.Disconnect(ctx) }()

	if err := client.Ping(ctx, nil); err != nil {
		log.Fatalf("archiver: ping mongo: %v", err)
	}

	db := client.Database(mongoDBName)
	a := archiver.New(db)

	if err := a.Run(ctx); err != nil {
		log.Fatalf("archiver: %v", err)
	}
}
