package clickhouse

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Config holds ClickHouse connection configuration
type Config struct {
	Host     string
	Port     int
	Database string
	Username string
	Password string
}

// Client wraps the ClickHouse connection
type Client struct {
	Conn   driver.Conn
	Config Config
}

// DefaultConfig returns a default ClickHouse configuration
func DefaultConfig() Config {
	return Config{
		Host:     "localhost",
		Port:     9000,
		Database: "event_analytics",
		Username: "default",
		Password: "",
	}
}

// NewClient creates a new ClickHouse client and verifies the connection
func NewClient(cfg Config) (*Client, error) {
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)},
		Auth: clickhouse.Auth{
			Database: cfg.Database,
			Username: cfg.Username,
			Password: cfg.Password,
		},
		Settings: clickhouse.Settings{
			"max_execution_time": 60,
		},
		DialTimeout:     10 * time.Second,
		ConnMaxLifetime: 1 * time.Hour,
		MaxOpenConns:    20,
		MaxIdleConns:    10,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open clickhouse connection: %w", err)
	}

	client := &Client{
		Conn:   conn,
		Config: cfg,
	}

	// Verify connectivity with retries (useful when starting with docker-compose)
	if err := client.PingWithRetry(30, 2*time.Second); err != nil {
		return nil, fmt.Errorf("failed to ping clickhouse after retries: %w", err)
	}

	return client, nil
}

// PingWithRetry attempts to ping ClickHouse with retries
func (c *Client) PingWithRetry(maxRetries int, delay time.Duration) error {
	ctx := context.Background()
	var lastErr error
	for i := 0; i < maxRetries; i++ {
		if err := c.Conn.Ping(ctx); err != nil {
			lastErr = err
			log.Printf("[clickhouse] ping attempt %d/%d failed: %v", i+1, maxRetries, err)
			time.Sleep(delay)
			continue
		}
		log.Println("[clickhouse] connected successfully")
		return nil
	}
	return fmt.Errorf("ping failed after %d retries: %w", maxRetries, lastErr)
}

// Close closes the ClickHouse connection
func (c *Client) Close() error {
	return c.Conn.Close()
}

// EnsureDatabase creates the database if it doesn't exist.
// This needs a separate connection to the 'default' database first.
func EnsureDatabase(cfg Config) error {
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)},
		Auth: clickhouse.Auth{
			Database: "default",
			Username: cfg.Username,
			Password: cfg.Password,
		},
		DialTimeout: 10 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("failed to connect to default db: %w", err)
	}
	defer conn.Close()

	// Retry ping
	ctx := context.Background()
	for i := 0; i < 30; i++ {
		if err := conn.Ping(ctx); err != nil {
			log.Printf("[clickhouse] waiting for server... attempt %d/30: %v", i+1, err)
			time.Sleep(2 * time.Second)
			continue
		}
		break
	}

	query := fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", cfg.Database)
	if err := conn.Exec(ctx, query); err != nil {
		return fmt.Errorf("failed to create database %s: %w", cfg.Database, err)
	}
	log.Printf("[clickhouse] database '%s' ensured", cfg.Database)
	return nil
}
