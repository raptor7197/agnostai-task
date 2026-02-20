package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/krxsna/agnostai-task/internal/clickhouse"
	"github.com/krxsna/agnostai-task/internal/kafka"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("[consumer] starting event analytics Kafka consumer")

	// --- Configuration from environment variables ---
	chHost := getEnv("CLICKHOUSE_HOST", "localhost")
	chPort := getEnvInt("CLICKHOUSE_PORT", 9000)
	chDB := getEnv("CLICKHOUSE_DATABASE", "event_analytics")
	chUser := getEnv("CLICKHOUSE_USER", "default")
	chPassword := getEnv("CLICKHOUSE_PASSWORD", "")

	kafkaBrokers := getEnv("KAFKA_BROKERS", "localhost:9092")
	kafkaTopic := getEnv("KAFKA_TOPIC", "events")
	kafkaGroupID := getEnv("KAFKA_GROUP_ID", "event-consumer-group")

	batchSize := getEnvInt("CONSUMER_BATCH_SIZE", 500)
	flushIntervalMs := getEnvInt("CONSUMER_FLUSH_INTERVAL_MS", 200)

	chCfg := clickhouse.Config{
		Host:     chHost,
		Port:     chPort,
		Database: chDB,
		Username: chUser,
		Password: chPassword,
	}

	// Ensure the database exists before connecting to it
	log.Println("[consumer] ensuring ClickHouse database exists...")
	if err := clickhouse.EnsureDatabase(chCfg); err != nil {
		log.Fatalf("[consumer] failed to ensure database: %v", err)
	}

	// Connect to ClickHouse
	chClient, err := clickhouse.NewClient(chCfg)
	if err != nil {
		log.Fatalf("[consumer] failed to connect to ClickHouse: %v", err)
	}
	defer chClient.Close()

	// migtrations idempotent
	if err := clickhouse.RunMigrations(chClient); err != nil {
		log.Fatalf("[consumer] failed to run migrations: %v", err)
	}

	repo := clickhouse.NewRepository(chClient)

	// --- Kafka Consumer Setup ---
	consumerCfg := kafka.ConsumerConfig{
		Brokers:  []string{kafkaBrokers},
		Topic:    kafkaTopic,
		GroupID:  kafkaGroupID,
		MinBytes: 1,
		MaxBytes: 10 << 20, // 10 MB
	}

	// Ensure the topic exists before consuming
	if err := kafka.EnsureTopic(consumerCfg.Brokers, consumerCfg.Topic, 3); err != nil {
		log.Printf("[consumer] warning: could not ensure topic: %v (will retry on consume)", err)
	}

	consumer := kafka.NewConsumer(consumerCfg, repo)
	defer consumer.Close()

	// --- Graceful Shutdown ---
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Listen for shutdown signals
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	// Start consuming in a goroutine
	errCh := make(chan error, 1)
	go func() {
		flushInterval := time.Duration(flushIntervalMs) * time.Millisecond
		log.Printf("[consumer] starting batch consumer (batch_size=%d, flush_interval=%v)", batchSize, flushInterval)
		errCh <- consumer.StartBatch(ctx, batchSize, flushInterval)
	}()

	// Log consumer stats periodically
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				stats := consumer.Stats()
				log.Printf("[consumer] stats: messages=%d, lag=%d, errors=%d, dials=%d",
					stats.Messages, stats.Lag, stats.Errors, stats.Dials)
			}
		}
	}()

	// Wait for either a shutdown signal or a consumer error
	select {
	case sig := <-quit:
		log.Printf("[consumer] received signal %v, shutting down gracefully...", sig)
		cancel()
		// Wait a bit for the consumer to finish processing current batch
		time.Sleep(2 * time.Second)
	case err := <-errCh:
		if err != nil && err != context.Canceled {
			log.Printf("[consumer] consumer exited with error: %v", err)
		}
	}

	log.Println("[consumer] shutdown complete")
}

// getEnv returns the value of an environment variable or a default value
func getEnv(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}

// getEnvInt returns the integer value of an environment variable or a default value
func getEnvInt(key string, defaultValue int) int {
	if value, exists := os.LookupEnv(key); exists {
		var result int
		_, err := fmt.Sscanf(value, "%d", &result)
		if err == nil {
			return result
		}
		log.Printf("[consumer] warning: invalid integer for env %s=%s, using default %d", key, value, defaultValue)
	}
	return defaultValue
}
