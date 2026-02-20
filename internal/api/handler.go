package api

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/krxsna/agnostai-task/internal/clickhouse"
	"github.com/krxsna/agnostai-task/internal/kafka"
	"github.com/krxsna/agnostai-task/internal/models"
)

// Handler holds dependencies for HTTP handlers
type Handler struct {
	producer *kafka.Producer
	repo     *clickhouse.Repository
}

// NewHandler creates a new Handler with the given dependencies
func NewHandler(producer *kafka.Producer, repo *clickhouse.Repository) *Handler {
	return &Handler{
		producer: producer,
		repo:     repo,
	}
}

// respondJSON writes a JSON response with the given status code
func respondJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if data != nil {
		if err := json.NewEncoder(w).Encode(data); err != nil {
			log.Printf("[api] error encoding response: %v", err)
		}
	}
}

// respondError writes a JSON error response
func respondError(w http.ResponseWriter, status int, message string) {
	respondJSON(w, status, models.APIResponse{
		Success: false,
		Error:   message,
	})
}

// CreateEvent handles POST /event
// Validates the request, generates an event ID, and publishes a create message to Kafka.
// The Kafka consumer will pick it up and write to ClickHouse asynchronously.
func (h *Handler) CreateEvent(w http.ResponseWriter, r *http.Request) {
	var req models.CreateEventRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	// Validate required fields
	if req.ConversationID == "" {
		respondError(w, http.StatusBadRequest, "conversation_id is required")
		return
	}
	if req.EventName == "" {
		respondError(w, http.StatusBadRequest, "event_name is required")
		return
	}
	if !models.IsValidEventName(req.EventName) {
		respondError(w, http.StatusBadRequest, "event_name must be one of: event_a, event_b, event_c")
		return
	}

	// Generate a new event ID
	eventID := models.NewEventID()

	// Build the Kafka payload
	latency := req.Latency
	reqError := req.Error
	payload := &models.EventPayload{
		ConversationID:       req.ConversationID,
		ConversationMetadata: req.ConversationMetadata,
		EventName:            req.EventName,
		Error:                &reqError,
		Latency:              &latency,
		Input:                req.Input,
		InputEmbeddings:      req.InputEmbeddings,
		Output:               req.Output,
		OutputEmbeddings:     req.OutputEmbeddings,
	}

	// Publish create message to Kafka
	if err := h.producer.PublishCreate(r.Context(), eventID, payload); err != nil {
		log.Printf("[api] failed to publish create event: %v", err)
		respondError(w, http.StatusInternalServerError, "failed to publish event: "+err.Error())
		return
	}

	respondJSON(w, http.StatusAccepted, models.APIResponse{
		Success: true,
		Message: "event creation queued",
		Data: map[string]string{
			"id": eventID,
		},
	})
}

// Publishes an update message to Kafka. The consumer will read the current state
// from ClickHouse, merge the updates, and insert a new version.
func (h *Handler) UpdateEvent(w http.ResponseWriter, r *http.Request) {
	eventID := chi.URLParam(r, "id")
	if eventID == "" {
		respondError(w, http.StatusBadRequest, "event id is required")
		return
	}

	var req models.UpdateEventRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	// Validate event_name if provided
	if req.EventName != nil && !models.IsValidEventName(*req.EventName) {
		respondError(w, http.StatusBadRequest, "event_name must be one of: event_a, event_b, event_c")
		return
	}

	// Build the Kafka payload from non-nil fields
	payload := &models.EventPayload{}
	if req.ConversationMetadata != nil {
		payload.ConversationMetadata = *req.ConversationMetadata
	}
	if req.EventName != nil {
		payload.EventName = *req.EventName
	}
	if req.Error != nil {
		payload.Error = req.Error
	}
	if req.Latency != nil {
		payload.Latency = req.Latency
	}
	if req.Input != nil {
		payload.Input = *req.Input
	}
	if req.InputEmbeddings != nil {
		payload.InputEmbeddings = req.InputEmbeddings
	}
	if req.Output != nil {
		payload.Output = *req.Output
	}
	if req.OutputEmbeddings != nil {
		payload.OutputEmbeddings = req.OutputEmbeddings
	}

	// Publish update message to Kafka
	if err := h.producer.PublishUpdate(r.Context(), eventID, payload); err != nil {
		log.Printf("[api] failed to publish update event: %v", err)
		respondError(w, http.StatusInternalServerError, "failed to publish update: "+err.Error())
		return
	}

	respondJSON(w, http.StatusAccepted, models.APIResponse{
		Success: true,
		Message: "event update queued",
		Data: map[string]string{
			"id": eventID,
		},
	})
}

// Publishes a delete message to Kafka. The consumer will perform a soft delete
// by inserting a new version with is_deleted = 1.
func (h *Handler) DeleteEvent(w http.ResponseWriter, r *http.Request) {
	eventID := chi.URLParam(r, "id")
	if eventID == "" {
		respondError(w, http.StatusBadRequest, "event id is required")
		return
	}

	// Publish delete message to Kafka
	if err := h.producer.PublishDelete(r.Context(), eventID); err != nil {
		log.Printf("[api] failed to publish delete event: %v", err)
		respondError(w, http.StatusInternalServerError, "failed to publish delete: "+err.Error())
		return
	}

	respondJSON(w, http.StatusAccepted, models.APIResponse{
		Success: true,
		Message: "event deletion queued",
		Data: map[string]string{
			"id": eventID,
		},
	})
}

