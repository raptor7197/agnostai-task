package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/krxsna/agnostai-task/internal/api"
	"github.com/krxsna/agnostai-task/internal/clickhouse"
	"github.com/krxsna/agnostai-task/internal/kafka"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("[api] starting event analytics API server")

	// --- Configuration from environment variables ---
	chHost := getEnv("CLICKHOUSE_HOST", "localhost")
	chPort := getEnvInt("CLICKHOUSE_PORT", 9000)
	chDB := getEnv("CLICKHOUSE_DATABASE", "event_analytics")
	chUser := getEnv("CLICKHOUSE_USER", "default")
	chPassword := getEnv("CLICKHOUSE_PASSWORD", "")

	kafkaBrokers := getEnv("KAFKA_BROKERS", "localhost:9092")
	kafkaTopic := getEnv("KAFKA_TOPIC", "events")

	apiPort := getEnv("API_PORT", "8080")

	// --- ClickHouse Setup ---
	chCfg := clickhouse.Config{
		Host:     chHost,
		Port:     chPort,
		Database: chDB,
		Username: chUser,
		Password: chPassword,
	}

	// Ensure the database exists before connecting to it
	log.Println("[api] ensuring ClickHouse database exists...")
	if err := clickhouse.EnsureDatabase(chCfg); err != nil {
		log.Fatalf("[api] failed to ensure database: %v", err)
	}

	// Connect to ClickHouse
	chClient, err := clickhouse.NewClient(chCfg)
	if err != nil {
		log.Fatalf("[api] failed to connect to ClickHouse: %v", err)
	}
	defer chClient.Close()

	// Run migrations
	if err := clickhouse.RunMigrations(chClient); err != nil {
		log.Fatalf("[api] failed to run migrations: %v", err)
	}

	repo := clickhouse.NewRepository(chClient)

	// --- Kafka Producer Setup ---
	producerCfg := kafka.ProducerConfig{
		Brokers: []string{kafkaBrokers},
		Topic:   kafkaTopic,
	}

	producer, err := kafka.NewProducer(producerCfg)
	if err != nil {
		log.Fatalf("[api] failed to create Kafka producer: %v", err)
	}
	defer producer.Close()

	// --- HTTP Server Setup ---
	router := api.NewRouter(producer, repo)

	server := &http.Server{
		Addr:         ":" + apiPort,
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// --- Graceful Shutdown ---
	// Start the server in a goroutine so we can listen for shutdown signals
	go func() {
		log.Printf("[api] HTTP server listening on :%s", apiPort)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[api] HTTP server error: %v", err)
		}
	}()

	// Wait for interrupt signal (SIGINT or SIGTERM)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	log.Printf("[api] received signal %v, shutting down gracefully...", sig)

	// Give outstanding requests 10 seconds to complete
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Fatalf("[api] server forced to shutdown: %v", err)
	}

	log.Println("[api] server exited cleanly")
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
		log.Printf("[api] warning: invalid integer for env %s=%s, using default %d", key, value, defaultValue)
	}
	return defaultValue
}
