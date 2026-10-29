# Logistics API — DEUS Go Backend Challenge

A Go REST API that tracks **cargoes**, the **vessels** that carry them and the **movement history** of every cargo.
It includes the bonus **event-driven pipeline**: each cargo status change is published to RabbitMQ and stored by a background consumer.
On top of that it has **OpenTelemetry observability**: per-endpoint latency metrics and distributed traces, with a Grafana dashboard ready to use.

| | |
|---|---|
| Language | Go 1.26 (modules), `chi` router, `zap` structured logs |
| Storage | PostgreSQL 16 via `pgx/v5`, raw SQL, `golang-migrate` migrations embedded in the binary |
| Messaging | RabbitMQ (quorum queues, publisher confirms, dead-letter queue) |
| Observability | OpenTelemetry → Collector → Jaeger (traces) + Prometheus (metrics) → Grafana |
| Tests | unit + integration + end-to-end, **93% combined coverage**, race detector on |

---

## Contents

1. [Quick start (Docker)](#quick-start-docker)
2. [Local development](#local-development)
3. [API documentation](#api-documentation)
4. [Event pipeline (bonus)](#event-pipeline-bonus)
5. [Observability](#observability)
6. [Testing](#testing)
7. [Design decisions](#design-decisions)
8. [Resilience and edge cases](#resilience-and-edge-cases)
9. [Project layout](#project-layout)

---

## Quick start (Docker)

Requirements: Docker with Compose v2 and `make`.

```bash
make docker-run      # builds the API image and starts the whole stack
```

| Service | URL |
|---|---|
| **API** | http://localhost:8080 |
| Grafana dashboard "Logistics API" | http://localhost:3000 (no login) |
| Jaeger (traces) | http://localhost:16686 |
| Prometheus | http://localhost:9090 |
| RabbitMQ management | http://localhost:15672 (`deus_user` / `deus_pass`) |

Try it:

```bash
VESSEL=$(curl -s -X POST localhost:8080/api/v1/vessels -H 'Content-Type: application/json' \
  -d '{"name":"Ever Given","capacity_kg":20000,"current_location":"Rotterdam"}' | jq -r .data.id)

CARGO=$(curl -s -X POST localhost:8080/api/v1/cargoes -H 'Content-Type: application/json' \
  -d "{\"name\":\"Coffee beans\",\"weight_kg\":1200,\"vessel_id\":\"$VESSEL\"}" | jq -r .data.id)

curl -s -X PATCH localhost:8080/api/v1/cargoes/$CARGO/status -H 'Content-Type: application/json' \
  -d '{"status":"in_transit","location":"Suez Canal"}'

curl -s localhost:8080/api/v1/cargoes/$CARGO/tracking   # full movement history
curl -s localhost:8080/api/v1/cargoes/$CARGO/events     # events stored by the consumer
```

Other targets: `make docker-logs`, `make docker-down` (also deletes volumes), `make help`.

> Postgres is published on host port **5433**, so it does not clash with a Postgres already installed locally. Override it with `POSTGRES_HOST_PORT`.

## Local development

```bash
make deps-up   # Postgres + RabbitMQ only
make run       # go run ./cmd/api  (migrations run automatically at startup)
```

Configuration comes from environment variables. The defaults work with `make deps-up`.

| Variable | Default | Purpose |
|---|---|---|
| `HTTP_ADDR` | `:8080` | listen address |
| `DATABASE_URL` | `postgres://deus:deus@localhost:5433/logistics?sslmode=disable` | Postgres |
| `RABBITMQ_URL` | `amqp://deus_user:deus_pass@localhost:5672/` | RabbitMQ |
| `REQUEST_TIMEOUT` | `5s` | deadline for every request (DB calls observe it) |
| `SHUTDOWN_TIMEOUT` | `15s` | time allowed to drain in-flight requests |
| `DB_MAX_CONNS` / `DB_STATEMENT_TIMEOUT` / `DB_CONNECT_ATTEMPTS` | `20` / `5s` / `10` | pool size, server-side query limit, startup retries |
| `RELAY_ENABLED` / `CONSUMER_ENABLED` | `true` / `true` | run the outbox relay / event consumer in this process |
| `OUTBOX_POLL_INTERVAL` / `OUTBOX_BATCH_SIZE` | `500ms` / `100` | relay tuning |
| `IDEMPOTENCY_TTL` | `24h` | how long `Idempotency-Key` responses are kept |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | *(empty = telemetry off)* | e.g. `http://localhost:4317` |
| `OTEL_SERVICE_NAME` | `logistics-api` | service name in traces and metrics |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

---

## API documentation

Base path: `/api/v1`. Request bodies must be `Content-Type: application/json`. All timestamps are RFC 3339 in UTC.

**Response envelope.** Success responses are `{"data": ...}`. Lists add `"meta": {"limit", "offset", "count"}`. Errors are always:

```json
{ "error": { "code": "validation_failed", "message": "request validation failed",
             "details": { "weight_kg": "must be greater than 0" } } }
```

### Endpoints

| Method | Path | Description | Success |
|---|---|---|---|
| `POST` | `/vessels` | Create a vessel (supports `Idempotency-Key`) | 201 |
| `GET` | `/vessels?limit=&offset=` | List vessels | 200 |
| `GET` | `/vessels/{id}` | Vessel details: name, capacity, current location, load | 200 |
| `PATCH` | `/vessels/{id}/location` | Move a vessel | 200 |
| `POST` | `/cargoes` | Create a cargo on a vessel (supports `Idempotency-Key`) | 201 |
| `GET` | `/cargoes?status=&vessel_id=&limit=&offset=` | List and filter cargoes | 200 |
| `GET` | `/cargoes/{id}` | Get a cargo (returns `ETag`) | 200 |
| `PATCH` | `/cargoes/{id}/status` | Change status (supports `If-Match`, `Idempotency-Key`) | 200 |
| `POST` | `/cargoes/{id}/tracking` | Record a movement without changing status (supports `Idempotency-Key`) | 201 |
| `GET` | `/cargoes/{id}/tracking` | Complete movement history, oldest first | 200 |
| `GET` | `/cargoes/{id}/events` | Status-change events stored by the consumer | 200 |
| `GET` | `/healthz` | Liveness (process is up) | 200 |
| `GET` | `/readyz` | Readiness (Postgres required; RabbitMQ reported as "degraded") | 200 / 503 |

Pagination: `limit` is 1–100 (default 20), `offset` ≥ 0.

### Vessels

```http
POST /api/v1/vessels
{ "name": "Ever Given", "capacity_kg": 20000, "current_location": "Rotterdam" }
```
```json
201 Created
Location: /api/v1/vessels/01a18920-e2a7-754b-b25f-698f83d4a062
{ "data": { "id": "01a18920-e2a7-754b-b25f-698f83d4a062", "name": "Ever Given",
            "capacity_kg": 20000, "current_location": "Rotterdam",
            "load_kg": 0, "available_capacity_kg": 20000,
            "created_at": "2026-10-29T19:26:36.199329Z", "updated_at": "2026-10-29T19:26:36.199329Z" } }
```

`load_kg` is the weight of cargo on board that has not been delivered yet. Vessel names are unique; a duplicate returns `409 already_exists`.

```http
PATCH /api/v1/vessels/{id}/location
{ "current_location": "Singapore" }
```

### Cargoes

```http
POST /api/v1/cargoes
Idempotency-Key: 7f1c2c1e-order-42          (optional)
{ "name": "Coffee beans", "description": "Arabica", "weight_kg": 1200, "vessel_id": "01a18920-e2a7-..." }
```
```json
201 Created
ETag: "1"
{ "data": { "id": "01a18920-e309-71a7-aef3-9989b6091bfb", "name": "Coffee beans", "description": "Arabica",
            "weight_kg": 1200, "status": "pending", "vessel_id": "01a18920-e2a7-...", "version": 1,
            "created_at": "...", "updated_at": "..." } }
```

- New cargo always starts as `pending`. Clients cannot choose the initial status.
- Creation records the first tracking entry at the vessel's current location.
- `409 vessel_capacity_exceeded` if the cargo does not fit on the vessel. `422` if `vessel_id` does not exist.

### Status changes

```http
PATCH /api/v1/cargoes/{id}/status
If-Match: "1"                                (optional: optimistic concurrency)
{ "status": "in_transit", "location": "Suez Canal", "note": "left port" }
```
```json
200 OK
ETag: "2"
{ "data": { "id": "...", "status": "in_transit", "version": 2, "...": "..." } }
```

The status lifecycle only moves **forward**:

```
pending ──► in_transit ──► delivered
```

Repeating the current status, going backwards or skipping a step (`pending → delivered`) returns `409 invalid_status_transition` and **emits no event**:

```json
{ "error": { "code": "invalid_status_transition",
             "message": "update cargo ... status: invalid status transition: cannot move from in_transit to pending" } }
```

`location` is optional and defaults to the vessel's current location. Every successful change adds a tracking entry and an event.

### Tracking history

```http
POST /api/v1/cargoes/{id}/tracking
{ "location": "Strait of Malacca", "note": "on schedule" }
```

```http
GET /api/v1/cargoes/{id}/tracking
```
```json
{ "data": [
  { "id": "...", "cargo_id": "...", "status": "pending",    "location": "Rotterdam",         "note": "cargo registered", "recorded_at": "..." },
  { "id": "...", "cargo_id": "...", "status": "in_transit", "location": "Suez Canal",                                    "recorded_at": "..." },
  { "id": "...", "cargo_id": "...", "status": "in_transit", "location": "Strait of Malacca", "note": "on schedule",      "recorded_at": "..." },
  { "id": "...", "cargo_id": "...", "status": "delivered",  "location": "Singapore",                                     "recorded_at": "..." } ] }
```

A delivered cargo accepts no more movements (`409 cargo_already_delivered`).

### Error codes

| HTTP | `code` | When |
|---|---|---|
| 400 | `bad_request` | malformed JSON, unknown field, trailing data, invalid UUID in path or query, bad `If-Match` |
| 404 | `not_found` / `route_not_found` | unknown resource / unknown path |
| 405 | `method_not_allowed` | e.g. `DELETE` on a cargo (history is never deleted) |
| 409 | `invalid_status_transition`, `vessel_capacity_exceeded`, `cargo_already_delivered`, `already_exists` | business rule violations |
| 412 | `version_mismatch` | `If-Match` does not match the current version |
| 413 | `payload_too_large` | body over 1 MiB |
| 415 | `unsupported_media_type` | body that is not `application/json` |
| 422 | `validation_failed` (with `details`), `idempotency_key_reused` | invalid field values; same key used for a different request |
| 500 | `internal_error` | unexpected failure (details are logged, never returned) |
| 503 | `service_unavailable` + `Retry-After: 5` | database down, overloaded or timed out |

### Retries and concurrent clients

- **`Idempotency-Key`.** Send it on `POST`/`PATCH`. A retry, or a duplicate sent at the same moment, gets the **original response** back (byte for byte, with the header `Idempotent-Replayed: true`) and the side effect happens only once. Reusing a key with a different body returns `422`. 5xx responses are not remembered, so retrying after a server error is safe.
- **`ETag` / `If-Match`.** `GET /cargoes/{id}` returns `ETag: "<version>"`. Send it back as `If-Match` on a status change to reject the change if someone else updated the cargo first (`412`).

---

## Event pipeline (bonus)

```
HTTP PATCH /status
   │  one DB transaction:
   │    UPDATE cargoes  +  INSERT tracking_entries  +  INSERT outbox
   ▼
outbox table ──► Outbox relay (goroutine) ──publish + confirm──► RabbitMQ
                                                                 exchange  cargo.events (topic)
                                                                 queue     cargo.status_changed (quorum)
                                                                 DLQ       cargo.status_changed.dlq
                                                                      │
                     Event consumer (goroutine) ◄─────────────────────┘
                        logs + INSERT INTO cargo_events ON CONFLICT DO NOTHING, then ack
```

Event payload (routing key `cargo.status_changed`):

```json
{ "event_id": "01a18920-e332-...", "cargo_id": "01a18920-e309-...",
  "old_status": "pending", "new_status": "in_transit", "timestamp": "2026-10-29T19:26:36.338845Z" }
```

- **Transactional outbox.** The event is written in the same transaction as the status change. An event exists if and only if the change was committed, even if the process crashes or RabbitMQ is down.
- **Relay.** Claims pending rows with `FOR UPDATE SKIP LOCKED`, so several API replicas can run it safely. It publishes with **publisher confirms** and marks a row published only after the broker acknowledges it. It stops a batch at the first failure to keep events in order, then backs off exponentially with jitter.
- **Consumer.** Uses manual acks and prefetch 10. It stores each event **idempotently** (`cargo_events.id` = `event_id`), so redeliveries are harmless.
  - Malformed messages, or events for unknown cargo, are rejected to the **DLQ**.
  - While Postgres is down it retries the same message instead of requeueing it, so an outage cannot push healthy messages into the DLQ.
  - The quorum queue's `x-delivery-limit` dead-letters a message that keeps crashing the worker.
- The trace context travels from the HTTP request, through the outbox row and the AMQP headers, to the consumer. Jaeger shows **one trace** from request to stored event.
- The relay and consumer run inside the API process for simplicity. `RELAY_ENABLED` / `CONSUMER_ENABLED` let you run them as separate deployments without code changes.

---

## Observability

The API exports OTLP to the OpenTelemetry Collector. The collector sends traces to **Jaeger** and metrics to **Prometheus**, and **Grafana** displays them. A collector outage never affects requests: export is asynchronous, bounded and drops on overflow.

**Grafana → "Logistics API" dashboard** (provisioned automatically):

- p50 / p95 / p99 latency **per endpoint**, throughput per endpoint and status code, 5xx error ratio
- cargo status transitions (`pending → in_transit`, ...)
- outbox backlog: events not yet confirmed by RabbitMQ (the first sign of a broker outage)
- events published vs. publish failures vs. consumed (by outcome: stored / duplicate / rejected), and consumer latency

| Metric (Prometheus name) | Source |
|---|---|
| `http_server_request_duration_seconds` (labels `http_route`, `http_request_method`, `http_response_status_code`) | `otelhttp` |
| `cargo_status_transitions_total{from,to}` | service |
| `events_published_total`, `events_publish_failures_total` | outbox relay |
| `events_consumed_total{outcome}`, `event_consume_duration_seconds` | consumer |
| `outbox_pending` | gauge over the outbox table |

The latency histogram is labelled with the **route pattern** (`/api/v1/cargoes/{cargoID}`), never the raw path. One time series per cargo id would blow up Prometheus. A regression test enforces this.

**Traces** (Jaeger): every request has spans for the handler and each SQL statement (`otelpgx`), continuing into `publish cargo.status_changed` and `consume cargo.status_changed`.

**Logs**: JSON via zap, with `request_id`, `trace_id` and `span_id` on every request log, so a log line leads straight to its trace.

---

## Testing

```bash
make test               # unit tests, no infrastructure (race detector on)
make test-integration   # unit + integration + end-to-end against disposable Postgres & RabbitMQ (docker-compose.test.yml)
make test-docker        # the same suite, run entirely inside Docker (no local Go needed)
make coverage           # combined coverage report (coverage.html); fails under 80%
make lint               # gofmt + go vet
```

| Layer | How it is tested |
|---|---|
| `domain` | pure unit tests: the full 3×3 transition matrix, validation |
| `service` | in-memory fakes whose fake transaction **rolls back**, so tests prove "nothing written on failure" |
| `transport/httpapi` | `httptest` with fake services: every status code, envelope shape and malformed input |
| `events` | relay and consumer logic with fake store and broker: outages, duplicates, poison messages |
| `repository/postgres` | integration tests on a real Postgres: SQL, constraints, triggers, locking |
| `events/rabbitmq` | integration tests on a real RabbitMQ: publish/consume, DLQ, reconnect |
| `app` | end-to-end: the real binary wiring against Postgres + RabbitMQ |

**Regression tests for the tricky parts** (each name describes the guarantee it protects):

| Guarantee | Test |
|---|---|
| Only forward transitions | `TestStatus_TransitionMatrix`, `TestUpdateStatus_RejectedTransitionsEmitNothing` |
| 10 racing status updates → 1 success, 1 tracking row, 1 event | `TestConcurrentStatusUpdates_ExactlyOneWins` |
| Concurrent cargo creation never overbooks a vessel | `TestConcurrentCargoCreation_NeverExceedsCapacity` |
| 20 identical POSTs at once with one `Idempotency-Key` → 1 cargo, identical responses | `TestEndToEnd_ConcurrentDuplicatePostsCreateOneCargo` |
| A duplicate waits for the in-flight original | `TestIdempotency_ConcurrentDuplicateWaitsForFirst` |
| Stale `If-Match` → 412 | `TestEndToEnd_OptimisticConcurrencyWithIfMatch` |
| RabbitMQ down: writes still succeed, events delivered exactly once later | `TestEndToEnd_BrokerOutageLosesNoEvents` |
| Postgres dies at runtime → 503 + `Retry-After`, readiness 503, liveness 200 | `TestEndToEnd_DatabaseOutageReturns503` |
| Redelivered event stored once | `TestHandler_DuplicateDeliveryIsStoredOnce`, `TestCargoEventRepository_SaveIsIdempotent` |
| DB outage does not dead-letter healthy events | `TestHandler_RetriesWhileDatabaseIsUnavailable` |
| Poison message → DLQ | `TestRejectedMessagesAreDeadLettered` |
| Two relays never publish the same row | `TestOutbox_SkipLockedSplitsWorkBetweenRelays` |
| History cannot be modified (UPDATE/DELETE/TRUNCATE) | `TestAuditTables_AreAppendOnly` |
| Graceful shutdown drains in-flight requests | `TestServe_DrainsInFlightRequestsOnShutdown` |
| Latency metric uses route patterns, not ids | `TestLatencyMetricUsesRoutePattern` |

---

## Design decisions

**Architecture: ports and adapters.** `domain` holds the entities and rules (no dependencies). `service` holds the use cases and *declares* the interfaces it needs. `repository/postgres` and `events/rabbitmq` are adapters that implement them. `transport/httpapi` is the REST edge. `internal/app` is the single composition root where everything is wired with constructor injection, with no globals.

| Pattern | Where | Why |
|---|---|---|
| Repository | `repository/postgres` | SQL behind intention-named methods (`GetForUpdate`, `Append`) |
| Unit of Work | `TxManager.WithinTx` (transaction carried in `ctx`, nested calls become savepoints) | status + tracking + outbox (+ idempotency record) commit atomically |
| State machine | `domain/status.go`: one transitions table | rules are declarative and live in one place |
| Domain events | `Cargo.ChangeStatus` returns `StatusChanged` | the domain says *what* happened; infrastructure decides delivery |
| Transactional outbox + idempotent consumer | `outbox`, `cargo_events` | reliable at-least-once delivery without two-phase commit |
| Adapter | `rabbitmq.Publisher` implements `events.Publisher` | the broker can be swapped (e.g. Kafka) without touching the relay |
| Decorator / middleware | request id → route tagging → access log → recover → timeout; `idempotencyGuard` | cross-cutting concerns stay out of the handlers |
| Strategy | `backoff.Policy` | one retry policy shared by DB connect, relay and consumer; zero-delay in tests |
| Functional options | `events.NewRelay(..., WithInterval(), WithBatchSize())` | readable constructors with sane defaults |
| DTO + mapper | `transport/httpapi/dto.go` | the API contract is independent of domain structs |

**Data model.**

```
vessels 1 ──< cargoes 1 ──< tracking_entries   (append-only)
                      1 ──< cargo_events       (append-only, filled by the consumer)
outbox            (events waiting to be published)
idempotency_keys  (stored responses for Idempotency-Key)
```

- Foreign keys use `ON DELETE RESTRICT`. `CHECK` constraints back the domain validation (weight > 0, etc.), `cargo_status` is a Postgres enum, and vessel names are unique.
- Indexes match the queries: `(cargo_id, recorded_at)` for history, `(vessel_id, status)` for load and filters, a partial index on unpublished outbox rows, and `created_at` for idempotency cleanup.
- **Auditability is enforced by the database, not just the code.** A trigger rejects `UPDATE`, `DELETE` and `TRUNCATE` on `tracking_entries` and `cargo_events`. Cargo has no `DELETE` endpoint.
- IDs are **UUIDv7**: time-ordered, which keeps B-tree inserts local.
- Migrations are plain SQL, embedded in the binary and applied at startup. golang-migrate takes an advisory lock, so replicas cannot migrate twice.

**Why these libraries.** chi gives `net/http`-compatible routing, plus the route pattern needed for metric labels. zap is listed in the spec and gives fast structured logs. pgx with raw SQL keeps queries explicit and reviewable (locking clauses like `FOR UPDATE SKIP LOCKED` would fight an ORM). amqp091-go is the official RabbitMQ client.

**Capacity locking detail.** `VesselRepository.GetForUpdate` locks the vessel and then computes its load in a *second* statement. Under READ COMMITTED, a single `SELECT ... FOR UPDATE` with a load subquery uses the snapshot from *before* it waited for the lock. It would miss cargo committed by the transaction it waited on, and the vessel could be overbooked. The concurrent-creation regression test covers this.

## Resilience and edge cases

| Situation | Behaviour |
|---|---|
| Same request sent twice at the same time | `Idempotency-Key` row is claimed inside the business transaction. The duplicate blocks on the primary key, then replays the committed response (or runs normally if the first rolled back) |
| Two clients change the same cargo at once | `SELECT ... FOR UPDATE` serialises them: one wins, the other gets 409 (or 412 with `If-Match`) |
| Many cargoes created on a nearly full vessel | the vessel row is locked during the capacity check, so it is never overbooked |
| Postgres not ready at startup | ping with exponential backoff (`DB_CONNECT_ATTEMPTS`), then exit non-zero so Docker/k8s restarts the API |
| Postgres down or slow at runtime | request deadline + `statement_timeout`. Connection errors map to **503 + `Retry-After`**. `/readyz` → 503, `/healthz` stays 200 so the orchestrator stops sending traffic without restart loops |
| RabbitMQ down | API keeps working: events wait in the outbox. The relay backs off and reconnects, and `/readyz` reports `degraded`. Events are delivered when the broker returns, with nothing lost |
| Broker confirm lost / crash after publish | message republished. The consumer deduplicates by `event_id` |
| Consumer's DB down | retries the same delivery with backoff, without requeue churn or false dead-lettering |
| Malformed or unknown-cargo event | rejected to the dead-letter queue, logged with its payload |
| OTel collector down | telemetry export fails quietly (logged). Requests are unaffected |
| SIGTERM | stop accepting connections → drain in-flight requests → stop workers (unfinished message is requeued) → flush telemetry → close pools |
| Panic in a handler | recovered: JSON 500, stack trace logged with the trace id |
| Invalid input | unknown fields, trailing JSON, wrong types, empty or oversized body, wrong content type, bad UUIDs, out-of-range pagination: each gets a specific 4xx before reaching business code |

**Trade-offs / next steps.** Offset pagination is simple but drifts on large tables; keyset pagination over `(created_at, id)` would fix that, and the index already exists. There is no authentication, since the spec does not ask for it. In production the relay and consumer would usually run as separate deployments; the env flags already allow it.

## Project layout

```
cmd/api/                     entry point (config, logger, signals)
internal/
  app/                       composition root, lifecycle, graceful shutdown
  config/                    env configuration
  domain/                    entities, state machine, validation, domain errors
  service/                   use cases + ports (interfaces)
  repository/postgres/       pgx adapters, Unit of Work, error translation, migrations runner
  transport/httpapi/         router, handlers, DTOs, middleware, idempotency, error mapping
  events/                    wire format, outbox relay, consumer handler
  events/rabbitmq/           RabbitMQ publisher, consumer, topology
  idempotency/               Idempotency-Key record + store port
  telemetry/                 OpenTelemetry setup + business metrics
  backoff/                   retry strategy
  logger/                    zap setup + trace/request correlation
migrations/                  SQL migrations (embedded)
deploy/                      otel-collector, prometheus, grafana (datasources + dashboard)
```
