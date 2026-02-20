package clickhouse

import (
	"context"
	"fmt"
	"log"
)

//  creates the required tables in ClickHouse
func RunMigrations(client *Client) error {
	ctx := context.Background()

	migrations := []struct {
		name  string
		query string
	}{
		{
			name: "create_events_table",
			query: `
				CREATE TABLE IF NOT EXISTS events (
					id               String,
					conversation_id  String,
					conversation_metadata String DEFAULT '',
					event_name       Enum8('event_a' = 1, 'event_b' = 2, 'event_c' = 3),
					error            String DEFAULT '',
					latency          Float64 DEFAULT 0,
					input            String DEFAULT '',
					input_embeddings Array(Float32),
					output           String DEFAULT '',
					output_embeddings Array(Float32),
					is_deleted       UInt8 DEFAULT 0,
					version          UInt64,
					created_at       DateTime64(3) DEFAULT now64(3),
					updated_at       DateTime64(3) DEFAULT now64(3)
				) ENGINE = ReplacingMergeTree(version)
				PARTITION BY toYYYYMM(created_at)
				ORDER BY (conversation_id, event_name, id)
				SETTINGS index_granularity = 8192
			`,
		},
		{
			// Secondary index on id for fast single-event lookups (GET /event/:id, UPDATE, DELETE)
			// The primary ORDER BY starts with conversation_id, so looking up by id alone
			// would require a full scan without this index.
			name: "create_id_index",
			query: `
				ALTER TABLE events
				ADD INDEX IF NOT EXISTS idx_id (id) TYPE bloom_filter(0.01) GRANULARITY 4
			`,
		},
		{
			// Index on created_at for time-range queries like "errors in last 7 days"
			name: "create_created_at_index",
			query: `
				ALTER TABLE events
				ADD INDEX IF NOT EXISTS idx_created_at (created_at) TYPE minmax GRANULARITY 1
			`,
		},
		{
			// Index on error column for quick filtering of rows that have errors
			name: "create_error_index",
			query: `
				ALTER TABLE events
				ADD INDEX IF NOT EXISTS idx_error (error) TYPE bloom_filter(0.01) GRANULARITY 4
			`,
		},
		{
			// Materialized view: error counts per event type per day
			// Pre-aggregates data so "errors in event_a over the last 7 days" is instant
			name: "create_error_counts_mv_target",
			query: `
				CREATE TABLE IF NOT EXISTS mv_error_counts_daily (
					event_date   Date,
					event_name   Enum8('event_a' = 1, 'event_b' = 2, 'event_c' = 3),
					error_count  AggregateFunction(sum, UInt64),
					total_count  AggregateFunction(sum, UInt64)
				) ENGINE = AggregatingMergeTree()
				PARTITION BY toYYYYMM(event_date)
				ORDER BY (event_name, event_date)
			`,
		},
		{
			name: "create_error_counts_mv",
			query: `
				CREATE MATERIALIZED VIEW IF NOT EXISTS mv_error_counts_daily_view
				TO mv_error_counts_daily
				AS
				SELECT
					toDate(created_at) AS event_date,
					event_name,
					sumState(toUInt64(error != '' AND is_deleted = 0)) AS error_count,
					sumState(toUInt64(is_deleted = 0)) AS total_count
				FROM events
				GROUP BY event_date, event_name
			`,
		},
		{
			// Materialized view: latency stats per event type per conversation
			// Supports "p95 latency per event type per session"
			name: "create_latency_stats_mv_target",
			query: `
				CREATE TABLE IF NOT EXISTS mv_latency_per_session (
					conversation_id  String,
					event_name       Enum8('event_a' = 1, 'event_b' = 2, 'event_c' = 3),
					latency_quantile AggregateFunction(quantile(0.95), Float64),
					event_count      AggregateFunction(sum, UInt64)
				) ENGINE = AggregatingMergeTree()
				ORDER BY (conversation_id, event_name)
			`,
		},
		{
			name: "create_latency_stats_mv",
			query: `
				CREATE MATERIALIZED VIEW IF NOT EXISTS mv_latency_per_session_view
				TO mv_latency_per_session
				AS
				SELECT
					conversation_id,
					event_name,
					quantileState(0.95)(latency) AS latency_quantile,
					sumState(toUInt64(1)) AS event_count
				FROM events
				WHERE is_deleted = 0
				GROUP BY conversation_id, event_name
			`,
		},
	}

	for _, m := range migrations {
		log.Printf("[migration] running: %s", m.name)
		if err := client.Conn.Exec(ctx, m.query); err != nil {
			return fmt.Errorf("migration '%s' failed: %w", m.name, err)
		}
		log.Printf("[migration] completed: %s", m.name)
	}

	log.Println("[migration] all migrations completed successfully")
	return nil
}
