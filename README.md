# 🚀 Event Analytics Pipeline

A high-throughput event ingestion and analytics system built with **Go**, **Kafka**, and **ClickHouse**. Events flow from an HTTP API through Kafka into ClickHouse, designed to handle hundreds of millions of rows with fast analytical queries.

Think of it as the backend of a product analytics tool like Mixpanel.

## Architecture

```text
┌──────────────┐       ┌──────────────┐       ┌───────────────┐       ┌──────────────┐
│  HTTP Client │──────▶│   Go API     │──────▶│    Kafka      │──────▶│ Go Consumer  │
│  / SDK       │       │  (writes)    │       │ (events topic)│       │ (batch mode) │
└──────────────┘       └──────┬───────┘       └───────────────┘       └──────┬───────┘
                              │ reads (direct)                               │ writes
                              ▼                                              ▼
                       ┌──────────────────────────────────────────────────────────┐
                       │                     ClickHouse                           │
                       │              (ReplacingMergeTree)                        │
                       └──────────────────────────────────────────────────────────┘
```

- **Writes** (create/update/delete) → API → Kafka → Consumer → ClickHouse
- **Reads** (get event, analytics) → API → ClickHouse directly

## Quick Start

### Prerequisites

- Docker & Docker Compose
- Go 1.22+ (for running benchmark locally)

### Run Everything

```bash
docker compose up --build
```

This starts:
| Service       | Port  | Description              |
|---------------|-------|--------------------------|
| API Server    | 8080  | HTTP REST API            |
| Kafka         | 9092  | Message broker           |
| Zookeeper     | 2181  | Kafka coordination       |
| ClickHouse    | 9000  | Analytics database       |
| Consumer      | —     | Kafka → ClickHouse       |

The API is available at `http://localhost:8080` once all services are healthy.

### Verify It's Working

```bash
# Health check
curl http://localhost:8080/health

# Create an event
curl -X POST http://localhost:8080/event/ \
  -H "Content-Type: application/json" \
  -d '{
    "conversation_id": "conv_001",
    "event_name": "event_a",
    "input": "Hello, world!",
    "output": "Hi there!",
    "latency": 150.5,
    "error": ""
  }'

# Wait a moment for consumer processing, then read it back
curl http://localhost:8080/event/<EVENT_ID>

# Get all events in a conversation
curl http://localhost:8080/events/conversation/conv_001

# Analytics: errors in event_a over the last 7 days
curl "http://localhost:8080/analytics/errors?event_name=event_a&days=7"

# Analytics: p95 latency per event type per session
curl "http://localhost:8080/analytics/latency?limit=50"

# Analytics: top sessions by error rate
curl "http://localhost:8080/analytics/sessions/top-errors?limit=10"
```

## API Reference

### Write Endpoints (async via Kafka)

| Method   | Endpoint         | Description                          |
|----------|------------------|--------------------------------------|
| `POST`   | `/event/`        | Create a new event                   |
| `PUT`    | `/event/:id`     | Update event fields                  |
| `DELETE` | `/event/:id`     | Soft-delete an event                 |

All write endpoints return `202 Accepted` immediately. The write reaches ClickHouse asynchronously through Kafka.

### Read Endpoints (direct from ClickHouse)

| Method | Endpoint                                  | Description                          |
|--------|-------------------------------------------|--------------------------------------|
| `GET`  | `/event/:id`                              | Get a single event                   |
| `GET`  | `/events/conversation/:conversation_id`   | All events in a session (by time)    |
| `GET`  | `/analytics/errors?event_name=X&days=N`   | Error counts per day                 |
| `GET`  | `/analytics/latency?limit=N`              | P95 latency per event type / session |
| `GET`  | `/analytics/sessions/top-errors?limit=N`  | Sessions ranked by error rate        |
| `GET`  | `/health`                                 | Health check                         |

## Running the Benchmark

The benchmark tests the full pipeline end-to-end: **500k inserts, 200k updates, 200k deletes**.

