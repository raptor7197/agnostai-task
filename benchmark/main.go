package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Config for the benchmark
type Config struct {
	APIBaseURL  string
	InsertCount int
	UpdateCount int
	DeleteCount int
	Concurrency int
	HTTPTimeout time.Duration
}

// CreateEventRequest mirrors the API request
type CreateEventRequest struct {
	ConversationID       string    `json:"conversation_id"`
	ConversationMetadata string    `json:"conversation_metadata,omitempty"`
	EventName            string    `json:"event_name"`
	Error                string    `json:"error,omitempty"`
	Latency              float64   `json:"latency,omitempty"`
	Input                string    `json:"input,omitempty"`
	InputEmbeddings      []float32 `json:"input_embeddings,omitempty"`
	Output               string    `json:"output,omitempty"`
	OutputEmbeddings     []float32 `json:"output_embeddings,omitempty"`
}

// UpdateEventRequest mirrors the API request
type UpdateEventRequest struct {
	Error   *string  `json:"error,omitempty"`
	Latency *float64 `json:"latency,omitempty"`
	Output  *string  `json:"output,omitempty"`
}

// APIResponse mirrors the API response
type APIResponse struct {
	Success bool                   `json:"success"`
	Message string                 `json:"message,omitempty"`
	Data    map[string]interface{} `json:"data,omitempty"`
	Error   string                 `json:"error,omitempty"`
}

var (
	eventNames    = []string{"event_a", "event_b", "event_c"}
	sampleErrors  = []string{"", "", "", "", "timeout", "connection_refused", "internal_error", "bad_request", "rate_limited", ""}
	sampleInputs  = []string{"hello world", "test input", "benchmark data", "sample query", "user message"}
	sampleOutputs = []string{"response 1", "result ok", "processed", "answer here", "completion"}
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	cfg := Config{
		APIBaseURL:  getEnv("API_BASE_URL", "http://localhost:8080"),
		InsertCount: getEnvInt("INSERT_COUNT", 500000),
		UpdateCount: getEnvInt("UPDATE_COUNT", 200000),
		DeleteCount: getEnvInt("DELETE_COUNT", 200000),
		Concurrency: getEnvInt("CONCURRENCY", 100),
		HTTPTimeout: 30 * time.Second,
	}

	log.Printf("=== Event Analytics Pipeline Benchmark ===")
	log.Printf("API Base URL:  %s", cfg.APIBaseURL)
	log.Printf("Insert Count:  %d", cfg.InsertCount)
	log.Printf("Update Count:  %d", cfg.UpdateCount)
	log.Printf("Delete Count:  %d", cfg.DeleteCount)
	log.Printf("Concurrency:   %d", cfg.Concurrency)
	log.Println()

	// Wait for API to be ready
	waitForAPI(cfg.APIBaseURL, 60)

	// Phase 1: Insert 500k events
	log.Println("========================================")
	log.Println("PHASE 1: INSERTING EVENTS")
	log.Println("========================================")
	eventIDs := runInserts(cfg)

	// Small pause to let consumer catch up a bit
	log.Println("\nWaiting 5 seconds for consumer to process inserts...")
	time.Sleep(5 * time.Second)

	// Shuffle event IDs for realistic access patterns
	rand.Shuffle(len(eventIDs), func(i, j int) {
		eventIDs[i], eventIDs[j] = eventIDs[j], eventIDs[i]
	})

	// Phase 2: Update 200k events
	log.Println("\n========================================")
	log.Println("PHASE 2: UPDATING EVENTS")
	log.Println("========================================")
	updateIDs := eventIDs[:cfg.UpdateCount]
	runUpdates(cfg, updateIDs)

	// Small pause
	log.Println("\nWaiting 5 seconds for consumer to process updates...")
	time.Sleep(5 * time.Second)

	// Phase 3: Delete 200k events
	log.Println("\n========================================")
	log.Println("PHASE 3: DELETING EVENTS")
	log.Println("========================================")
	// Use different events from the ones updated where possible
	deleteIDs := eventIDs[cfg.UpdateCount : cfg.UpdateCount+cfg.DeleteCount]
	runDeletes(cfg, deleteIDs)

	log.Println("\n========================================")
	log.Println("BENCHMARK COMPLETE")
	log.Println("========================================")
}

