package clickhouse

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/krxsna/agnostai-task/internal/models"
)

// Repository provides data access methods for events in ClickHouse
type Repository struct {
	client *Client
}

// NewRepository creates a new ClickHouse repository
func NewRepository(client *Client) *Repository {
	return &Repository{client: client}
}

// InsertEvent inserts a new event into ClickHouse
func (r *Repository) InsertEvent(ctx context.Context, event *models.Event) error {
	query := `
		INSERT INTO events (
			id, conversation_id, conversation_metadata, event_name,
			error, latency, input, input_embeddings,
			output, output_embeddings, is_deleted, version,
			created_at, updated_at
		) VALUES (
			?, ?, ?, ?,
			?, ?, ?, ?,
			?, ?, ?, ?,
			?, ?
		)
	`

	err := r.client.Conn.Exec(ctx, query,
		event.ID,
		event.ConversationID,
		event.ConversationMetadata,
		event.EventName,
		event.Error,
		event.Latency,
		event.Input,
		event.InputEmbeddings,
		event.Output,
		event.OutputEmbeddings,
		event.IsDeleted,
		event.Version,
		event.CreatedAt,
		event.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to insert event %s: %w", event.ID, err)
	}
	return nil
}

//   deduplicate by keeping the highest version.
func (r *Repository) UpdateEvent(ctx context.Context, eventID string, payload *models.EventPayload) error {
	// First, fetch the current version of the event
	existing, err := r.GetEventByID(ctx, eventID)
	if err != nil {
		return fmt.Errorf("failed to fetch event for update: %w", err)
	}
	if existing == nil {
		return fmt.Errorf("event %s not found", eventID)
	}
	if existing.IsDeleted == 1 {
		return fmt.Errorf("event %s is deleted", eventID)
	}

	if payload.ConversationID != "" {
		existing.ConversationID = payload.ConversationID
	}
	if payload.ConversationMetadata != "" {
		existing.ConversationMetadata = payload.ConversationMetadata
	}
	if payload.EventName != "" {
		existing.EventName = payload.EventName
	}
	if payload.Error != nil {
		existing.Error = *payload.Error
	}
	if payload.Latency != nil {
		existing.Latency = *payload.Latency
	}
	if payload.Input != "" {
		existing.Input = payload.Input
	}
	if payload.InputEmbeddings != nil {
		existing.InputEmbeddings = payload.InputEmbeddings
	}
	if payload.Output != "" {
		existing.Output = payload.Output
	}
	if payload.OutputEmbeddings != nil {
		existing.OutputEmbeddings = payload.OutputEmbeddings
	}

	// Increment version and update timestamp
	existing.Version++
	existing.UpdatedAt = time.Now()

	return r.InsertEvent(ctx, existing)
}

// DeleteEvent performs a soft delete by inserting a new version with is_deleted = 1
func (r *Repository) DeleteEvent(ctx context.Context, eventID string) error {
	existing, err := r.GetEventByID(ctx, eventID)
	if err != nil {
		return fmt.Errorf("failed to fetch event for delete: %w", err)
	}
	if existing == nil {
		return fmt.Errorf("event %s not found", eventID)
	}
	if existing.IsDeleted == 1 {
		return fmt.Errorf("event %s is already deleted", eventID)
	}

	existing.Version++
	existing.IsDeleted = 1
	existing.UpdatedAt = time.Now()

	return r.InsertEvent(ctx, existing)
}

