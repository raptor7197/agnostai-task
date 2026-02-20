package kafka

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/krxsna/agnostai-task/internal/clickhouse"
	"github.com/krxsna/agnostai-task/internal/models"
	kafkago "github.com/segmentio/kafka-go"
)

// ConsumerConfig holds Kafka consumer configuration
type ConsumerConfig struct {
	Brokers  []string
	Topic    string
	GroupID  string
	MinBytes int
	MaxBytes int
}

// DefaultConsumerConfig returns a default consumer configuration
func DefaultConsumerConfig() ConsumerConfig {
	return ConsumerConfig{
		Brokers:  []string{"localhost:9092"},
		Topic:    "events",
		GroupID:  "event-consumer-group",
		MinBytes: 1,        // 1 byte minimum fetch
		MaxBytes: 10 << 20, // 10 MB max fetch
	}
}

// Consumer reads messages from Kafka and writes to ClickHouse
type Consumer struct {
	reader *kafkago.Reader
	repo   *clickhouse.Repository
	config ConsumerConfig
}

// NewConsumer creates a new Kafka consumer connected to ClickHouse
func NewConsumer(cfg ConsumerConfig, repo *clickhouse.Repository) *Consumer {
	reader := kafkago.NewReader(kafkago.ReaderConfig{
		Brokers:        cfg.Brokers,
		Topic:          cfg.Topic,
		GroupID:        cfg.GroupID,
		MinBytes:       cfg.MinBytes,
		MaxBytes:       cfg.MaxBytes,
		CommitInterval: 1 * time.Second,
		StartOffset:    kafkago.FirstOffset,
		MaxWait:        500 * time.Millisecond,
	})

	log.Printf("[kafka-consumer] initialized for topic '%s', group '%s', brokers %v",
		cfg.Topic, cfg.GroupID, cfg.Brokers)

	return &Consumer{
		reader: reader,
		repo:   repo,
		config: cfg,
	}
}

// Start begins consuming messages in a blocking loop.
// It processes messages one by one for correctness (ordering per partition).
// Call this in a goroutine if you need it to be non-blocking.
func (c *Consumer) Start(ctx context.Context) error {
	log.Println("[kafka-consumer] starting message consumption loop")

	for {
		select {
		case <-ctx.Done():
			log.Println("[kafka-consumer] context cancelled, stopping consumer")
			return ctx.Err()
		default:
		}

		msg, err := c.reader.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("[kafka-consumer] error fetching message: %v", err)
			time.Sleep(500 * time.Millisecond)
			continue
		}

		if err := c.processMessage(ctx, msg); err != nil {
			log.Printf("[kafka-consumer] error processing message (partition=%d, offset=%d): %v",
				msg.Partition, msg.Offset, err)
			// tbd : dead letter queue can be implemented for backup ,afterwards
		}

		// Commit the message offset after processing
		if err := c.reader.CommitMessages(ctx, msg); err != nil {
			log.Printf("[kafka-consumer] error committing offset: %v", err)
		}
	}
}

// StartBatch consumes messages in batches for higher throughput.
// Batches create operations together; updates and deletes are processed individually
// because they require reading the current state first.
func (c *Consumer) StartBatch(ctx context.Context, batchSize int, flushInterval time.Duration) error {
	log.Printf("[kafka-consumer] starting batch consumption (batch_size=%d, flush_interval=%v)",
		batchSize, flushInterval)

	var createBatch []*models.Event
	var pendingMessages []kafkago.Message
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	flushCreates := func() {
		if len(createBatch) == 0 {
			return
		}

		if err := c.repo.BatchInsertEvents(ctx, createBatch); err != nil {
			log.Printf("[kafka-consumer] batch insert failed for %d events: %v", len(createBatch), err)
			// Fall back to individual inserts
			for _, event := range createBatch {
				if err := c.repo.InsertEvent(ctx, event); err != nil {
					log.Printf("[kafka-consumer] individual insert also failed for event %s: %v", event.ID, err)
				}
			}
		}

		if len(pendingMessages) > 0 {
			if err := c.reader.CommitMessages(ctx, pendingMessages...); err != nil {
				log.Printf("[kafka-consumer] error committing batch offsets: %v", err)
			}
		}

		log.Printf("[kafka-consumer] flushed batch of %d creates", len(createBatch))
		createBatch = createBatch[:0]
		pendingMessages = pendingMessages[:0]
	}

	for {
		select {
		case <-ctx.Done():
			flushCreates() // Flush remaining before exit
			log.Println("[kafka-consumer] context cancelled, stopping batch consumer")
			return ctx.Err()

		case <-ticker.C:
			flushCreates()

		default:
			// periodic flushing
			fetchCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
			msg, err := c.reader.FetchMessage(fetchCtx)
			cancel()

			if err != nil {
				if ctx.Err() != nil {
					flushCreates()
					return ctx.Err()
				}
				continue
			}

			var kafkaMsg models.KafkaMessage
			if err := json.Unmarshal(msg.Value, &kafkaMsg); err != nil {
				log.Printf("[kafka-consumer] failed to unmarshal message: %v", err)
				_ = c.reader.CommitMessages(ctx, msg)
				continue
			}

			switch kafkaMsg.Operation {
			case models.OpCreate:
				event := c.buildEventFromPayload(kafkaMsg.EventID, kafkaMsg.Data, kafkaMsg.Timestamp)
				createBatch = append(createBatch, event)
				pendingMessages = append(pendingMessages, msg)

				if len(createBatch) >= batchSize {
					flushCreates()
				}

			case models.OpUpdate:
				// Flush pending creates first to ensure ordering
				flushCreates()
				if err := c.handleUpdate(ctx, kafkaMsg); err != nil {
					log.Printf("[kafka-consumer] update failed for event %s: %v", kafkaMsg.EventID, err)
				}
				_ = c.reader.CommitMessages(ctx, msg)

			case models.OpDelete:
				// Flush pending creates first to ensure ordering
				flushCreates()
				if err := c.handleDelete(ctx, kafkaMsg); err != nil {
					log.Printf("[kafka-consumer] delete failed for event %s: %v", kafkaMsg.EventID, err)
				}
				_ = c.reader.CommitMessages(ctx, msg)

			default:
				log.Printf("[kafka-consumer] unknown operation: %s", kafkaMsg.Operation)
				_ = c.reader.CommitMessages(ctx, msg)
			}
		}
	}
}