func runInserts(cfg Config) []string {
	client := &http.Client{Timeout: cfg.HTTPTimeout}
	eventIDs := make([]string, cfg.InsertCount)
	var mu sync.Mutex

	var successCount int64
	var errorCount int64

	sem := make(chan struct{}, cfg.Concurrency)
	var wg sync.WaitGroup

	start := time.Now()
	lastReport := time.Now()
	var lastReportCount int64

	// Progress reporter
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				current := atomic.LoadInt64(&successCount)
				errors := atomic.LoadInt64(&errorCount)
				elapsed := time.Since(start)
				sinceLastReport := time.Since(lastReport)
				recentRate := float64(current-lastReportCount) / sinceLastReport.Seconds()
				overallRate := float64(current) / elapsed.Seconds()
				log.Printf("[INSERT] progress: %d/%d (errors: %d) | recent: %.0f/s | overall: %.0f/s | elapsed: %v",
					current, cfg.InsertCount, errors, recentRate, overallRate, elapsed.Round(time.Second))
				lastReport = time.Now()
				lastReportCount = current
			}
		}
	}()

	for i := 0; i < cfg.InsertCount; i++ {
		wg.Add(1)
		sem <- struct{}{}

		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()

			convID := fmt.Sprintf("conv_%d", rand.Intn(cfg.InsertCount/10)) // ~10 events per conversation
			req := CreateEventRequest{
				ConversationID:       convID,
				ConversationMetadata: fmt.Sprintf(`{"user_id": "user_%d", "session": %d}`, rand.Intn(1000), rand.Intn(100)),
				EventName:            eventNames[rand.Intn(len(eventNames))],
				Error:                sampleErrors[rand.Intn(len(sampleErrors))],
				Latency:              rand.Float64() * 5000, // 0-5000ms
				Input:                sampleInputs[rand.Intn(len(sampleInputs))],
				Output:               sampleOutputs[rand.Intn(len(sampleOutputs))],
			}

			body, _ := json.Marshal(req)
			resp, err := client.Post(cfg.APIBaseURL+"/event/", "application/json", bytes.NewReader(body))
			if err != nil {
				atomic.AddInt64(&errorCount, 1)
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusOK {
				var apiResp APIResponse
				respBody, _ := io.ReadAll(resp.Body)
				if err := json.Unmarshal(respBody, &apiResp); err == nil && apiResp.Data != nil {
					if id, ok := apiResp.Data["id"].(string); ok {
						mu.Lock()
						eventIDs[idx] = id
						mu.Unlock()
					}
				}
				atomic.AddInt64(&successCount, 1)
			} else {
				io.Copy(io.Discard, resp.Body)
				atomic.AddInt64(&errorCount, 1)
			}
		}(i)
	}

	wg.Wait()
	close(done)

	elapsed := time.Since(start)
	rate := float64(successCount) / elapsed.Seconds()

	log.Printf("\n--- INSERT RESULTS ---")
	log.Printf("Total:     %d events", cfg.InsertCount)
	log.Printf("Success:   %d", successCount)
	log.Printf("Errors:    %d", errorCount)
	log.Printf("Duration:  %v", elapsed.Round(time.Millisecond))
	log.Printf("Rate:      %.2f events/sec", rate)

	// Filter out empty IDs (from errors)
	var validIDs []string
	for _, id := range eventIDs {
		if id != "" {
			validIDs = append(validIDs, id)
		}
	}

	return validIDs
}

