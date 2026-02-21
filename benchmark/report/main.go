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
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

type BenchmarkConfig struct {
	APIBaseURL  string
	InsertCount int
	UpdateCount int
	DeleteCount int
	Concurrency int
	HTTPTimeout time.Duration
}

func loadConfig() BenchmarkConfig {
	return BenchmarkConfig{
		APIBaseURL:  envStr("API_BASE_URL", "http://localhost:8080"),
		InsertCount: envInt("INSERT_COUNT", 500_000),
		UpdateCount: envInt("UPDATE_COUNT", 200_000),
		DeleteCount: envInt("DELETE_COUNT", 200_000),
		Concurrency: envInt("CONCURRENCY", 150),
		HTTPTimeout: 30 * time.Second,
	}
}

// ---------------------------------------------------------------------------
// Request / Response types (mirrors the API models)
// ---------------------------------------------------------------------------

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

type UpdateEventRequest struct {
	Error   *string  `json:"error,omitempty"`
	Latency *float64 `json:"latency,omitempty"`
	Output  *string  `json:"output,omitempty"`
}

type APIResponse struct {
	Success bool                   `json:"success"`
	Message string                 `json:"message,omitempty"`
	Data    map[string]interface{} `json:"data,omitempty"`
	Error   string                 `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// Phase result tracking
// ---------------------------------------------------------------------------

type PhaseResult struct {
	Name      string
	Total     int
	Success   int64
	Errors    int64
	Duration  time.Duration
	StartedAt time.Time
	EndedAt   time.Time
}

func (r PhaseResult) Rate() float64 {
	if r.Duration.Seconds() == 0 {
		return 0
	}
	return float64(r.Success) / r.Duration.Seconds()
}

func (r PhaseResult) AvgLatencyMicros() float64 {
	if r.Success == 0 {
		return 0
	}
	return float64(r.Duration.Microseconds()) / float64(r.Success)
}

// ---------------------------------------------------------------------------
// Sample data pools
// ---------------------------------------------------------------------------

var (
	eventNames    = []string{"event_a", "event_b", "event_c"}
	sampleErrors  = []string{"", "", "", "", "", "timeout", "connection_refused", "internal_error", "bad_request", "rate_limited"}
	sampleInputs  = []string{"hello world", "test input", "benchmark data", "sample query", "user message", "search request", "api call", "webhook trigger", "cron job", "batch process"}
	sampleOutputs = []string{"response ok", "result found", "processed", "answer generated", "completion done", "data returned", "ack", "success", "queued", "done"}
)

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	cfg := loadConfig()

	printBanner()
	printConfig(cfg)

	// Wait for API readiness
	waitForAPI(cfg.APIBaseURL, 60)

	results := make([]PhaseResult, 0, 3)

	printPhaseHeader("PHASE 1 — INGEST", cfg.InsertCount)
	ids, ingestResult := runIngestPhase(cfg)
	results = append(results, ingestResult)
	printPhaseResult(ingestResult)

	log.Println()
	log.Println("⏳  Pausing 5 s to let the Kafka consumer drain inserts …")
	time.Sleep(5 * time.Second)

	// Shuffle IDs so updates and deletes hit different partitions randomly
	rand.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })

	// We need at least UpdateCount + DeleteCount valid IDs
	needed := cfg.UpdateCount + cfg.DeleteCount
	if len(ids) < needed {
		log.Printf("⚠  Only %d valid IDs collected (need %d). Adjusting counts.", len(ids), needed)
		half := len(ids) / 2
		cfg.DeleteCount = half
		cfg.UpdateCount = half
	}

	deleteIDs := ids[:cfg.DeleteCount]
	updateIDs := ids[cfg.DeleteCount : cfg.DeleteCount+cfg.UpdateCount]

	// ── Phase 2: Delete 200 k events ────────────────────────────────────
	printPhaseHeader("PHASE 2 — DELETE", cfg.DeleteCount)
	deleteResult := runDeletePhase(cfg, deleteIDs)
	results = append(results, deleteResult)
	printPhaseResult(deleteResult)

	log.Println()
	log.Println("⏳  Pausing 5 s to let the Kafka consumer drain deletes …")
	time.Sleep(5 * time.Second)

	// ── Phase 3: Update 200 k events ────────────────────────────────────
	printPhaseHeader("PHASE 3 — UPDATE", cfg.UpdateCount)
	updateResult := runUpdatePhase(cfg, updateIDs)
	results = append(results, updateResult)
	printPhaseResult(updateResult)

	// ── Final Summary ───────────────────────────────────────────────────
	printFinalReport(cfg, results)
}

// ---------------------------------------------------------------------------
// Phase runners
// ---------------------------------------------------------------------------

func runIngestPhase(cfg BenchmarkConfig) ([]string, PhaseResult) {
	client := &http.Client{Timeout: cfg.HTTPTimeout}
	ids := make([]string, cfg.InsertCount)
	var mu sync.Mutex

	var success, errors int64
	sem := make(chan struct{}, cfg.Concurrency)
	var wg sync.WaitGroup

	startTime := time.Now()

	// progress ticker
	done := startProgressTicker("INGEST", &success, &errors, int64(cfg.InsertCount), startTime)

	for i := 0; i < cfg.InsertCount; i++ {
		wg.Add(1)
		sem <- struct{}{}

		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()

			convID := fmt.Sprintf("conv_%d", rand.Intn(cfg.InsertCount/10))
			req := CreateEventRequest{
				ConversationID:       convID,
				ConversationMetadata: fmt.Sprintf(`{"user_id":"user_%d","session":%d}`, rand.Intn(1000), rand.Intn(100)),
				EventName:            eventNames[rand.Intn(len(eventNames))],
				Error:                sampleErrors[rand.Intn(len(sampleErrors))],
				Latency:              rand.Float64() * 5000,
				Input:                sampleInputs[rand.Intn(len(sampleInputs))],
				Output:               sampleOutputs[rand.Intn(len(sampleOutputs))],
			}

			body, _ := json.Marshal(req)
			resp, err := client.Post(cfg.APIBaseURL+"/event/", "application/json", bytes.NewReader(body))
			if err != nil {
				atomic.AddInt64(&errors, 1)
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusOK {
				var apiResp APIResponse
				respBody, _ := io.ReadAll(resp.Body)
				if err := json.Unmarshal(respBody, &apiResp); err == nil && apiResp.Data != nil {
					if id, ok := apiResp.Data["id"].(string); ok {
						mu.Lock()
						ids[idx] = id
						mu.Unlock()
					}
				}
				atomic.AddInt64(&success, 1)
			} else {
				io.Copy(io.Discard, resp.Body)
				atomic.AddInt64(&errors, 1)
			}
		}(i)
	}

	wg.Wait()
	close(done)
	elapsed := time.Since(startTime)

	// collect valid IDs
	valid := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" {
			valid = append(valid, id)
		}
	}

	return valid, PhaseResult{
		Name:      "INGEST",
		Total:     cfg.InsertCount,
		Success:   atomic.LoadInt64(&success),
		Errors:    atomic.LoadInt64(&errors),
		Duration:  elapsed,
		StartedAt: startTime,
		EndedAt:   startTime.Add(elapsed),
	}
}

func runDeletePhase(cfg BenchmarkConfig, ids []string) PhaseResult {
	client := &http.Client{Timeout: cfg.HTTPTimeout}
	var success, errors int64
	sem := make(chan struct{}, cfg.Concurrency)
	var wg sync.WaitGroup

	startTime := time.Now()
	done := startProgressTicker("DELETE", &success, &errors, int64(len(ids)), startTime)

	for _, id := range ids {
		wg.Add(1)
		sem <- struct{}{}

		go func(eventID string) {
			defer wg.Done()
			defer func() { <-sem }()

			req, _ := http.NewRequest(http.MethodDelete, cfg.APIBaseURL+"/event/"+eventID, nil)
			resp, err := client.Do(req)
			if err != nil {
				atomic.AddInt64(&errors, 1)
				return
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)

			if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusOK {
				atomic.AddInt64(&success, 1)
			} else {
				atomic.AddInt64(&errors, 1)
			}
		}(id)
	}

	wg.Wait()
	close(done)
	elapsed := time.Since(startTime)

	return PhaseResult{
		Name:      "DELETE",
		Total:     len(ids),
		Success:   atomic.LoadInt64(&success),
		Errors:    atomic.LoadInt64(&errors),
		Duration:  elapsed,
		StartedAt: startTime,
		EndedAt:   startTime.Add(elapsed),
	}
}

func runUpdatePhase(cfg BenchmarkConfig, ids []string) PhaseResult {
	client := &http.Client{Timeout: cfg.HTTPTimeout}
	var success, errors int64
	sem := make(chan struct{}, cfg.Concurrency)
	var wg sync.WaitGroup

	startTime := time.Now()
	done := startProgressTicker("UPDATE", &success, &errors, int64(len(ids)), startTime)

	for _, id := range ids {
		wg.Add(1)
		sem <- struct{}{}

		go func(eventID string) {
			defer wg.Done()
			defer func() { <-sem }()

			newErr := sampleErrors[rand.Intn(len(sampleErrors))]
			newLat := rand.Float64() * 5000
			newOut := fmt.Sprintf("updated_output_%d", rand.Intn(100000))

			payload := UpdateEventRequest{
				Error:   &newErr,
				Latency: &newLat,
				Output:  &newOut,
			}

			body, _ := json.Marshal(payload)
			req, _ := http.NewRequest(http.MethodPut, cfg.APIBaseURL+"/event/"+eventID, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")

			resp, err := client.Do(req)
			if err != nil {
				atomic.AddInt64(&errors, 1)
				return
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)

			if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusOK {
				atomic.AddInt64(&success, 1)
			} else {
				atomic.AddInt64(&errors, 1)
			}
		}(id)
	}

	wg.Wait()
	close(done)
	elapsed := time.Since(startTime)

	return PhaseResult{
		Name:      "UPDATE",
		Total:     len(ids),
		Success:   atomic.LoadInt64(&success),
		Errors:    atomic.LoadInt64(&errors),
		Duration:  elapsed,
		StartedAt: startTime,
		EndedAt:   startTime.Add(elapsed),
	}
}

// ---------------------------------------------------------------------------
// Progress ticker — prints a line every 5 seconds during a phase
// ---------------------------------------------------------------------------

func startProgressTicker(label string, success, errors *int64, total int64, start time.Time) chan struct{} {
	done := make(chan struct{})
	var lastCount int64
	lastTime := time.Now()

	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				cur := atomic.LoadInt64(success)
				errs := atomic.LoadInt64(errors)
				elapsed := time.Since(start)
				dt := time.Since(lastTime)
				recentRate := float64(cur-lastCount) / dt.Seconds()
				overallRate := float64(cur) / elapsed.Seconds()
				pct := float64(cur+errs) / float64(total) * 100

				log.Printf("  [%s] %6.1f%%  %d/%d (err %d) | recent %8.0f/s | overall %8.0f/s | %v",
					label, pct, cur, total, errs, recentRate, overallRate, elapsed.Round(time.Second))

				lastCount = cur
				lastTime = time.Now()
			}
		}
	}()

	return done
}

// ---------------------------------------------------------------------------
// Pretty printers
// ---------------------------------------------------------------------------

func printBanner() {
	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════════════╗")
	fmt.Println("║        EVENT ANALYTICS PIPELINE — FULL BENCHMARK REPORT         ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════╝")
	fmt.Println()
}

func printConfig(cfg BenchmarkConfig) {
	log.Printf("Configuration:")
	log.Printf("  API Base URL :  %s", cfg.APIBaseURL)
	log.Printf("  Insert count :  %d", cfg.InsertCount)
	log.Printf("  Delete count :  %d", cfg.DeleteCount)
	log.Printf("  Update count :  %d", cfg.UpdateCount)
	log.Printf("  Concurrency  :  %d", cfg.Concurrency)
	log.Printf("  HTTP timeout :  %v", cfg.HTTPTimeout)
	fmt.Println()
}

func printPhaseHeader(name string, count int) {
	log.Println()
	log.Printf("┌──────────────────────────────────────────────────────────────┐")
	log.Printf("│  %s  (%d operations)%s│", name, count, strings.Repeat(" ", 42-len(name)-digitCount(count)))
	log.Printf("└──────────────────────────────────────────────────────────────┘")
}

func printPhaseResult(r PhaseResult) {
	log.Println()
	log.Printf("  ✅ %s completed", r.Name)
	log.Printf("     Total requested :  %d", r.Total)
	log.Printf("     Successful      :  %d", r.Success)
	log.Printf("     Errors          :  %d", r.Errors)
	log.Printf("     Wall-clock time :  %v", r.Duration.Round(time.Millisecond))
	log.Printf("     Throughput      :  %.2f ops/sec", r.Rate())
	log.Printf("     Avg latency     :  %.0f µs/op", r.AvgLatencyMicros())
}

func printFinalReport(cfg BenchmarkConfig, results []PhaseResult) {
	var totalDuration time.Duration
	var totalOps int64
	for _, r := range results {
		totalDuration += r.Duration
		totalOps += r.Success
	}

	fmt.Println()
	fmt.Println("══════════════════════════════════════════════════════════════════")
	fmt.Println("                      BENCHMARK SUMMARY")
	fmt.Println("══════════════════════════════════════════════════════════════════")
	fmt.Println()

	// Results table
	fmt.Printf("  %-10s  %10s  %10s  %14s  %12s\n", "Phase", "Success", "Errors", "Duration", "Ops/sec")
	fmt.Println("  ──────────  ──────────  ──────────  ──────────────  ────────────")

	for _, r := range results {
		fmt.Printf("  %-10s  %10d  %10d  %14v  %12.0f\n",
			r.Name,
			r.Success,
			r.Errors,
			r.Duration.Round(time.Millisecond),
			r.Rate(),
		)
	}

	fmt.Println("  ──────────  ──────────  ──────────  ──────────────  ────────────")
	overallRate := float64(totalOps) / totalDuration.Seconds()
	fmt.Printf("  %-10s  %10d  %10s  %14v  %12.0f\n",
		"TOTAL",
		totalOps,
		"—",
		totalDuration.Round(time.Millisecond),
		overallRate,
	)

	// Visual time breakdown
	fmt.Println()
	fmt.Println("  Time breakdown (wall-clock, excludes 5 s pauses between phases)")
	fmt.Println()

	for _, r := range results {
		barLen := int(r.Duration.Seconds() / totalDuration.Seconds() * 40)
		if barLen < 1 {
			barLen = 1
		}
		bar := strings.Repeat("█", barLen) + strings.Repeat("░", 40-barLen)
		fmt.Printf("  %-8s %s  %v\n", r.Name, bar, r.Duration.Round(time.Millisecond))
	}

	// Configuration
	fmt.Println()
	fmt.Println("  Configuration")
	fmt.Printf("    Concurrency : %d\n", cfg.Concurrency)
	fmt.Printf("    HTTP timeout: %v\n", cfg.HTTPTimeout)
	fmt.Printf("    API URL     : %s\n", cfg.APIBaseURL)

	fmt.Println()
	fmt.Println("══════════════════════════════════════════════════════════════════")
	fmt.Println()

	log.Println("Benchmark finished. Consumer may still be draining Kafka — check consumer logs for final state.")
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func waitForAPI(baseURL string, maxRetries int) {
	log.Printf("Waiting for API at %s …", baseURL)
	client := &http.Client{Timeout: 5 * time.Second}

	for i := 0; i < maxRetries; i++ {
		resp, err := client.Get(baseURL + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				log.Println("✅ API is ready")
				return
			}
		}
		if i%5 == 0 {
			log.Printf("  … still waiting (attempt %d/%d)", i+1, maxRetries)
		}
		time.Sleep(2 * time.Second)
	}
	log.Fatal("❌ API did not become ready in time — aborting")
}

func envStr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v, ok := os.LookupEnv(key); ok {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return fallback
}

func digitCount(n int) int {
	if n == 0 {
		return 1
	}
	c := 0
	for n > 0 {
		c++
		n /= 10
	}
	return c
}
