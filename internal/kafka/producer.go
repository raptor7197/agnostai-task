package kafka

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/krxsna/agnostai-task/internal/models"
	kafkago "github.com/segmentio/kafka-go"
)

type ProducerConfig struct {
	Brokers []string
	Topic   string
}

func DefaultProducerConfig() ProducerConfig {
	return ProducerConfig{
		Brokers: []string{"localhost:9092"},
		Topic:   "events",
	}
}

type Producer struct {
	writer *kafkago.Writer
	config ProducerConfig
}

// It uses the event ID as the message key to ensure all operations for the same
// event land on the same partition, preserving ordering guarantees per event.
func NewProducer(cfg ProducerConfig) (*Producer, error) {
	// Ensure the topic exists before we start producing
	if err := EnsureTopic(cfg.Brokers, cfg.Topic, 3); err != nil {
		log.Printf("[kafka-producer] warning: could not ensure topic '%s': %v (will retry on writes)", cfg.Topic, err)
	}

	writer := &kafkago.Writer{
		Addr:         kafkago.TCP(cfg.Brokers...),
		Topic:        cfg.Topic,
		Balancer:     &kafkago.Hash{}, // Hash by key (event ID) for partition affinity
		BatchSize:    100,
		BatchTimeout: 10 * time.Millisecond,
		WriteTimeout: 10 * time.Second,
		ReadTimeout:  10 * time.Second,
		RequiredAcks: kafkago.RequireOne, // Wait for leader ack (balance between durability and speed)
		Async:        false,              // Synchronous writes for correctness in API responses
	}

	log.Printf("[kafka-producer] initialized for topic '%s' with brokers %v", cfg.Topic, cfg.Brokers)

	return &Producer{
		writer: writer,
		config: cfg,
	}, nil
}

// PublishCreate publishes a create event message to Kafka
func (p *Producer) PublishCreate(ctx context.Context, eventID string, payload *models.EventPayload) error {
	msg := models.KafkaMessage{
		Operation: models.OpCreate,
		EventID:   eventID,
		Timestamp: time.Now().UTC(),
		Data:      payload,
	}
	return p.publish(ctx, eventID, msg)
}

// PublishUpdate publishes an update event message to Kafka
func (p *Producer) PublishUpdate(ctx context.Context, eventID string, payload *models.EventPayload) error {
	msg := models.KafkaMessage{
		Operation: models.OpUpdate,
		EventID:   eventID,
		Timestamp: time.Now().UTC(),
		Data:      payload,
	}
	return p.publish(ctx, eventID, msg)
}

//  publishes a delete event message to Kafka
func (p *Producer) PublishDelete(ctx context.Context, eventID string) error {
	msg := models.KafkaMessage{
		Operation: models.OpDelete,
		EventID:   eventID,
		Timestamp: time.Now().UTC(),
	}
	return p.publish(ctx, eventID, msg)
}

// publish serializes and sends a KafkaMessage
func (p *Producer) publish(ctx context.Context, key string, msg models.KafkaMessage) error {
	value, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal kafka message: %w", err)
	}

	kafkaMsg := kafkago.Message{
		Key:   []byte(key),
		Value: value,
	}

	if err := p.writer.WriteMessages(ctx, kafkaMsg); err != nil {
		return fmt.Errorf("failed to write message to kafka: %w", err)
	}

	log.Printf("[kafka-producer] published %s for event %s", msg.Operation, msg.EventID)
	return nil
}

// Close closes the Kafka writer
func (p *Producer) Close() error {
	log.Println("[kafka-producer] closing writer")
	return p.writer.Close()
}

// EnsureTopic creates the Kafka topic if it doesn't exist.
// Uses kafka-go's direct connection to create topics programmatically.
func EnsureTopic(brokers []string, topic string, numPartitions int) error {
	// Try to connect to the controller to create the topic
	var lastErr error
	for attempt := 0; attempt < 30; attempt++ {
		conn, err := kafkago.Dial("tcp", brokers[0])
		if err != nil {
			lastErr = err
			log.Printf("[kafka] waiting for broker... attempt %d/30: %v", attempt+1, err)
			time.Sleep(2 * time.Second)
			continue
		}

		controller, err := conn.Controller()
		if err != nil {
			conn.Close()
			lastErr = err
			log.Printf("[kafka] failed to get controller, attempt %d/30: %v", attempt+1, err)
			time.Sleep(2 * time.Second)
			continue
		}
		conn.Close()

		controllerConn, err := kafkago.Dial("tcp", fmt.Sprintf("%s:%d", controller.Host, controller.Port))
		if err != nil {
			lastErr = err
			time.Sleep(2 * time.Second)
			continue
		}
		defer controllerConn.Close()

		err = controllerConn.CreateTopics(kafkago.TopicConfig{
			Topic:             topic,
			NumPartitions:     numPartitions,
			ReplicationFactor: 1,
		})
		if err != nil {
			// Topic might already exist, which is fine
			log.Printf("[kafka] create topic result: %v", err)
		}

		log.Printf("[kafka] topic '%s' ensured with %d partitions", topic, numPartitions)
		return nil
	}

	return fmt.Errorf("failed to ensure topic after retries: %w", lastErr)
}