// GetEvent handles GET /event/:id
// Reads directly from ClickHouse (not through Kafka).
func (h *Handler) GetEvent(w http.ResponseWriter, r *http.Request) {
	eventID := chi.URLParam(r, "id")
	if eventID == "" {
		respondError(w, http.StatusBadRequest, "event id is required")
		return
	}

	event, err := h.repo.GetEventByID(r.Context(), eventID)
	if err != nil {
		log.Printf("[api] failed to get event %s: %v", eventID, err)
		respondError(w, http.StatusInternalServerError, "failed to get event: "+err.Error())
		return
	}
	if event == nil {
		respondError(w, http.StatusNotFound, "event not found")
		return
	}

	respondJSON(w, http.StatusOK, models.APIResponse{
		Success: true,
		Data:    event,
	})
}

// GetEventsByConversation handles GET /events/conversation/:conversation_id
// Returns all events for a given conversation, ordered by creation time.
// Reads directly from ClickHouse.
func (h *Handler) GetEventsByConversation(w http.ResponseWriter, r *http.Request) {
	conversationID := chi.URLParam(r, "conversation_id")
	if conversationID == "" {
		respondError(w, http.StatusBadRequest, "conversation_id is required")
		return
	}

	events, err := h.repo.GetEventsByConversation(r.Context(), conversationID)
	if err != nil {
		log.Printf("[api] failed to get events for conversation %s: %v", conversationID, err)
		respondError(w, http.StatusInternalServerError, "failed to get events: "+err.Error())
		return
	}

	respondJSON(w, http.StatusOK, models.APIResponse{
		Success: true,
		Data:    events,
	})
}

// GetErrorCounts handles GET /analytics/errors?event_name=event_a&days=7
// Returns error counts per day for a given event name.
// Reads directly from ClickHouse.
func (h *Handler) GetErrorCounts(w http.ResponseWriter, r *http.Request) {
	eventName := r.URL.Query().Get("event_name")
	if eventName == "" {
		respondError(w, http.StatusBadRequest, "event_name query parameter is required")
		return
	}
	if !models.IsValidEventName(eventName) {
		respondError(w, http.StatusBadRequest, "event_name must be one of: event_a, event_b, event_c")
		return
	}

	daysStr := r.URL.Query().Get("days")
	days := 7 // default
	if daysStr != "" {
		var err error
		days, err = strconv.Atoi(daysStr)
		if err != nil || days <= 0 {
			respondError(w, http.StatusBadRequest, "days must be a positive integer")
			return
		}
	}

	results, err := h.repo.GetErrorCountsByEventName(r.Context(), eventName, days)
	if err != nil {
		log.Printf("[api] failed to get error counts: %v", err)
		respondError(w, http.StatusInternalServerError, "failed to get error counts: "+err.Error())
		return
	}

	respondJSON(w, http.StatusOK, models.APIResponse{
		Success: true,
		Data:    results,
	})
}

// GetLatencyStats handles GET /analytics/latency?limit=100
// Returns p95 latency per event type per session.
// Reads directly from ClickHouse.
func (h *Handler) GetLatencyStats(w http.ResponseWriter, r *http.Request) {
	limitStr := r.URL.Query().Get("limit")
	limit := 100 // default
	if limitStr != "" {
		var err error
		limit, err = strconv.Atoi(limitStr)
		if err != nil || limit <= 0 {
			respondError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
	}

	results, err := h.repo.GetP95LatencyPerEventPerSession(r.Context(), limit)
	if err != nil {
		log.Printf("[api] failed to get latency stats: %v", err)
		respondError(w, http.StatusInternalServerError, "failed to get latency stats: "+err.Error())
		return
	}

	respondJSON(w, http.StatusOK, models.APIResponse{
		Success: true,
		Data:    results,
	})
}

// GetTopSessionsByErrorRate handles GET /analytics/sessions/top-errors?limit=100
// Returns top sessions ranked by error rate.
// Reads directly from ClickHouse.
func (h *Handler) GetTopSessionsByErrorRate(w http.ResponseWriter, r *http.Request) {
	limitStr := r.URL.Query().Get("limit")
	limit := 100 // default
	if limitStr != "" {
		var err error
		limit, err = strconv.Atoi(limitStr)
		if err != nil || limit <= 0 {
			respondError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
	}

	results, err := h.repo.GetTopSessionsByErrorRate(r.Context(), limit)
	if err != nil {
		log.Printf("[api] failed to get session error rates: %v", err)
		respondError(w, http.StatusInternalServerError, "failed to get session error rates: "+err.Error())
		return
	}

	respondJSON(w, http.StatusOK, models.APIResponse{
		Success: true,
		Data:    results,
	})
}

// HealthCheck handles GET /health
func (h *Handler) HealthCheck(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, models.APIResponse{
		Success: true,
		Message: "ok",
	})
}
