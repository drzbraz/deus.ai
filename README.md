# Logistics API — DEUS Go Backend Challenge

[![CI](https://github.com/drzbraz/deus.ai/actions/workflows/ci.yml/badge.svg)](https://github.com/drzbraz/deus.ai/actions/workflows/ci.yml)
[![Quality Gate](https://sonarcloud.io/api/project_badges/measure?project=drzbraz_deus.ai&metric=alert_status)](https://sonarcloud.io/project/overview?id=drzbraz_deus.ai)
[![Coverage](https://sonarcloud.io/api/project_badges/measure?project=drzbraz_deus.ai&metric=coverage)](https://sonarcloud.io/project/overview?id=drzbraz_deus.ai)

A Go REST API that tracks **cargoes**, the **vessels** that carry them and the **movement history** of every cargo.
It includes the bonus **event-driven pipeline**: each cargo status change is published to RabbitMQ and stored by a background consumer.
On top of that it has **OpenTelemetry observability**: per-endpoint latency metrics and distributed traces, with a Grafana dashboard ready to use.

| | |
|---|---|
| Language | Go 1.26 (modules), `chi` router, `zap` structured logs |
| Storage | PostgreSQL 16 via `pgx/v5`, raw SQL, `golang-migrate` migrations embedded in the binary |
| Messaging | RabbitMQ (quorum queues, publisher confirms, dead-letter queue) |
| Observability | OpenTelemetry → Collector → Jaeger (traces) + Prometheus (metrics) → Grafana |
| Tests | unit, integration and black-box e2e / resilience / schema suites (267 test functions), **94–95% coverage**, race detector on |

---

## Interviewer's guide

Everything below maps the [challenge brief](#requirements-checklist) item by item to where it is implemented and how to check it. **Every requirement and the full bonus are delivered.**

### Run it (Docker is the only requirement)

```bash
make docker-run   # builds the API image, starts API + Postgres + RabbitMQ + observability; API on http://localhost:8080
make demo         # guided 12-step tour of the API; every step asserts its HTTP status
make test-docker  # the whole test suite inside Docker (no local Go needed)
```

Then open **http://localhost:8080/docs** to try the API in Swagger UI. `make docker-down` stops everything.

### Requirements checklist

✅ = delivered. Paths are relative to the repository root; test names can be found with `grep -rn <name>`.

**Technical information**

| Requirement | ✓ | Where | How to verify |
|---|---|---|---|
| Go 1.21+, Go modules | ✅ | `go.mod` (Go 1.26, toolchain pinned to the patched 1.26.10) | `make build` |
| Clean, modular architecture (handlers, services, repositories, models) | ✅ | `internal/transport/httpapi` → `internal/service` → `internal/repository/postgres`, models in `internal/domain` | [Design decisions](#design-decisions) |
| Go best practices: interfaces, error wrapping, idiomatic style | ✅ | ports declared by consumers (`internal/service/ports.go`); errors wrapped with `%w` at each layer | `make lint`: golangci-lint (incl. errorlint, wrapcheck), 0 issues; SonarQube Cloud A ratings |
| Structured logging | ✅ | zap, JSON, with `request_id` / `trace_id` / `span_id` (`internal/logger`) | `make docker-logs` |
| Errors handled gracefully, meaningful messages, proper HTTP codes | ✅ | one mapping table, one JSON error shape (`internal/transport/httpapi/response.go`) | [Error codes](#error-codes); `TestErrorMapping`, `TestContract_ErrorsNeverLeakInternals` |
| Dockerfile + docker-compose.yml | ✅ | `Dockerfile` (multi-stage, distroless, non-root), `docker-compose.yml` | `make docker-run` |

**Clients should be able to**

| Requirement | ✓ | Endpoint | How to verify |
|---|---|---|---|
| Create, retrieve and update cargo status (`pending`, `in_transit`, `delivered`) | ✅ | `POST /cargoes`, `GET /cargoes[/{id}]`, `PATCH /cargoes/{id}/status` | `make demo` steps 2–10; `TestStatus_HappyPathThroughTheWholeLifecycle`, `TestStatus_TransitionMatrix` |
| Retrieve vessel details: name, capacity, current location | ✅ | `GET /vessels/{id}` (also `load_kg`, `available_capacity_kg`) | `TestVessel_CreateReturnsResourceAndLocation` |
| Complete movement history for each cargo | ✅ | `GET /cargoes/{id}/tracking` | `make demo` step 11; `TestTracking_FullHistoryIsCompleteAndChronological` |

**Data model**

| Requirement | ✓ | Where | How to verify |
|---|---|---|---|
| PostgreSQL | ✅ | PostgreSQL 16, `pgx/v5` | `docker-compose.yml` |
| Relationships between cargoes, vessels and tracking | ✅ | foreign keys `cargoes.vessel_id`, `tracking_entries.cargo_id`, `cargo_events.cargo_id` (`ON DELETE RESTRICT`) | `TestSchema_ReferencedRowsCannotBeDeleted` |
| Data integrity, indexing, normalization | ✅ | `CHECK` / `NOT NULL` / `UNIQUE` constraints, a status enum, indexes matching every query | `TestSchema_CargoConstraints`, `TestSchema_ExpectedIndexesExist` |
| DB migrations | ✅ | raw SQL with golang-migrate, embedded in the binary, applied at startup (`migrations/`) | `TestMigrations_RoundTripDownAndUp` (up → down → up on a fresh database) |

**Part 1: API development**

| Requirement | ✓ | Where | How to verify |
|---|---|---|---|
| RESTful API | ✅ | resources under `/api/v1`, correct verbs and codes, `Location` / `ETag` headers | [Endpoints](#endpoints); Swagger UI at `/docs` |
| Endpoints support the required operations | ✅ | create / retrieve / update status / history. There is **no DELETE**: history must never be erased (auditability), so `DELETE` returns 405 | `TestContract_UnknownRoutesAndMethods` |
| Input validation | ✅ | field-level validation in the domain; strict JSON decoding (unknown fields, trailing data, size, content type) | `TestCargo_Validation`, `TestContract_MalformedBodiesOnEveryWriteEndpoint` (12 cases × 5 endpoints) |
| Consistent JSON responses | ✅ | `{"data": …}` / `{"error": {code, message, details}}`, checked against the [OpenAPI contract](api/openapi.yaml) on **every** response in the black-box suite | `TestContract_EveryResponseIsJSONWithAConsistentEnvelope` |

**Part 2: Database & persistence**

| Requirement | ✓ | Where | How to verify |
|---|---|---|---|
| Repository / data access layer | ✅ | `internal/repository/postgres`, with a Unit of Work (`TxManager.WithinTx`) | integration tests in that package |
| A cargo belongs to a vessel | ✅ | `cargoes.vessel_id` FK, capacity enforced under a row lock | `TestConcurrentCargoCreation_NeverExceedsCapacity` |
| A cargo has many tracking entries | ✅ | `tracking_entries.cargo_id` FK | `TestTrackingRepository_AppendAndListInOrder` |
| Auditability: movements fully logged, never overwritten | ✅ | every status change, movement and vessel move appends an entry; **database triggers reject UPDATE / DELETE / TRUNCATE** on the history tables | `TestSchema_AuditTablesAreAppendOnly`, `TestTracking_HistoryIsNeverRewritten` |

**Part 3: Testing**

| Requirement | ✓ | Where | How to verify |
|---|---|---|---|
| Unit tests: services, handlers with mocks | ✅ | services with in-memory fakes (`internal/service/fakes_test.go`); handlers with fake services (`internal/transport/httpapi/fakes_test.go`) | `make test` |
| Integration tests: repositories against a test DB | ✅ | `internal/repository/postgres/*_integration_test.go` against real Postgres | `make test-integration` |
| docker-compose test environment | ✅ | `docker-compose.test.yml`: disposable Postgres + RabbitMQ, plus a `tests` service that runs the suite in Docker | `make test-docker` |
| 80%+ coverage | ✅ | **94%** combined (`make coverage`, fails under 80%); **95%** on [SonarQube Cloud](https://sonarcloud.io/project/overview?id=drzbraz_deus.ai) | `make coverage`, coverage badge |

**Part 4: Dockerization**

| Requirement | ✓ | Where | How to verify |
|---|---|---|---|
| Dockerfile that builds and runs the API | ✅ | multi-stage build → distroless, non-root image | `docker build .` |
| docker-compose.yml with API + Postgres | ✅ | API, Postgres, RabbitMQ (+ OpenTelemetry collector, Jaeger, Prometheus, Grafana) | `make docker-run` |
| API at http://localhost:8080 | ✅ | port 8080 | `curl localhost:8080/healthz` |
| `make docker-run` | ✅ | `Makefile` (waits until the API is up) | CI's "Docker stack" job runs it on every change |

**Deliverables**

| Deliverable | ✓ | Where |
|---|---|---|
| Working Go backend API | ✅ | CI builds, tests and boots it on every change |
| Dockerized setup | ✅ | `Dockerfile`, `docker-compose.yml` |
| Tests (unit + integration) | ✅ | 267 test functions (plus table-driven cases): next to the code, and black-box suites in [`tests/`](tests/README.md) |
| README: setup (local & Docker), API docs with examples, design decisions | ✅ | [Quick start](#quick-start-docker), [Local development](#local-development), [API documentation](#api-documentation), [Design decisions](#design-decisions) |

### Bonus: asynchronous event processing (delivered)

| Bonus requirement | ✓ | Where |
|---|---|---|
| **Event producer:** on status change, emit `cargo_id`, `old_status`, `new_status`, `timestamp` | ✅ | `CargoService.UpdateStatus` writes the event to an **outbox table in the same transaction** as the change; a relay publishes it with broker confirms (`internal/events/relay.go`). The payload also carries an `event_id` for deduplication. |
| **Queue:** RabbitMQ, using the provided setup | ✅ | the provided image and credentials (`deus_user` / `deus_pass`), pinned to 4.4; quorum queue `cargo.status_changed` with a dead-letter queue |
| **Consumer:** background worker that listens for events | ✅ | `internal/events/rabbitmq/consumer.go`, runs in the API process (parallel per cargo, ordered within a cargo) |
| **…logs and stores them in `cargo_events`** | ✅ | logs `cargo status event stored`, then inserts idempotently into `cargo_events` (`internal/events/handler.go`) |

**See it working:**
1. `make demo`: step 12 shows the two stored events for the cargo it moved (`GET /api/v1/cargoes/{id}/events`).
2. The consumer's logs: `docker compose logs api | grep "cargo status event stored"`.
3. The table itself: `docker compose exec postgres psql -U deus -d logistics -c "SELECT * FROM cargo_events ORDER BY occurred_at DESC LIMIT 5;"`
4. The queue: RabbitMQ UI at http://localhost:15672 (`deus_user` / `deus_pass`) → Queues → `cargo.status_changed`.
5. One trace from HTTP request to stored event: Jaeger at http://localhost:16686 → service `logistics-api` → `PATCH /api/v1/cargoes/{cargoID}/status`.

**Proven by tests:** `TestEvents_WireFormatOnTheBroker` (exact message contract), `TestEndToEnd_CargoLifecycleProducesEvents`, `TestEndToEnd_BrokerOutageLosesNoEvents` (RabbitMQ down → nothing lost), `TestDuplicateMessagesAreStoredOnce`, `TestPoisonMessagesAreDeadLetteredAndDoNotBlockTheQueue`. Details in [Event pipeline](#event-pipeline-bonus).

### Evaluation criteria → evidence

| Criterion | Evidence |
|---|---|
| 1. Correctness & completeness | checklist above; `make demo`; CI green on every change |
| 2. API design | REST resources, one envelope, documented error codes, OpenAPI contract enforced in tests, `ETag` / `If-Match`, `Idempotency-Key` |
| 3. Code quality | ports & adapters, constructor injection, no globals; golangci-lint 0 issues; SonarQube Cloud **A / A / A** (security / reliability / maintainability), 0 bugs, 0 smells, 0% duplication |
| 4. Database modeling | FKs, constraints, enum, query-matched indexes, append-only triggers, reversible migrations |
| 5. Error handling & logging | specific 4xx for every bad input, 503 + `Retry-After` when dependencies fail, internals never leaked; structured logs correlated with traces |
| 6. Testing | 94–95% coverage; unit, integration, black-box e2e, fault-injection (`tests/resilience`) and schema suites; race detector everywhere |
| 7. Docker expertise | multi-stage distroless image, health-checked compose, pinned images, test compose, CI boots the real stack |
| 8. Bonus | full event pipeline with transactional outbox, idempotent consumer and dead-letter queue (above) |

### Beyond the brief
- **Reliability:** transactional outbox (no lost events), idempotency keys and optimistic locking (safe retries and races), graceful shutdown, 503 instead of 500 when dependencies fail.
- **Observability:** OpenTelemetry traces and per-endpoint latency metrics, a ready-made Grafana dashboard.
- **Quality pipeline:** CI (lint, vulnerability scan, every suite, Docker stack test), SonarQube Cloud quality gate, SHA-pinned actions kept current by Dependabot.

### A 15-minute code tour
1. `internal/domain/status.go`: the forward-only state machine, as one table
2. `internal/service/cargo_service.go`: `UpdateStatus` locks the cargo, then commits the status, history and event atomically
3. `internal/repository/postgres/db.go`: Unit of Work (transaction carried in `ctx`) and DB error translation
4. `internal/events/relay.go` + `handler.go`: outbox → RabbitMQ → idempotent consumer
5. `internal/transport/httpapi/idempotency.go`: concurrent duplicate requests serialised by the database

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
9. [Known limitations and next steps](#known-limitations-and-next-steps)
10. [Project layout](#project-layout)

---

## Quick start (Docker)

Requirements: Docker with Compose v2 and `make`.

> Local (non-Docker) runs need **Go 1.26+** (pinned to 1.26.10 in `go.mod` for its security fixes; the challenge asks for 1.21+). An older Go downloads the right toolchain automatically unless `GOTOOLCHAIN=local` is set. The Docker targets need no local Go at all.

```bash
make docker-run      # builds the API image and starts the whole stack
```

| Service | URL |
|---|---|
| **API** | http://localhost:8080 |
| **Swagger UI** (interactive API docs) | http://localhost:8080/docs |
| OpenAPI 3 contract | http://localhost:8080/openapi.yaml ([`api/openapi.yaml`](api/openapi.yaml)) |
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

(The snippet uses `jq`. `make demo` runs a longer version of this tour with no extra tools.)

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
| `CONSUMER_WORKERS` | `4` | events stored in parallel (one cargo's events always stay in order) |
| `OUTBOX_POLL_INTERVAL` / `OUTBOX_BATCH_SIZE` | `500ms` / `100` | relay tuning |
| `IDEMPOTENCY_TTL` | `24h` | how long `Idempotency-Key` responses are kept |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | *(empty = telemetry off)* | e.g. `http://localhost:4317` |
| `OTEL_SERVICE_NAME` | `logistics-api` | service name in traces and metrics |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

---

## API documentation

The machine-readable contract is [`api/openapi.yaml`](api/openapi.yaml). It is embedded in the binary and served at `/openapi.yaml`, and Swagger UI at `/docs` lets you try every endpoint. Two checks keep it honest:
- a unit test fails if a route exists without being documented, or the reverse;
- every response received by the black-box suites is validated against it (status code documented, exact schema, no extra fields).

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

Cargo travels with its vessel. In the same transaction, every `in_transit` cargo on board gets a tracking entry at the new location (note `moved with vessel`). Pending cargo has not left port and delivered cargo has been unloaded, so neither moves. Existing history is never changed.

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
            "weight_kg": 1200, "status": "pending", "vessel_id": "01a18920-e2a7-...",
            "current_location": "Rotterdam", "version": 1,
            "created_at": "...", "updated_at": "..." } }
```

- New cargo always starts as `pending`. Clients cannot choose the initial status.
- Creation records the first tracking entry at the vessel's current location.
- `current_location` is where the cargo was last recorded (its latest tracking entry). It is derived on read, never stored, so it cannot drift from the history.
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
- **Consumer.** Stores events on `CONSUMER_WORKERS` goroutines. The publisher puts the cargo id in an AMQP header, and the consumer always sends a given cargo to the same worker, so **different cargoes are processed in parallel while each cargo's events keep their order** within a consumer instance. (With several API replicas consuming, RabbitMQ spreads messages across them; storage stays correct because inserts are idempotent and reads sort by `occurred_at`. Strict cross-replica ordering would need the queue's single-active-consumer mode.) Uses manual acks; the prefetch window is 5 messages per worker, for backpressure. It stores each event **idempotently** (`cargo_events.id` = `event_id`), so redeliveries are harmless.
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
make test-docker        # every suite, run entirely inside Docker (no local Go needed)
make test-e2e           # black-box suites in tests/ (e2e, resilience, schema)
make test-e2e-stack     # black-box e2e suite against the running `make docker-run` stack
make test-all           # every suite with -race
make coverage           # combined coverage report (coverage.html); fails under 80%
make lint               # golangci-lint: vet, staticcheck, gosec, errorlint, wrapcheck, gofmt/goimports, ...
make vuln               # govulncheck: known vulnerabilities in dependencies and the Go toolchain
```

Besides the unit and integration tests next to the code, [`tests/`](tests/README.md) holds a separate **black-box suite** (108 test functions, many of them table-driven). It covers every API flow and edge case over HTTP, plus fault injection (database and broker outages, lock timeouts, poison and duplicate messages, crashes, shutdown under load) and schema integrity.

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
| Consumer runs cargoes in parallel, never reorders one cargo | `TestConsumer_ParallelAcrossCargoesOrderedWithinCargo`, `TestShardedPool_PreservesOrderPerKey` |
| Readiness checks run concurrently (probe time = slowest check) | `TestReadiness_ChecksRunConcurrently` |
| A slow cargo never starves the other consumer workers | `TestShardedPool_BusyWorkerDoesNotStarveOthers` |
| A vessel move racing a delivery never records a movement after it | `TestConcurrentVesselMoveAndDelivery_NoMovementAfterDelivery`, `TestConcurrency_VesselMovesRacingDeliveryNeverFollowIt` |
| Latency metric uses route patterns, not ids | `TestLatencyMetricUsesRoutePattern` |

### Continuous integration

`.github/workflows/ci.yml` runs on every push to `main` and on every pull request:

| Job | What it does |
|---|---|
| Lint | golangci-lint v2 with the project config (`.golangci.yml`) |
| Vulnerability scan | `govulncheck`: fails if our code reaches a known vulnerable function |
| Tests | Postgres and RabbitMQ as service containers; every suite (unit, integration, e2e, resilience, schema) with `-race`; **fails under 80% coverage**; coverage report uploaded as an artifact |
| Docker stack + black-box e2e | builds the image, boots the full `docker-compose` stack, runs the e2e and schema suites against it over HTTP |
| SonarQube Cloud | imports the coverage and golangci-lint reports and applies the quality gate; results are public on the [dashboard](https://sonarcloud.io/project/overview?id=drzbraz_deus.ai) and posted on each pull request |

Tool versions are pinned in the `Makefile` and the workflow, and Go itself comes from the `toolchain` line in `go.mod`, so local runs and CI behave the same. Every GitHub Action is pinned to a full commit SHA; Dependabot keeps those pins and the Go modules current.

**SonarQube Cloud setup** (one-time, for forks of this repository): import the repository on sonarcloud.io, turn off *Automatic Analysis* (CI-based analysis is what brings in coverage), and add the project token as the `SONAR_TOKEN` repository secret. The keys in `sonar-project.properties` must match the project. Without the secret the job skips itself instead of failing.

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

**Credentials.** Every password in the repository (`deus` / `deus`, `deus_user` / `deus_pass`) is a fixed, non-secret default for disposable local and CI containers; the RabbitMQ ones come from the challenge's own helper. The application reads every connection string from environment variables (`DATABASE_URL`, `RABBITMQ_URL`), so a real deployment injects them from a secrets manager.

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

## Known limitations and next steps

- **Relay ordering is global.** The outbox relay publishes in creation order and stops a batch at the first failure. That is correct when RabbitMQ is down (everything waits and nothing is lost), but a failure specific to *one* message would hold back every cargo's events until it succeeds. Next step: publish per cargo (only the oldest pending event of each cargo is eligible), back off per message, and park a message after N attempts behind an `outbox.parked` metric and alert.
- **Ordering across replicas.** The consumer keeps each cargo's events in order within one instance. With several API replicas, RabbitMQ spreads messages between them. Storage stays correct (idempotent inserts, reads sorted by `occurred_at`); strict cross-replica order would need the queue's single-active-consumer mode.
- **Offset pagination** is simple but drifts on large, changing tables; keyset pagination over `(created_at, id)` would fix that, and the index already exists.
- **No authentication**, since the challenge does not ask for it; an API key or JWT middleware would slot into the existing middleware chain.
- **Swagger UI loads its assets from a CDN** (unpkg), so `/docs` needs internet access. `/openapi.yaml` is served from the binary.
- **Deployment shape.** The relay and consumer run inside the API process for simplicity; `RELAY_ENABLED` / `CONSUMER_ENABLED` already allow running them as separate deployments.

## Project layout

```
.github/workflows/ci.yml     CI: lint, vulnerability scan, every test suite, Docker stack test, SonarQube Cloud
.github/dependabot.yml       weekly updates for the pinned actions and Go modules
.golangci.yml                linter configuration
sonar-project.properties     SonarQube Cloud analysis settings
api/openapi.yaml             OpenAPI 3 contract (embedded; served at /openapi.yaml and /docs)
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
scripts/demo.sh              guided API tour (`make demo`)
tests/                       black-box suites: e2e, resilience, schema (+ testkit harness)
```
