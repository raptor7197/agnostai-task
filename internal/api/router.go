package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/krxsna/agnostai-task/internal/clickhouse"
	"github.com/krxsna/agnostai-task/internal/kafka"
)

func NewRouter(producer *kafka.Producer, repo *clickhouse.Repository) http.Handler {
	h := NewHandler(producer, repo)

	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))
	r.Use(middleware.Heartbeat("/ping"))

// cors :(
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	})


// check for live ro dead
	r.Get("/health", h.HealthCheck)

// read ->ck
	r.Route("/event", func(r chi.Router) {
		r.Post("/", h.CreateEvent)       // POST 
		r.Get("/{id}", h.GetEvent)       //  ClickHouse
		r.Put("/{id}", h.UpdateEvent)    //  Kafka
		r.Delete("/{id}", h.DeleteEvent) //  Kafka
	})

	// Conversation queries (direct reads from ClickHouse)
	r.Get("/events/conversation/{conversation_id}", h.GetEventsByConversation)

	// Analytics endpoints (direct reads from ClickHouse)
	r.Route("/analytics", func(r chi.Router) {
		r.Get("/errors", h.GetErrorCounts)                         // GET /analytics/errors?event_name=event_a&days=7
		r.Get("/latency", h.GetLatencyStats)                       // GET /analytics/latency?limit=100
		r.Get("/sessions/top-errors", h.GetTopSessionsByErrorRate) // GET /analytics/sessions/top-errors?limit=100
	})

	return r
}
