package models

import (
	"time"

	"github.com/google/uuid"
)

// which event types are allowed 
type EventName string

const (
	EventA EventName = "event_a"
	EventB EventName = "event_b"
	EventC EventName = "event_c"
)

func IsValidEventName(name string) bool {
	switch EventName(name) {
	case EventA, EventB, EventC:
		return true
	}
	return false
}

//  core event data model
type Event struct {
	ID                   string    `json:"id" ch:"id"`
	ConversationID       string    `json:"conversation_id" ch:"conversation_id"`
	ConversationMetadata string    `json:"conversation_metadata" ch:"conversation_metadata"`
	EventName            string    `json:"event_name" ch:"event_name"`
	Error                string    `json:"error" ch:"error"`
	Latency              float64   `json:"latency" ch:"latency"`
	Input                string    `json:"input" ch:"input"`
	InputEmbeddings      []float32 `json:"input_embeddings" ch:"input_embeddings"`
	Output               string    `json:"output" ch:"output"`
	OutputEmbeddings     []float32 `json:"output_embeddings" ch:"output_embeddings"`
	IsDeleted            uint8     `json:"-" ch:"is_deleted"`
	Version              uint64    `json:"-" ch:"version"`
	CreatedAt            time.Time `json:"created_at" ch:"created_at"`
	UpdatedAt            time.Time `json:"updated_at" ch:"updated_at"`
}

//  type of Kafka message operation
type OperationType string

const (
	OpCreate OperationType = "create"
	OpUpdate OperationType = "update"
	OpDelete OperationType = "delete"
)

//  envelope for all event operations sent through Kafka
type KafkaMessage struct {
	Operation OperationType `json:"operation"`
	EventID   string        `json:"event_id"`
	Timestamp time.Time     `json:"timestamp"`
	Data      *EventPayload `json:"data,omitempty"`
}

// which data sent with create/update operations
type EventPayload struct {
	ConversationID       string    `json:"conversation_id,omitempty"`
	ConversationMetadata string    `json:"conversation_metadata,omitempty"`
	EventName            string    `json:"event_name,omitempty"`
	Error                *string   `json:"error,omitempty"`
	Latency              *float64  `json:"latency,omitempty"`
	Input                string    `json:"input,omitempty"`
	InputEmbeddings      []float32 `json:"input_embeddings,omitempty"`
	Output               string    `json:"output,omitempty"`
	OutputEmbeddings     []float32 `json:"output_embeddings,omitempty"`
}

// http req body 
type CreateEventRequest struct {
	ConversationID       string    `json:"conversation_id" validate:"required"`
	ConversationMetadata string    `json:"conversation_metadata,omitempty"`
	EventName            string    `json:"event_name" validate:"required"`
	Error                string    `json:"error,omitempty"`
	Latency              float64   `json:"latency,omitempty"`
	Input                string    `json:"input,omitempty"`
	InputEmbeddings      []float32 `json:"input_embeddings,omitempty"`
	Output               string    `json:"output,omitempty"`
	OutputEmbeddings     []float32 `json:"output_embeddings,omitempty"`
}

//  is the HTTP request body for PUT /event/:id
type UpdateEventRequest struct {
	ConversationMetadata *string   `json:"conversation_metadata,omitempty"`
	EventName            *string   `json:"event_name,omitempty"`
	Error                *string   `json:"error,omitempty"`
	Latency              *float64  `json:"latency,omitempty"`
	Input                *string   `json:"input,omitempty"`
	InputEmbeddings      []float32 `json:"input_embeddings,omitempty"`
	Output               *string   `json:"output,omitempty"`
	OutputEmbeddings     []float32 `json:"output_embeddings,omitempty"`
}

//   generic response wrapper
type APIResponse struct {
	Success bool        `json:"success"`
	Message string      `json:"message,omitempty"`
	Data    interface{} `json:"data,omitempty"`
	Error   string      `json:"error,omitempty"`
}

//  generates a new UUID for events
func NewEventID() string {
	return uuid.New().String()
}
