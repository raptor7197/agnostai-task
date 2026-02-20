# 🏗️ Architecture — Event Analytics Pipeline

## Table of Contents
- [System Overview](#system-overview)
- [Architecture Diagram](#architecture-diagram)
- [Data Flow](#data-flow)
- [Project Structure](#project-structure)
- [Data Model](#data-model)
- [ClickHouse Schema Design](#clickhouse-schema-design)
- [Kafka Topic Design](#kafka-topic-design)
- [HTTP API Reference](#http-api-reference)
- [Consumer Design](#consumer-design)
- [Python SDK](#python-sdk)
- [Docker Compose Setup](#docker-compose-setup)
- [Benchmark Methodology](#benchmark-methodology)
- [Design Decisions & Trade-offs](#design-decisions--trade-offs)
- [What Breaks at 1 Billion Rows](#what-breaks-at-1-billion-rows)
- [What I'd Improve With More Time](#what-id-improve-with-more-time)
- [Things I Considered and Decided Against](#things-i-considered-and-decided-against)

---

## System Overview

This is an **event analytics pipeline** — the backend of a product analytics tool similar to Mixpanel. Events flow from an HTTP API into Kafka, are consumed by a Go service, and written into ClickHouse for analytical queries.

The system is designed with a clear separation of concerns:
- **API server** — Accepts HTTP requests, validates input, and publishes messages to Kafka. Serves read queries directly from ClickHouse.
- **Kafka** — Acts as a durable, ordered message buffer between the API and ClickHouse. Decouples ingestion throughput from storage write speed.
- **Consumer** — Reads from Kafka and writes to ClickHouse. Supports batch inserts for high throughput on creates, and sequential processing for updates/deletes that require read-modify-write.
- **ClickHouse** — Columnar OLAP database optimized for analytical queries on hundreds of millions of rows.

---

## Architecture Diagram

```text
┌──────────────┐       ┌───────────────┐       ┌──────────────────┐        ┌─────────────────┐
│              │       │               │       │                  │        │                 │
│  HTTP Client │──────▶│   Go API      │──────▶│     Kafka        │──────▶│   Go Consumer   │
│  / SDK       │       │   Server      │       │   (events topic)  │       │   (batch mode)  │
│              │       │   (:8080)     │       │   3 partitions    │       │                 │
└──────────────┘       └───────┬───────┘       └───────────────────┘       └────────┬────────┘
                               │                                                    │
                               │  Reads (direct)                        Writes      │
                               │                                                    │
                               ▼                                                    ▼
                       ┌───────────────────────────────────────────────────────────────────┐
                       │                                                                   │
                       │                      ClickHouse                                   │
                       │                                                                   │
                       │                                                                   │
                       │   events table                                                    │
                       │   ├── Partitioned by month (toYYYYMM)                             │
                       │   ├── Ordered by (conversation_id, event_name, id)                │
                       │   ├── Bloom filter index on id                                    │
                       │   ├── MinMax index on created_at                                  │
                       │   └── Bloom filter index on error                                 │
                       │                                                                   │
                       │   Materialized Views                                              │
                       │   ├── mv_error_counts_daily (pre-aggregated error counts)         │
                       │   └── mv_latency_per_session (pre-aggregated p95 latency)         │
                       │                                                                   │
                       └───────────────────────────────────────────────────────────────────┘
```

### Write Path (API → Kafka → Consumer → ClickHouse)
```text
POST /event ──▶ Validate ──▶ Generate UUID ──▶ Publish to Kafka ──▶ Return 202 Accepted
                                                        │
                                                        ▼
                                              Consumer reads msg
                                                        │
                                                        ▼
                                              Insert into ClickHouse
```

### Read Path (API → ClickHouse)
```text
GET /event/:id ──▶ Query ClickHouse (FINAL) ──▶ Return event JSON
```

---

## Data Flow

### 1. Create Event
1. Client sends `POST /event` with event data
2. API validates required fields (`conversation_id`, `event_name`)
3. API generates a new UUID for the event
4. API publishes a `{"operation": "create", ...}` message to Kafka topic `events`
5. API returns `202 Accepted` with the generated event ID immediately
6. Consumer picks up the message from Kafka
7. Consumer inserts the event into ClickHouse with `version = 1` and `is_deleted = 0`
8. Consumer commits the Kafka offset

### 2. Update Event
1. Client sends `PUT /event/:id` with fields to update
2. API publishes a `{"operation": "update", ...}` message to Kafka
3. API returns `202 Accepted`
4. Consumer picks up the message
5. Consumer reads the **current version** of the event from ClickHouse (using `FINAL`)
6. Consumer merges the update fields into the existing event
7. Consumer inserts a **new row** with `version + 1` (ReplacingMergeTree handles dedup)
8. Consumer commits the Kafka offset

### 3. Delete Event
1. Client sends `DELETE /event/:id`
2. API publishes a `{"operation": "delete", ...}` message to Kafka
3. API returns `202 Accepted`
4. Consumer picks up the message
5. Consumer reads the current version of the event
6. Consumer inserts a **new row** with `version + 1` and `is_deleted = 1`
7. All read queries filter out `is_deleted = 1` rows
8. Eventually, ReplacingMergeTree merges will keep only the latest version

### 4. Read Event / Analytics
- All reads go **directly to ClickHouse**, bypassing Kafka entirely
- Queries use `FINAL` modifier to see the latest merged version
- Analytics endpoints use standard ClickHouse aggregation functions

---

## Project Structure

```
agnostai-task/
├── cmd/
│   ├── api/                    # HTTP API server entrypoint
│   │   └── main.go             # Server startup, graceful shutdown
│   └── consumer/               # Kafka consumer entrypoint
│       └── main.go             # Consumer startup, graceful shutdown
├── internal/
│   ├── api/                    # HTTP layer
│   │   ├── handler.go          # Request handlers for all endpoints
│   │   └── router.go           # Chi router setup with middleware
│   ├── clickhouse/             # Storage layer
│   │   ├── client.go           # Connection management, retry logic
│   │   ├── migrations.go       # DDL: tables, indexes, materialized views
│   │   └── repository.go       # CRUD operations + analytics queries
│   ├── kafka/                  # Messaging layer
│   │   ├── producer.go         # Message publishing with key-based partitioning
│   │   └── consumer.go         # Batch consumption with flush logic
│   └── models/                 # Shared data structures
│       └── event.go            # Event, KafkaMessage, API request/response types
├── benchmark/
│   └── main.go                 # Load testing: 500k inserts, 200k updates, 200k deletes
├── sdk/
│   └── python/                 # Python SDK (zero dependencies)
│       ├── event_client.py     # Full-featured client library
│       ├── setup.py            # pip installable
│       └── README.md           # SDK documentation
├── docker-compose.yml          # Full stack: Zookeeper, Kafka, ClickHouse, API, Consumer
├── Dockerfile.api              # Multi-stage build for API server
├── Dockerfile.consumer         # Multi-stage build for Consumer
├── go.mod / go.sum             # Go dependencies
├── ARCHITECTURE.md             # This file
├── Project.md                  # Original task specification
└── README.md                   # Quick start guide
```

### Why `internal/`?
Go's `internal` package convention prevents external packages from importing implementation details. The public interface is only the two binaries (`cmd/api` and `cmd/consumer`). This enforces clean boundaries.

---

## Data Model

### Core Event Structure

| Field                  | Type            | Description                                    |
|------------------------|-----------------|------------------------------------------------|
| `id`                   | String (UUID)   | Unique event identifier                        |
| `conversation_id`      | String          | Groups events into a conversation/session      |
| `conversation_metadata`| String (JSON)   | Arbitrary metadata about the conversation      |
| `event_name`           | Enum8           | Event type: `event_a`, `event_b`, `event_c`   |
| `error`                | String          | Error message (empty string = no error)        |
| `latency`              | Float64         | Latency in milliseconds                        |
| `input`                | String          | Input text/data                                |
| `input_embeddings`     | Array(Float32)  | Vector embeddings for the input                |
| `output`               | String          | Output text/data                               |
| `output_embeddings`    | Array(Float32)  | Vector embeddings for the output               |
| `is_deleted`           | UInt8           | Soft-delete flag (0 = active, 1 = deleted)     |
| `version`              | UInt64          | Monotonically increasing version for RMT dedup |
| `created_at`           | DateTime64(3)   | Event creation timestamp (ms precision)        |
| `updated_at`           | DateTime64(3)   | Last update timestamp (ms precision)           |

### Kafka Message Envelope

```json
{
    "operation": "create | update | delete",
    "event_id": "uuid-string",
    "timestamp": "2024-01-15T10:30:00Z",
    "data": {
        "conversation_id": "conv_123",
        "event_name": "event_a",
        "error": "",
        "latency": 142.5,
        "input": "user query",
        "output": "bot response",
        "input_embeddings": [0.1, 0.2, ...],
        "output_embeddings": [0.3, 0.4, ...]
    }
}
```

The `data` field is present for `create` and `update` operations, absent for `delete`.

---

## ClickHouse Schema Design

### Why ReplacingMergeTree?

ClickHouse is not Postgres. It is an **append-only columnar store** optimized for analytical queries, not transactional updates. There are several ways to handle updates/deletes:

| Approach                    | Pros                        | Cons                                      |
|-----------------------------|-----------------------------|-------------------------------------------|
| `ALTER TABLE ... UPDATE`    | SQL-like, familiar          | Mutations are heavy, async, rewrite parts  |
| `ALTER TABLE ... DELETE`    | SQL-like                    | Same as above — rewrites data parts        |
| **ReplacingMergeTree**      | Native, efficient           | Requires FINAL or manual dedup in queries  |
| CollapsingMergeTree         | Good for cancel-and-replace | Complex, requires tracking sign column     |

I chose **ReplacingMergeTree** because:
1. **Updates** = insert a new row with higher `version`; the engine automatically deduplicates during background merges
2. **Deletes** = insert a new row with `is_deleted = 1` and higher `version`; filtered out in queries
3. No expensive ALTER TABLE mutations that rewrite data parts
4. Reads use `FINAL` modifier to get the latest version without waiting for merges

### Table DDL

```sql
CREATE TABLE events (
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
```

### Why This ORDER BY?

The `ORDER BY` clause in ClickHouse's MergeTree family serves **three purposes**:
1. **Physical sort order** — determines how data is stored on disk
2. **Primary key** — the first N columns become the sparse primary index
3. **Dedup key** (for ReplacingMergeTree) — rows with the same ORDER BY values are candidates for replacement

I chose `(conversation_id, event_name, id)` because:

- **`conversation_id` first** — The most common query pattern is "all events in session X". Having `conversation_id` as the leftmost column means ClickHouse can skip entire granules using the sparse index.
- **`event_name` second** — Queries like "errors in event_a" benefit from having event_name in the sort key. Combined with conversation_id, this also helps "p95 latency per event type per session".
- **`id` last** — Ensures each event is uniquely identifiable for ReplacingMergeTree deduplication. Two rows with the same `(conversation_id, event_name, id)` are the same event at different versions.

### Why This PARTITION BY?

`PARTITION BY toYYYYMM(created_at)` — Monthly partitions.

- Time-range queries (e.g., "last 7 days") can skip entire monthly partitions
- Old data can be dropped efficiently with `ALTER TABLE DROP PARTITION`
- Partitions stay manageable in size (not too many, not too few)
- At 100M+ rows, this gives ~8-12 partitions per year, which is healthy

### Secondary Indexes

```sql
-- Fast single-event lookups by ID (primary key starts with conversation_id)
ALTER TABLE events ADD INDEX idx_id (id) TYPE bloom_filter(0.01) GRANULARITY 4;

-- Time-range filtering optimization
ALTER TABLE events ADD INDEX idx_created_at (created_at) TYPE minmax GRANULARITY 1;

-- Quick error filtering (for "count errors" queries)
ALTER TABLE events ADD INDEX idx_error (error) TYPE bloom_filter(0.01) GRANULARITY 4;
```

**Why bloom_filter on `id`?** The primary key starts with `conversation_id`, so looking up a single event by `id` alone would require scanning. The bloom filter index lets ClickHouse skip granules that definitely don't contain the target ID.

**Why minmax on `created_at`?** Even though we partition by month, within a partition ClickHouse still needs to scan all granules. A minmax index on `created_at` lets it skip granules outside the time range.

### Materialized Views

Two pre-aggregated materialized views for the most common analytical queries:

**1. Error Counts per Day per Event Type**
```sql
-- Target table (AggregatingMergeTree for incremental aggregation)
CREATE TABLE mv_error_counts_daily (
    event_date   Date,
    event_name   Enum8('event_a' = 1, 'event_b' = 2, 'event_c' = 3),
    error_count  AggregateFunction(sum, UInt64),
    total_count  AggregateFunction(sum, UInt64)
) ENGINE = AggregatingMergeTree()
PARTITION BY toYYYYMM(event_date)
ORDER BY (event_name, event_date);
```

Answers: *"How many errors in event_a over the last 7 days?"* — Instant, pre-computed.

**2. Latency per Session per Event Type**
```sql
CREATE TABLE mv_latency_per_session (
    conversation_id  String,
    event_name       Enum8('event_a' = 1, 'event_b' = 2, 'event_c' = 3),
    latency_quantile AggregateFunction(quantile(0.95), Float64),
    event_count      AggregateFunction(sum, UInt64)
) ENGINE = AggregatingMergeTree()
ORDER BY (conversation_id, event_name);
```

Answers: *"p95 latency per event type per session"* — Pre-aggregated quantile sketches.

> **Note:** Materialized views in ClickHouse are populated **at insert time**, so they include data from creates but may have stale data for updates/deletes (since those insert new rows, the MV sees both old and new). For perfectly accurate analytics on mutable data, the queries in `repository.go` use the base `events` table with `FINAL`. The MVs serve as fast approximations for dashboards.

---

## Kafka Topic Design

### Single Topic: `events`

| Property           | Value                |
|--------------------|----------------------|
| Topic name         | `events`             |
| Partitions         | 3                    |
| Replication factor | 1 (dev; 3 in prod)  |
| Retention          | 7 days               |
| Partition key      | Event ID (UUID)      |

### Why a Single Topic?

I chose a single topic with a message type field (`operation: create|update|delete`) instead of separate topics per operation type. Here's why:

**1. Ordering guarantees per event**

The critical invariant is: **for a given event, operations must be processed in order**. If a `create` and `update` for the same event go to different topics, the `update` might be processed before the `create`, causing a "not found" error.

With a single topic and event ID as the partition key, all operations for the same event land on the **same partition**. Kafka guarantees ordering within a partition, so a `create` is always processed before its subsequent `update` or `delete`.

**2. Simpler consumer logic**

One consumer group, one topic. The consumer reads messages and routes by `operation` type. No need to coordinate across multiple consumers reading different topics.

**3. When would I use multiple topics?**

If create, update, and delete operations had wildly different processing characteristics (e.g., creates need batch processing but updates need exactly-once semantics), separate topics would make sense. In this system, the processing is similar enough to keep it unified.

### Why 3 Partitions?

- 3 partitions allow up to 3 concurrent consumer instances in the consumer group
- Matches the 3 event types (though the hash partitioner distributes by event ID, not event type)
- Enough parallelism for the expected throughput without over-fragmenting
- In production, this would scale to 12-24 partitions based on throughput requirements

### Partition Key: Event ID

Using the event UUID as the Kafka message key means:
- The hash partitioner distributes events across partitions by UUID
- All operations for the same event go to the same partition (ordering!)
- Good distribution across partitions (UUIDs are uniformly distributed)

---

## HTTP API Reference

### Base URL
```
http://localhost:8080
```

### Write Endpoints (via Kafka)

These endpoints publish messages to Kafka and return `202 Accepted` immediately. The actual write to ClickHouse happens asynchronously.

#### `POST /event/` — Create Event

**Request:**
```json
{
    "conversation_id": "conv_123",
    "event_name": "event_a",
    "conversation_metadata": "{\"user_id\": \"u_456\"}",
    "error": "",
    "latency": 142.5,
    "input": "What is the weather?",
    "input_embeddings": [0.1, 0.2, 0.3],
    "output": "It's sunny!",
    "output_embeddings": [0.4, 0.5, 0.6]
}
```

**Response (202):**
```json
{
    "success": true,
    "message": "event creation queued",
    "data": {
        "id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
    }
}
```

#### `PUT /event/:id` — Update Event

Only provided fields are updated. Uses pointer types in Go to distinguish between "not provided" and "set to empty/zero".

**Request:**
```json
{
    "error": "timeout",
    "latency": 5000.0,
    "output": "Request timed out"
}
```

**Response (202):**
```json
{
    "success": true,
    "message": "event update queued",
    "data": {
        "id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
    }
}
```

#### `DELETE /event/:id` — Delete Event

**Response (202):**
```json
{
    "success": true,
    "message": "event deletion queued",
    "data": {
        "id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
    }
}
```

### Read Endpoints (direct from ClickHouse)

#### `GET /event/:id` — Get Single Event

**Response (200):**
```json
{
    "success": true,
    "data": {
        "id": "a1b2c3d4-...",
        "conversation_id": "conv_123",
        "event_name": "event_a",
        "latency": 142.5,
        "created_at": "2024-01-15T10:30:00.000Z",
        ...
    }
}
```

#### `GET /events/conversation/:conversation_id` — All Events in a Session

Returns events ordered by `created_at ASC`.

#### `GET /analytics/errors?event_name=event_a&days=7` — Error Counts

*Answers: "How many errors in event_a over the last 7 days?"*

**Response:**
```json
{
    "success": true,
    "data": [
        {"date": "2024-01-09", "event_name": "event_a", "error_count": 42, "total_count": 1500},
        {"date": "2024-01-10", "event_name": "event_a", "error_count": 38, "total_count": 1620},
        ...
    ]
}
```

#### `GET /analytics/latency?limit=100` — P95 Latency per Event per Session

*Answers: "p95 latency per event type per session"*

#### `GET /analytics/sessions/top-errors?limit=100` — Top Sessions by Error Rate

*Answers: "Top sessions by error rate"*

### Utility Endpoints

| Endpoint    | Description        |
|-------------|--------------------|
| `GET /health` | Health check     |
| `GET /ping`   | Heartbeat (204)  |

---

## Consumer Design

### Batch Mode (Default)

The consumer runs in **batch mode** for optimal throughput:

1. **Creates** are batched together (default batch size: 500) and flushed either when the batch is full or every 200ms (whichever comes first). ClickHouse's native batch insert is orders of magnitude faster than individual inserts.

2. **Updates and deletes** are processed individually because they require a **read-modify-write** cycle: read the current version from ClickHouse, merge changes, write a new version. These flush any pending create batch first to maintain ordering.

3. The consumer commits offsets **after** successful writes, providing at-least-once delivery semantics.

### Why At-Least-Once?

- **Exactly-once** would require distributed transactions between Kafka and ClickHouse, adding massive complexity
- **At-least-once** means a message might be processed twice if the consumer crashes after writing but before committing. With ReplacingMergeTree, duplicate inserts with the same version are harmless — they'll be deduplicated during merges
- This is the standard trade-off in event streaming systems

### Error Handling

- If a batch insert fails, the consumer falls back to individual inserts for that batch
- If an individual insert fails, the error is logged and the offset is committed to avoid blocking the consumer
- In production, failed messages would go to a **dead letter queue** for manual inspection

### Configuration

| Env Variable              | Default | Description                              |
|---------------------------|---------|------------------------------------------|
| `CONSUMER_BATCH_SIZE`     | 500     | Max events per batch insert              |
| `CONSUMER_FLUSH_INTERVAL_MS` | 200  | Max time before flushing a partial batch |

---

## Python SDK

The Python SDK is a **zero-dependency** client library that wraps the REST API. It uses only Python's standard library (`urllib`, `json`).

### Key Features
- Type-validated inputs (event_name must be valid)
- Automatic retries with exponential backoff
- Custom exception hierarchy (`EventSDKError`, `EventNotFoundError`, `EventValidationError`)
- Both write operations (async, via Kafka) and read operations (sync, from ClickHouse)
- `wait_until_ready()` helper for startup synchronization

### Quick Usage

```python
from event_client import EventClient

client = EventClient("http://localhost:8080")

# Track an event
event_id = client.track_event(
    conversation_id="conv_123",
    event_name="event_a",
    input="hello",
    output="world",
    latency=150.5,
)

# Update it
client.update_event(event_id, error="timeout", latency=5000.0)

# Query analytics
errors = client.get_error_counts("event_a", days=7)
sessions = client.get_top_error_sessions(limit=10)
```

---

## Docker Compose Setup

### Services

| Service      | Image                              | Ports       | Purpose                    |
|--------------|------------------------------------|-------------|----------------------------|
| `zookeeper`  | confluentinc/cp-zookeeper:7.6.0   | 2181        | Kafka coordination         |
| `kafka`      | confluentinc/cp-kafka:7.6.0       | 9092, 29092 | Message broker             |
| `clickhouse` | clickhouse/clickhouse-server:24.3  | 8123, 9000  | Analytics database         |
| `api`        | Multi-stage Go build               | 8080        | HTTP API server            |
| `consumer`   | Multi-stage Go build               | —           | Kafka → ClickHouse writer  |

### Networking

All services are on a shared `event-network` bridge network. Internal Kafka traffic uses `kafka:29092`, external (host) access uses `localhost:9092`.

### Health Checks

Each infrastructure service has a health check. The `api` and `consumer` services have `depends_on` conditions that wait for `kafka` and `clickhouse` to be healthy before starting.

### Quick Start

```bash
docker compose up --build
```

This single command starts the entire stack. The API will be available at `http://localhost:8080` once everything is healthy.

---

## Benchmark Methodology

The benchmark tool (`benchmark/main.go`) tests the full pipeline end-to-end through the HTTP API:

### Test Phases

| Phase  | Operation | Count   | Concurrency | Description                              |
|--------|-----------|---------|-------------|------------------------------------------|
| 1      | INSERT    | 500,000 | 100         | Create events with realistic random data |
| 2      | UPDATE    | 200,000 | 100         | Update error, latency, and output fields |
| 3      | DELETE    | 200,000 | 100         | Soft-delete events                       |

### Data Characteristics

- ~50,000 unique conversations (10 events per conversation on average)
- Events distributed across `event_a`, `event_b`, `event_c`
- ~30% of events have error messages
- Latency values range from 0 to 5000ms
- Updates and deletes target non-overlapping event sets where possible

### Running the Benchmark

```bash
# With the stack running via Docker Compose
go run benchmark/main.go

# Or with custom settings
API_BASE_URL=http://localhost:8080 \
INSERT_COUNT=500000 \
UPDATE_COUNT=200000 \
DELETE_COUNT=200000 \
CONCURRENCY=100 \
go run benchmark/main.go
```

### What the Benchmark Measures

The benchmark measures **end-to-end API throughput** — the time from sending an HTTP request to receiving a `202 Accepted` response. This includes:
- HTTP request/response overhead
- JSON serialization/deserialization
- Kafka message production (synchronous ack from leader)

It does **not** measure consumer-to-ClickHouse write latency (which is async). After each phase, there's a 5-second pause to let the consumer catch up.

---

## Design Decisions & Trade-offs

### 1. Asynchronous Writes via Kafka

**Decision:** All writes go through Kafka. The API returns `202 Accepted` before the data reaches ClickHouse.

**Pro:** Decouples ingestion speed from storage speed. The API can accept events at high throughput even if ClickHouse is temporarily slow.

**Con:** Read-after-write is not immediately consistent. A client that creates an event and immediately reads it might get a 404.

**Mitigation:** For use cases that need immediate consistency, the API could optionally bypass Kafka and write directly to ClickHouse (not implemented, but straightforward to add).

### 2. ReplacingMergeTree for Updates/Deletes

**Decision:** Use ReplacingMergeTree with version-based deduplication instead of ALTER TABLE mutations.

**Pro:** Inserts are fast and append-only. No expensive data part rewrites.

**Con:** Reads must use `FINAL` to get the latest version, which has some overhead. Background merges are not instant, so without `FINAL` you might see duplicate/old rows.

**Alternative considered:** CollapsingMergeTree — but it requires tracking a `sign` column and is more complex for the update-in-place pattern.

### 3. Soft Deletes

**Decision:** Deleted events are marked with `is_deleted = 1` instead of being physically removed.

**Pro:** Delete is an append operation (fast). Consistent with ReplacingMergeTree's append-only nature.

**Con:** Deleted data still occupies storage until partition TTLs clean it up or the partition is dropped. All read queries must filter `is_deleted = 0`.

### 4. Single Kafka Topic

**Decision:** One topic (`events`) with operation type in the message body.

**Pro:** Guarantees ordering per event ID. Simpler consumer.

**Con:** Can't independently scale consumer throughput for different operation types.

### 5. Chi for HTTP Routing (vs. Gin, Fiber, net/http)

**Decision:** Used `go-chi/chi` for HTTP routing.

**Why:** Lightweight, idiomatic (builds on `net/http`), good middleware ecosystem. Gin and Fiber add unnecessary abstraction layers for a service this size. Raw `net/http` would work but chi's route patterns and middleware make the code cleaner.

### 6. Batch Consumer with Flush Timer

**Decision:** Batch creates together, but process updates/deletes individually.

**Why:** Creates are pure inserts (no read dependency) so they benefit enormously from batching — ClickHouse's batch insert is 100-1000x faster per row than individual inserts. Updates and deletes need to read current state first, making batching them more complex (and potentially stale-read-prone).

---

## What Breaks at 1 Billion Rows

### 1. `FINAL` Becomes Expensive
At 1B rows with many versions per event, `FINAL` forces ClickHouse to merge-sort rows on the fly. **Fix:** Use `OPTIMIZE TABLE ... FINAL` on a schedule to force merges, reducing the work `FINAL` does at query time. Or use `argMax` patterns instead of `FINAL`:
```sql
SELECT argMax(latency, version) FROM events WHERE id = ? GROUP BY id
```

### 2. Single Consumer Bottleneck
One consumer group with one instance may not keep up. **Fix:** Scale to N consumer instances (up to partition count). Increase partition count to 12-24.

### 3. Update Read-Modify-Write Latency
Each update reads from ClickHouse, which gets slower at scale. **Fix:** Cache recent events in Redis. The consumer reads from cache first, falls back to ClickHouse.

### 4. Embeddings Storage Bloat
If each event has 1536-dim float32 embeddings (~6KB per embedding, two per event), 1B rows = ~12TB just for embeddings. **Fix:** Store embeddings in a separate table or external vector store. Keep only a reference/hash in the events table.

### 5. Monthly Partitions Get Large
1B rows over 2-3 months = 300-500M rows per partition. **Fix:** Switch to daily partitioning (`PARTITION BY toYYYYMMDD(created_at)`) or add a secondary partitioning dimension.

### 6. Kafka Lag
500k events/second would overwhelm a 3-partition topic. **Fix:** Increase partitions to 24+, use multiple consumer groups for different workloads (real-time vs. batch), enable Kafka compression (snappy/lz4).

---

## What I'd Improve With More Time

1. **Redis caching layer** — Cache recently written events so updates/deletes don't need to hit ClickHouse. Also serves as a read-through cache for the API's GET endpoints.

2. **Dead Letter Queue** — Failed Kafka messages should go to a DLQ topic instead of being logged and skipped. Add a separate consumer that processes DLQ messages with manual intervention.

3. **Exactly-once semantics** — Implement idempotent writes using a deduplication table keyed on `(event_id, version)`. Check before inserting to avoid processing the same message twice.

4. **API rate limiting** — Add per-client rate limiting middleware (token bucket) to protect the pipeline from burst traffic.

5. **Observability** — Add Prometheus metrics (event counts, latency histograms, consumer lag), structured JSON logging, and OpenTelemetry traces for end-to-end request tracking.

6. **Schema evolution** — Add a schema registry (Avro/Protobuf) for Kafka messages. JSON is flexible but has no schema enforcement.

7. **TTL cleanup** — Add ClickHouse TTL to automatically drop soft-deleted rows after a retention period:
   ```sql
   ALTER TABLE events MODIFY TTL created_at + INTERVAL 90 DAY DELETE WHERE is_deleted = 1
   ```

8. **Horizontal API scaling** — The API server is stateless; just add a load balancer and run N instances. Would need to benchmark the Kafka producer connection pool under shared load.

9. **Integration tests** — Testcontainers-based integration tests that spin up Kafka and ClickHouse in Docker, test the full pipeline, and verify end-to-end correctness.

10. **Backpressure handling** — If the Kafka topic fills up or ClickHouse is down, the API should return 503 with a Retry-After header instead of blocking indefinitely.

---

## Things I Considered and Decided Against

### 1. Multiple Kafka Topics (events-create, events-update, events-delete)
**Why not:** Breaks ordering guarantees. A `create` on `events-create` and an `update` on `events-update` for the same event might be processed in the wrong order.

### 2. ALTER TABLE mutations for updates/deletes
**Why not:** ClickHouse mutations rewrite entire data parts. At scale, this causes heavy disk I/O and blocks reads. ReplacingMergeTree's append-only approach is much lighter.

### 3. Using ClickHouse's `Buffer` table engine
**Why not:** Buffer tables act as an in-memory write buffer before flushing to the main table. While this could improve write throughput, it adds another layer of eventual consistency and data loss risk (unflushed buffer data is lost on crash). Kafka already serves as our durable buffer.

### 4. gRPC instead of REST
**Why not:** REST is simpler for this project scope and makes the SDK easier (just HTTP calls). gRPC would add complexity (protobuf schema management, code generation) without meaningful benefit at this scale. For a production system with internal service-to-service calls, gRPC would be worth it.

### 5. Storing embeddings as FixedString
**Why not:** ClickHouse supports `Array(Float32)` natively and can perform vector operations on it. FixedString would require manual serialization/deserialization and lose the ability to use ClickHouse's array functions.

### 6. Using Kafka Connect for ClickHouse sink
**Why not:** Kafka Connect's ClickHouse sink connector handles simple inserts well, but our update/delete pattern requires custom logic (read current version, merge, insert new version). A custom Go consumer gives full control over this logic.

### 7. Using `MergeTree` + `OPTIMIZE ... DEDUPLICATE`
**Why not:** Plain MergeTree doesn't deduplicate. We'd need to manually run `OPTIMIZE` commands periodically and handle duplicates in every query. ReplacingMergeTree does this automatically during background merges.