func runUpdates(cfg Config, eventIDs []string) {
	client := &http.Client{Timeout: cfg.HTTPTimeout}

	var successCount int64
	var errorCount int64

	sem := make(chan struct{}, cfg.Concurrency)
	var wg sync.WaitGroup

	start := time.Now()
	lastReport := time.Now()
	var lastReportCount int64

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				current := atomic.LoadInt64(&successCount)
				errors := atomic.LoadInt64(&errorCount)
				elapsed := time.Since(start)
				sinceLastReport := time.Since(lastReport)
				recentRate := float64(current-lastReportCount) / sinceLastReport.Seconds()
				overallRate := float64(current) / elapsed.Seconds()
				log.Printf("[UPDATE] progress: %d/%d (errors: %d) | recent: %.0f/s | overall: %.0f/s | elapsed: %v",
					current, len(eventIDs), errors, recentRate, overallRate, elapsed.Round(time.Second))
				lastReport = time.Now()
				lastReportCount = current
			}
		}
	}()

	for _, id := range eventIDs {
		wg.Add(1)
		sem <- struct{}{}

		go func(eventID string) {
			defer wg.Done()
			defer func() { <-sem }()

			newError := sampleErrors[rand.Intn(len(sampleErrors))]
			newLatency := rand.Float64() * 5000
			newOutput := fmt.Sprintf("updated_output_%d", rand.Intn(10000))

			req := UpdateEventRequest{
				Error:   &newError,
				Latency: &newLatency,
				Output:  &newOutput,
			}

			body, _ := json.Marshal(req)
			httpReq, _ := http.NewRequest(http.MethodPut, cfg.APIBaseURL+"/event/"+eventID, bytes.NewReader(body))
			httpReq.Header.Set("Content-Type", "application/json")

			resp, err := client.Do(httpReq)
			if err != nil {
				atomic.AddInt64(&errorCount, 1)
				return
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)

			if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusOK {
				atomic.AddInt64(&successCount, 1)
			} else {
				atomic.AddInt64(&errorCount, 1)
			}
		}(id)
	}

	wg.Wait()
	close(done)

	elapsed := time.Since(start)
	rate := float64(successCount) / elapsed.Seconds()

	log.Printf("\n--- UPDATE RESULTS ---")
	log.Printf("Total:     %d events", len(eventIDs))
	log.Printf("Success:   %d", successCount)
	log.Printf("Errors:    %d", errorCount)
	log.Printf("Duration:  %v", elapsed.Round(time.Millisecond))
	log.Printf("Rate:      %.2f events/sec", rate)
}

func runDeletes(cfg Config, eventIDs []string) {
	client := &http.Client{Timeout: cfg.HTTPTimeout}

	var successCount int64
	var errorCount int64

	sem := make(chan struct{}, cfg.Concurrency)
	var wg sync.WaitGroup

	start := time.Now()
	lastReport := time.Now()
	var lastReportCount int64

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				current := atomic.LoadInt64(&successCount)
				errors := atomic.LoadInt64(&errorCount)
				elapsed := time.Since(start)
				sinceLastReport := time.Since(lastReport)
				recentRate := float64(current-lastReportCount) / sinceLastReport.Seconds()
				overallRate := float64(current) / elapsed.Seconds()
				log.Printf("[DELETE] progress: %d/%d (errors: %d) | recent: %.0f/s | overall: %.0f/s | elapsed: %v",
					current, len(eventIDs), errors, recentRate, overallRate, elapsed.Round(time.Second))
				lastReport = time.Now()
				lastReportCount = current
			}
		}
	}()

	for _, id := range eventIDs {
		wg.Add(1)
		sem <- struct{}{}

		go func(eventID string) {
			defer wg.Done()
			defer func() { <-sem }()

			httpReq, _ := http.NewRequest(http.MethodDelete, cfg.APIBaseURL+"/event/"+eventID, nil)
			resp, err := client.Do(httpReq)
			if err != nil {
				atomic.AddInt64(&errorCount, 1)
				return
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)

			if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusOK {
				atomic.AddInt64(&successCount, 1)
			} else {
				atomic.AddInt64(&errorCount, 1)
			}
		}(id)
	}

	wg.Wait()
	close(done)

	elapsed := time.Since(start)
	rate := float64(successCount) / elapsed.Seconds()

	log.Printf("\n--- DELETE RESULTS ---")
	log.Printf("Total:     %d events", len(eventIDs))
	log.Printf("Success:   %d", successCount)
	log.Printf("Errors:    %d", errorCount)
	log.Printf("Duration:  %v", elapsed.Round(time.Millisecond))
	log.Printf("Rate:      %.2f events/sec", rate)
}

func waitForAPI(baseURL string, maxRetries int) {
	log.Printf("Waiting for API at %s to be ready...", baseURL)
	client := &http.Client{Timeout: 5 * time.Second}

	for i := 0; i < maxRetries; i++ {
		resp, err := client.Get(baseURL + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				log.Println("API is ready!")
				return
			}
		}
		log.Printf("API not ready yet (attempt %d/%d)...", i+1, maxRetries)
		time.Sleep(2 * time.Second)
	}

	log.Fatal("API did not become ready in time")
}

func getEnv(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value, exists := os.LookupEnv(key); exists {
		var result int
		_, err := fmt.Sscanf(value, "%d", &result)
		if err == nil {
			return result
		}
	}
	return defaultValue
}