// Uses FINAL to get the latest version after ReplacingMergeTree deduplication.
func (r *Repository) GetEventByID(ctx context.Context, eventID string) (*models.Event, error) {
	query := `
		SELECT
			id, conversation_id, conversation_metadata, event_name,
			error, latency, input, input_embeddings,
			output, output_embeddings, is_deleted, version,
			created_at, updated_at
		FROM events FINAL
		WHERE id = ? AND is_deleted = 0
		LIMIT 1
	`

	row := r.client.Conn.QueryRow(ctx, query, eventID)

	var event models.Event
	err := row.Scan(
		&event.ID,
		&event.ConversationID,
		&event.ConversationMetadata,
		&event.EventName,
		&event.Error,
		&event.Latency,
		&event.Input,
		&event.InputEmbeddings,
		&event.Output,
		&event.OutputEmbeddings,
		&event.IsDeleted,
		&event.Version,
		&event.CreatedAt,
		&event.UpdatedAt,
	)
	if err != nil {
		// clickhouse-go returns a specific error for no rows; check the error message
		if err.Error() == "sql: no rows in result set" || err.Error() == "EOF" {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to scan event: %w", err)
	}

	return &event, nil
}

// this retrieves a single event by ID including soft-deleted ones.
// Used internally by update/delete operations.
func (r *Repository) GetEventByIDIncludeDeleted(ctx context.Context, eventID string) (*models.Event, error) {
	query := `
		SELECT
			id, conversation_id, conversation_metadata, event_name,
			error, latency, input, input_embeddings,
			output, output_embeddings, is_deleted, version,
			created_at, updated_at
		FROM events FINAL
		WHERE id = ?
		LIMIT 1
	`

	row := r.client.Conn.QueryRow(ctx, query, eventID)

	var event models.Event
	err := row.Scan(
		&event.ID,
		&event.ConversationID,
		&event.ConversationMetadata,
		&event.EventName,
		&event.Error,
		&event.Latency,
		&event.Input,
		&event.InputEmbeddings,
		&event.Output,
		&event.OutputEmbeddings,
		&event.IsDeleted,
		&event.Version,
		&event.CreatedAt,
		&event.UpdatedAt,
	)
	if err != nil {
		if err.Error() == "sql: no rows in result set" || err.Error() == "EOF" {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to scan event: %w", err)
	}

	return &event, nil
}

//  returns all events for a given conversation, ordered by creation time
func (r *Repository) GetEventsByConversation(ctx context.Context, conversationID string) ([]models.Event, error) {
	query := `
		SELECT
			id, conversation_id, conversation_metadata, event_name,
			error, latency, input, input_embeddings,
			output, output_embeddings, is_deleted, version,
			created_at, updated_at
		FROM events FINAL
		WHERE conversation_id = ? AND is_deleted = 0
		ORDER BY created_at ASC
	`

	rows, err := r.client.Conn.Query(ctx, query, conversationID)
	if err != nil {
		return nil, fmt.Errorf("failed to query events by conversation: %w", err)
	}
	defer rows.Close()

	var events []models.Event
	for rows.Next() {
		var event models.Event
		if err := rows.Scan(
			&event.ID,
			&event.ConversationID,
			&event.ConversationMetadata,
			&event.EventName,
			&event.Error,
			&event.Latency,
			&event.Input,
			&event.InputEmbeddings,
			&event.Output,
			&event.OutputEmbeddings,
			&event.IsDeleted,
			&event.Version,
			&event.CreatedAt,
			&event.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan event row: %w", err)
		}
		events = append(events, event)
	}

	return events, nil
}

// ErrorCountResult holds the result for error count queries
type ErrorCountResult struct {
	Date       string `json:"date"`
	EventName  string `json:"event_name"`
	ErrorCount uint64 `json:"error_count"`
	TotalCount uint64 `json:"total_count"`
}

// GetErrorCountsByEventName returns error counts for a given event name over the last N days.
// Query: "How many errors in event_a over the last 7 days?"
func (r *Repository) GetErrorCountsByEventName(ctx context.Context, eventName string, days int) ([]ErrorCountResult, error) {
	query := `
		SELECT
			toDate(created_at) AS event_date,
			event_name,
			countIf(error != '') AS error_count,
			count() AS total_count
		FROM events FINAL
		WHERE event_name = ?
			AND created_at >= now() - INTERVAL ? DAY
			AND is_deleted = 0
		GROUP BY event_date, event_name
		ORDER BY event_date ASC
	`

	rows, err := r.client.Conn.Query(ctx, query, eventName, days)
	if err != nil {
		return nil, fmt.Errorf("failed to query error counts: %w", err)
	}
	defer rows.Close()

	var results []ErrorCountResult
	for rows.Next() {
		var r ErrorCountResult
		var eventDate time.Time
		if err := rows.Scan(&eventDate, &r.EventName, &r.ErrorCount, &r.TotalCount); err != nil {
			return nil, fmt.Errorf("failed to scan error count row: %w", err)
		}
		r.Date = eventDate.Format("2006-01-02")
		results = append(results, r)
	}

	return results, nil
}

// LatencyStatsResult holds p95 latency results
type LatencyStatsResult struct {
	ConversationID string  `json:"conversation_id"`
	EventName      string  `json:"event_name"`
	P95Latency     float64 `json:"p95_latency"`
	EventCount     uint64  `json:"event_count"`
}

// GetP95LatencyPerEventPerSession returns p95 latency per event type per session.
// Query: "p95 latency per event type per session"
func (r *Repository) GetP95LatencyPerEventPerSession(ctx context.Context, limit int) ([]LatencyStatsResult, error) {
	if limit <= 0 {
		limit = 100
	}

	query := `
		SELECT
			conversation_id,
			event_name,
			quantile(0.95)(latency) AS p95_latency,
			count() AS event_count
		FROM events FINAL
		WHERE is_deleted = 0
		GROUP BY conversation_id, event_name
		ORDER BY p95_latency DESC
		LIMIT ?
	`

	rows, err := r.client.Conn.Query(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query latency stats: %w", err)
	}
	defer rows.Close()

	var results []LatencyStatsResult
	for rows.Next() {
		var result LatencyStatsResult
		if err := rows.Scan(
			&result.ConversationID,
			&result.EventName,
			&result.P95Latency,
			&result.EventCount,
		); err != nil {
			return nil, fmt.Errorf("failed to scan latency row: %w", err)
		}
		results = append(results, result)
	}

	return results, nil
}

// SessionErrorRateResult holds error rate per session
type SessionErrorRateResult struct {
	ConversationID string  `json:"conversation_id"`
	TotalEvents    uint64  `json:"total_events"`
	ErrorEvents    uint64  `json:"error_events"`
	ErrorRate      float64 `json:"error_rate"`
}

// GetTopSessionsByErrorRate returns top sessions by error rate.
// Query: "Top sessions by error rate"
func (r *Repository) GetTopSessionsByErrorRate(ctx context.Context, limit int) ([]SessionErrorRateResult, error) {
	if limit <= 0 {
		limit = 100
	}

	query := `
		SELECT
			conversation_id,
			count() AS total_events,
			countIf(error != '') AS error_events,
			error_events / total_events AS error_rate
		FROM events FINAL
		WHERE is_deleted = 0
		GROUP BY conversation_id
		HAVING total_events > 0
		ORDER BY error_rate DESC, total_events DESC
		LIMIT ?
	`

	rows, err := r.client.Conn.Query(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query session error rates: %w", err)
	}
	defer rows.Close()

	var results []SessionErrorRateResult
	for rows.Next() {
		var result SessionErrorRateResult
		if err := rows.Scan(
			&result.ConversationID,
			&result.TotalEvents,
			&result.ErrorEvents,
			&result.ErrorRate,
		); err != nil {
			return nil, fmt.Errorf("failed to scan session error rate row: %w", err)
		}
		results = append(results, result)
	}

	return results, nil
}

// BatchInsertEvents inserts multiple events in a batch for better performance.
// Used by the consumer for batch processing.
func (r *Repository) BatchInsertEvents(ctx context.Context, events []*models.Event) error {
	if len(events) == 0 {
		return nil
	}

	batch, err := r.client.Conn.PrepareBatch(ctx, `
		INSERT INTO events (
			id, conversation_id, conversation_metadata, event_name,
			error, latency, input, input_embeddings,
			output, output_embeddings, is_deleted, version,
			created_at, updated_at
		)
	`)
	if err != nil {
		return fmt.Errorf("failed to prepare batch: %w", err)
	}

	for _, event := range events {
		err := batch.Append(
			event.ID,
			event.ConversationID,
			event.ConversationMetadata,
			event.EventName,
			event.Error,
			event.Latency,
			event.Input,
			event.InputEmbeddings,
			event.Output,
			event.OutputEmbeddings,
			event.IsDeleted,
			event.Version,
			event.CreatedAt,
			event.UpdatedAt,
		)
		if err != nil {
			return fmt.Errorf("failed to append to batch: %w", err)
		}
	}

	if err := batch.Send(); err != nil {
		return fmt.Errorf("failed to send batch: %w", err)
	}

	log.Printf("[clickhouse] batch inserted %d events", len(events))
	return nil
}
