# Event Analytics SDK - Python Client

A lightweight Python client library for the Event Analytics Pipeline API. Zero external dependencies — uses only Python's standard library.

## Installation

```bash
# From the sdk/python directory
pip install -e .
```

Or simply copy `event_client.py` into your project.

## Quick Start

```python
from event_client import EventClient

# Initialize the client
client = EventClient("http://localhost:8080")

# Check if the API is available
if not client.health_check():
    print("API is not available!")
    exit(1)

# Track a new event
event_id = client.track_event(
    conversation_id="conv_123",
    event_name="event_a",
    input="What is the weather today?",
    output="It's sunny and 75°F.",
    latency=142.5,
    conversation_metadata='{"user_id": "u_456", "session": 7}',
)
print(f"Tracked event: {event_id}")
```

## API Reference

### Initialization

```python
client = EventClient(
    base_url="http://localhost:8080",  # API server URL
    timeout=30.0,                      # HTTP timeout in seconds
    max_retries=3,                     # Retry count for failed requests
    retry_delay=1.0,                   # Base delay between retries (doubles each attempt)
)
```

### Write Operations

All write operations are **asynchronous** — they publish messages to Kafka, which are then processed by the consumer and written to ClickHouse. The event ID is returned immediately, but the event may not be queryable for a brief moment.

#### `track_event(...)` → `str`

Create a new event. Returns the generated event ID.

```python
event_id = client.track_event(
    conversation_id="conv_123",       # Required: conversation/session ID
    event_name="event_a",             # Required: "event_a", "event_b", or "event_c"
    error="",                         # Optional: error message (empty = no error)
    latency=142.5,                    # Optional: latency in milliseconds
    input="user query",               # Optional: input text
    input_embeddings=[0.1, 0.2, ...], # Optional: input vector embeddings
    output="bot response",            # Optional: output text
    output_embeddings=[0.3, 0.4, ...],# Optional: output vector embeddings
    conversation_metadata='{"k":"v"}',# Optional: JSON metadata string
)
```

#### `update_event(event_id, ...)` → `dict`

Update an existing event. Only the provided fields will be updated.

```python
client.update_event(
    event_id="uuid-here",
    error="timeout",          # Optional
    latency=5000.0,           # Optional
    output="updated response",# Optional
    event_name="event_b",     # Optional
)
```

#### `delete_event(event_id)` → `dict`

Soft-delete an event. The event is marked as deleted but not physically removed.

```python
client.delete_event("uuid-here")
```

### Read Operations

All read operations go **directly to ClickHouse** and return immediately.

#### `get_event(event_id)` → `dict | None`

Get a single event by ID. Returns `None` if not found.

```python
event = client.get_event("uuid-here")
if event:
    print(event["event_name"], event["latency"])
```

#### `get_events_by_conversation(conversation_id)` → `list[dict]`

Get all events for a conversation, ordered by creation time.

```python
events = client.get_events_by_conversation("conv_123")
for event in events:
    print(f"{event['event_name']}: {event['latency']}ms")
```

### Analytics Operations

#### `get_error_counts(event_name, days=7)` → `list[dict]`

Get error counts per day for a given event name.

*Answers: "How many errors in event_a over the last 7 days?"*

```python
errors = client.get_error_counts("event_a", days=7)
for day in errors:
    print(f"{day['date']}: {day['error_count']}/{day['total_count']} errors")
```

#### `get_latency_stats(limit=100)` → `list[dict]`

Get p95 latency per event type per session.

*Answers: "p95 latency per event type per session"*

```python
stats = client.get_latency_stats(limit=50)
for s in stats:
    print(f"Session {s['conversation_id']} | {s['event_name']}: p95={s['p95_latency']:.1f}ms")
```

#### `get_top_error_sessions(limit=100)` → `list[dict]`

Get top sessions ranked by error rate.

*Answers: "Top sessions by error rate"*

```python
sessions = client.get_top_error_sessions(limit=10)
for s in sessions:
    print(f"Session {s['conversation_id']}: {s['error_rate']:.1%} error rate ({s['error_events']}/{s['total_events']})")
```

### Utility Methods

#### `health_check()` → `bool`

Check if the API server is healthy.

```python
if client.health_check():
    print("Server is up!")
```

#### `wait_until_ready(timeout=60.0, interval=2.0)` → `bool`

Block until the API server is ready, or timeout.

```python
if client.wait_until_ready(timeout=30):
    print("Server is ready!")
else:
    print("Server did not start in time.")
```

## Error Handling

The SDK raises typed exceptions for different error scenarios:

```python
from event_client import EventClient, EventSDKError, EventNotFoundError, EventValidationError

client = EventClient("http://localhost:8080")

try:
    client.track_event(
        conversation_id="conv_1",
        event_name="invalid_name",  # Will raise EventValidationError
    )
except EventValidationError as e:
    print(f"Validation error: {e}")
except EventNotFoundError as e:
    print(f"Not found: {e}")
except EventSDKError as e:
    print(f"API error (status {e.status_code}): {e}")
```

## Full Example

```python
import time
import json
from event_client import EventClient

client = EventClient("http://localhost:8080")
client.wait_until_ready(timeout=30)

# Simulate a conversation with multiple events
conv_id = "conversation_demo_001"

# Event 1: User sends a message
e1 = client.track_event(
    conversation_id=conv_id,
    event_name="event_a",
    input="Tell me about Python.",
    output="Python is a versatile programming language...",
    latency=230.0,
    conversation_metadata=json.dumps({"user_id": "u_42", "channel": "web"}),
)

# Event 2: Follow-up question
e2 = client.track_event(
    conversation_id=conv_id,
    event_name="event_b",
    input="What about async support?",
    output="Python supports async/await since version 3.5...",
    latency=180.0,
)

# Event 3: An error occurs
e3 = client.track_event(
    conversation_id=conv_id,
    event_name="event_c",
    input="Show me an example",
    error="context_length_exceeded",
    latency=5200.0,
)

# Wait for consumer to process
time.sleep(3)

# Query the conversation
events = client.get_events_by_conversation(conv_id)
print(f"Found {len(events)} events in conversation {conv_id}")

# Check error analytics
errors = client.get_error_counts("event_c", days=1)
print(f"Event C errors today: {errors}")

# Fix the errored event
client.update_event(e3, error="", output="Here's an async example...", latency=350.0)
print(f"Fixed event {e3}")
```

## Notes

- **No external dependencies**: Uses only Python's `urllib` from the standard library
- **Thread-safe**: Each request is independent; safe to use from multiple threads
- **Async writes**: Create/update/delete operations return immediately but are processed asynchronously through Kafka. There is a small delay before changes are visible in read queries
- **Python 3.9+**: Uses modern type hints; compatible with Python 3.9 and later