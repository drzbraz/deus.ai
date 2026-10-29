# Black-box test suites

These suites live outside the production code and treat the service as a black box. They talk to the API **only over HTTP**, and verify side effects directly in **Postgres** and **RabbitMQ**. The unit and integration tests next to the code (`internal/**/_test.go`) check components in isolation; these check the behaviour a client sees.

```bash
make test-e2e         # all suites against an in-process API + disposable Postgres/RabbitMQ
make test-e2e-stack   # e2e + schema suites against the running `make docker-run` stack
make test-all         # everything (unit, integration, e2e, resilience, schema) with -race
```

| Suite | What it proves |
|---|---|
| `e2e/` | every endpoint, flow and edge case of the public API |
| `resilience/` | behaviour when dependencies fail (in-process only; skipped against an external stack) |
| `schema/` | migrations round-trip, and integrity rules hold even for writes that bypass the API |
| `testkit/` | shared harness: app bootstrap, HTTP client, fixtures, TCP fault-injection proxy, AMQP/DB helpers |

The suites build with the `e2e` tag. Point them elsewhere with `API_BASE_URL`, `E2E_DATABASE_URL` and `E2E_RABBITMQ_URL`. Tests isolate themselves with fresh data (unique vessels and cargoes), so they never truncate tables (the audit tables forbid it anyway) and can run against a shared environment.

## Catalogue

**Vessels** (`e2e/vessels_test.go`)
- creation, `Location` header, trimming, case-sensitive unique names, duplicate (also after trimming) → 409
- field validation table, all errors reported at once, boundary values (200 chars, 0.01 kg, 1e9 kg, unicode)
- 404 / invalid ids, stable and disjoint pagination, `meta`, `[]` for empty pages, bad pagination → 400/422
- moving a vessel: timestamps, validation, immutability of name, history not rewritten, load excludes delivered cargo

**Cargoes** (`e2e/cargoes_test.go`)
- created `pending` with version 1 and `ETag`, first tracking entry at the vessel's location, no event on creation
- clients cannot set `status`, `version` or `id`; full validation table; wrong JSON types
- capacity: exact fit, 0.01 kg over, in-transit still counts, delivery frees space, single cargo heavier than vessel
- filters (`status`, `vessel_id`, combined, pagination), bad filters, unknown query params ignored

**Status lifecycle** (`e2e/status_lifecycle_test.go`)
- happy path with versions, ETags, locations, notes and both events
- the complete 3×3 transition matrix over HTTP; each rejected move leaves status, version, history and events untouched
- delivered is terminal; invalid status values; location defaults to the vessel position; invalid location/note rolls back
- `If-Match`: match, stale → 412 with no side effects, precedence over transition errors, accepted formats, malformed headers

**Tracking and events** (`e2e/tracking_events_test.go`)
- movements keep status and version; complete chronological history; history never rewritten; delivered accepts no movement
- 404 vs 400 for unknown/invalid cargo; `[]` when there are no events; event times equal the change times
- wire contract on the broker: exact JSON fields, content type, persistent delivery, `message_id` = `event_id`

**Idempotency** (`e2e/idempotency_test.go`)
- byte-identical replay with headers; replay is the original answer even after the resource changed
- different keys → different resources; no key → duplicates (documented); key reused with another body or path → 422
- keys scoped per operation; 4xx and 409 replayed; status-change and movement retries do not duplicate; key length; empty key; GET ignores keys

**Concurrency** (`e2e/concurrency_test.go`)
- 20 racing identical transitions → 1 winner, 1 tracking entry, 1 outbox row, 1 event
- mixed racing transitions keep history a valid prefix of the lifecycle
- 25 concurrent 10 kg cargoes on a 100 kg vessel → exactly 10 accepted; mixed weights never exceed capacity
- movements racing delivery never land after it; `If-Match` race → 1×200 + 14×412; vessel-name race → 1×201
- 25 simultaneous POSTs with one key → 1 cargo, identical responses; 20 simultaneous status PATCHes with one key → 1 event
- same key with two different bodies at once → one cargo, twins replayed, others 422
- readers never see a torn state (status and version always agree); independent cargoes do not block each other

**HTTP contract** (`e2e/http_contract_test.go`)
- 12 malformed-body cases × 5 write endpoints (empty, truncated, array, string, two objects, trailing data, unknown field, missing/wrong content type, form-encoded, >1 MiB)
- JSON content-type variants; invalid path ids; unknown routes; no DELETE/PUT anywhere (append-only); trailing slash
- consistent envelopes on every response; UTC timestamps; no internals leaked in errors; SQL injection stored as plain text; health probes

**Resilience** (`resilience/resilience_test.go`)
- RabbitMQ down at startup: writes succeed, readiness `degraded`, events wait in the outbox and arrive in order, exactly once, after recovery
- RabbitMQ connection cut mid-run, then restored: nothing lost, readiness recovers
- the same message delivered 5 times → stored once; poison messages → DLQ and the queue keeps flowing
- relay crash between broker confirm and commit → republished, still exactly one stored event
- Postgres cut mid-run → 503 + `Retry-After` on reads and writes, readiness 503, liveness 200; the pool recovers by itself
- Postgres unreachable at startup → fails fast after N attempts
- row locked by another transaction → 503 within the request deadline; the same Idempotency-Key succeeds on retry
- consumer during a DB outage does not dead-letter healthy events and stores them after recovery
- graceful shutdown under load: no 5xx, and every 201 was persisted (and nothing else)
- OTel collector down: requests unaffected; W3C `traceparent` travels inside the AMQP message

**Schema** (`schema/schema_test.go`), on a freshly created database
- migrations up → down (removes everything) → up again → idempotent
- expected indexes exist; vessel and cargo CHECK / NOT NULL / UNIQUE / FK / enum constraints; defaults
- referenced vessels and cargoes cannot be deleted; tracking and events reject UPDATE / DELETE / TRUNCATE
- unique event ids, events must reference a real cargo, idempotency keys unique per scope
