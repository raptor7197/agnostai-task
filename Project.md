# 🛠️ Intern Take-Home Task — Event Analytics Pipeline

**Time:** ~6 hours from now. No hard deadline though.  
**Tools:** Use any coding agent, AI tool, or the internet freely.  
**Goal:** We care about your decisions and understanding - not just working code. Need not to be perfect, It's fine if something is incomplete, just document it

## Context
You're building the core ingestion and storage layer for an event tracking system. Events flow from an **API → Kafka → Go consumer → ClickHouse**. Think of it like the backend of a product analytics tool (think Mixpanel internals).

## Data Model
event -
id
conversation_id
conversation_metadata
event_name (event_a | event_b | event_c)
error
latency
input
input_embeddings
output
output_embeddings


**Note:** one conversation will have multiple events!

**Design DB as you like.**

## What to Build

### 1. HTTP API (Go)
Expose REST endpoints:
- `POST /event` - Create an event (with io)
- `PUT /event/:id` - Update event fields  
- `DELETE /event/:id` - Delete event

**All writes (insert/update/delete) go through Kafka** — the API publishes a message, a consumer picks it up and writes to ClickHouse. **Reads can go directly to ClickHouse.**

### 2. Kafka
- One or more topics of your choice (document why you structured them this way)
- Consumer written in Go

### 3. ClickHouse Schema
**Design the schema yourself.** Things to think about:
- This will grow to **100s of millions of rows** in the events table
- You need to answer queries like:
  - How many errors in event_a over the last 7 days?
  - p95 latency per event type per session
  - All events in session X ordered by time
  - Top sessions by error rate

### 4. Brownie Points (Optional but impressive)
- **Docker Compose** - full setup: Kafka, Zookeeper, ClickHouse, Go service, all wired. `docker compose up` should give a running system.
- **SDK** - a small client library(any language) that wraps the API for ingestion. Something a developer would import and call `client.TrackEvent(...)`.

## Deliverables
- [ ] **GitHub repo:** clean commits, not one giant dump
- [ ] **Loom video** (any length is fine): walk through the system via terminal. Show it working. Doesn't need to be polished, just talk through your choices.
- [ ] **Benchmark results:** ingest 500k entries, then run 200k deletes and 200k updates. Report time taken for each.

## What We're Evaluating
We're not just checking if it works. We'll do a **deep-dive conversation** after where we'll ask things like:
- Why did you pick this Kafka topic structure?
- What breaks at 1 billion rows and how would you fix it?
- What did you consider and decide against?
- If you had 4 more hours, what would you improve?

## Tips
- **ClickHouse is not Postgres**, understand it a bit before starting
- Don't over-engineer, but be ready to explain how you'd scale what you built
- It's fine if something is incomplete, **just document it**

**Good luck. Questions are welcome. Feel free to reach me out at +91 8460141275 on WA**