```bash
# With the Docker Compose stack running
go run benchmark/main.go

# Custom settings
API_BASE_URL=http://localhost:8080 \
CONCURRENCY=100 \
INSERT_COUNT=500000 \
UPDATE_COUNT=200000 \
DELETE_COUNT=200000 \
go run benchmark/main.go
```

The benchmark reports throughput (events/sec) and duration for each phase.

## Python SDK

A zero-dependency Python client library is included in `sdk/python/`.

```python
from event_client import EventClient

client = EventClient("http://localhost:8080")

# Track an event
event_id = client.track_event(
    conversation_id="conv_123",
    event_name="event_a",
    input="What is the weather?",
    output="It's sunny!",
    latency=142.5,
)

# Update it
client.update_event(event_id, error="timeout", latency=5000.0)

# Delete it
client.delete_event(event_id)

# Analytics
errors = client.get_error_counts("event_a", days=7)
sessions = client.get_top_error_sessions(limit=10)
```

Install with:
```bash
cd sdk/python && pip install -e .
```

See [sdk/python/README.md](sdk/python/README.md) for full documentation.

## Project Structure

```
├── cmd/
│   ├── api/               # HTTP API server entrypoint
│   └── consumer/          # Kafka consumer entrypoint
├── internal/
│   ├── api/               # HTTP handlers & router (chi)
│   ├── clickhouse/        # Client, migrations, repository
│   ├── kafka/             # Producer & consumer
│   └── models/            # Shared data types
├── benchmark/             # Load testing tool
├── sdk/python/            # Python client SDK
├── docker-compose.yml     # Full stack orchestration
├── Dockerfile.api         # Multi-stage API build
├── Dockerfile.consumer    # Multi-stage consumer build
└── ARCHITECTURE.md        # Deep-dive design documentation
```

## Key Design Decisions

| Decision | Why |
|----------|-----|
| **ReplacingMergeTree** | Updates/deletes as versioned appends — no expensive ALTER TABLE mutations |
| **Single Kafka topic** | Ordering guarantee per event ID (create before update before delete) |
| **Event ID as partition key** | All ops for same event → same partition → ordered processing |
| **Batch consumer for creates** | ClickHouse batch insert is 100-1000x faster than individual inserts |
| **Soft deletes** | Append-only delete consistent with ReplacingMergeTree's nature |
| **PARTITION BY month** | Efficient time-range pruning, easy old data cleanup |

For the full design deep-dive, trade-off analysis, scaling considerations, and "what breaks at 1 billion rows" — see **[ARCHITECTURE.md](ARCHITECTURE.md)**.

## Configuration

All configuration is via environment variables:

| Variable                    | Default              | Description                    |
|-----------------------------|----------------------|--------------------------------|
| `API_PORT`                  | `8080`               | HTTP server port               |
| `CLICKHOUSE_HOST`           | `localhost`          | ClickHouse hostname            |
| `CLICKHOUSE_PORT`           | `9000`               | ClickHouse native port         |
| `CLICKHOUSE_DATABASE`       | `event_analytics`    | Database name                  |
| `CLICKHOUSE_USER`           | `default`            | ClickHouse username            |
| `CLICKHOUSE_PASSWORD`       | (empty)              | ClickHouse password            |
| `KAFKA_BROKERS`             | `localhost:9092`     | Kafka broker addresses         |
| `KAFKA_TOPIC`               | `events`             | Kafka topic name               |
| `KAFKA_GROUP_ID`            | `event-consumer-group` | Consumer group ID            |
| `CONSUMER_BATCH_SIZE`       | `500`                | Max events per batch insert    |
| `CONSUMER_FLUSH_INTERVAL_MS`| `200`                | Max time before batch flush    |

## Tech Stack

- **Go** — API server & Kafka consumer
- **Apache Kafka** — Message broker (Confluent Platform 7.6)
- **ClickHouse** — Columnar OLAP database (v24.3)
- **chi** — Lightweight HTTP router
- **kafka-go** — Pure Go Kafka client
- **clickhouse-go** — Official ClickHouse Go driver
- **Docker Compose** — Orchestration