// processMessage handles a single Kafka message by routing it to the appropriate handler
func (c *Consumer) processMessage(ctx context.Context, msg kafkago.Message) error {
	var kafkaMsg models.KafkaMessage
	if err := json.Unmarshal(msg.Value, &kafkaMsg); err != nil {
		return fmt.Errorf("failed to unmarshal message: %w", err)
	}

	log.Printf("[kafka-consumer] processing %s for event %s (partition=%d, offset=%d)",
		kafkaMsg.Operation, kafkaMsg.EventID, msg.Partition, msg.Offset)

	switch kafkaMsg.Operation {
	case models.OpCreate:
		return c.handleCreate(ctx, kafkaMsg)
	case models.OpUpdate:
		return c.handleUpdate(ctx, kafkaMsg)
	case models.OpDelete:
		return c.handleDelete(ctx, kafkaMsg)
	default:
		return fmt.Errorf("unknown operation: %s", kafkaMsg.Operation)
	}
}

// handleCreate inserts a new event into ClickHouse
func (c *Consumer) handleCreate(ctx context.Context, msg models.KafkaMessage) error {
	if msg.Data == nil {
		return fmt.Errorf("create message has no data payload")
	}

	event := c.buildEventFromPayload(msg.EventID, msg.Data, msg.Timestamp)
	return c.repo.InsertEvent(ctx, event)
}

// buildEventFromPayload constructs an Event from a KafkaMessage payload
func (c *Consumer) buildEventFromPayload(eventID string, payload *models.EventPayload, timestamp time.Time) *models.Event {
	event := &models.Event{
		ID:                   eventID,
		ConversationID:       payload.ConversationID,
		ConversationMetadata: payload.ConversationMetadata,
		EventName:            payload.EventName,
		Error:                "",
		Input:                payload.Input,
		InputEmbeddings:      payload.InputEmbeddings,
		Output:               payload.Output,
		OutputEmbeddings:     payload.OutputEmbeddings,
		IsDeleted:            0,
		Version:              1,
		CreatedAt:            timestamp,
		UpdatedAt:            timestamp,
	}

	if payload.Error != nil {
		event.Error = *payload.Error
	}
	if payload.Latency != nil {
		event.Latency = *payload.Latency
	}

	// Ensure embeddings are not nil (ClickHouse Array requires non-nil)
	if event.InputEmbeddings == nil {
		event.InputEmbeddings = []float32{}
	}
	if event.OutputEmbeddings == nil {
		event.OutputEmbeddings = []float32{}
	}

	return event
}

// handleUpdate updates an existing event in ClickHouse via ReplacingMergeTree versioning
func (c *Consumer) handleUpdate(ctx context.Context, msg models.KafkaMessage) error {
	if msg.Data == nil {
		return fmt.Errorf("update message has no data payload")
	}

	return c.repo.UpdateEvent(ctx, msg.EventID, msg.Data)
}

// handleDelete performs a soft delete by inserting a new version with is_deleted = 1
func (c *Consumer) handleDelete(ctx context.Context, msg models.KafkaMessage) error {
	return c.repo.DeleteEvent(ctx, msg.EventID)
}

// Close closes the Kafka reader
func (c *Consumer) Close() error {
	log.Println("[kafka-consumer] closing reader")
	return c.reader.Close()
}

// Stats returns consumer lag and other stats
func (c *Consumer) Stats() kafkago.ReaderStats {
	return c.reader.Stats()
}
