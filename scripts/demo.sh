#!/usr/bin/env bash
# Guided tour of the API against a running stack (`make docker-run`).
# Every step checks its HTTP status, so the tour doubles as a smoke test.
#
#   make demo                      # http://localhost:8080
#   BASE_URL=http://host:8080 make demo
set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:8080}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
LAST_STATUS="" LAST_BODY=""

if [[ -t 1 ]]; then B=$'\033[1m' C=$'\033[1;36m' G=$'\033[32m' D=$'\033[2m' R=$'\033[0m'; else B="" C="" G="" D="" R=""; fi

step() { printf '\n%s== %s%s\n' "$C" "$*" "$R"; }
say()  { printf '%s%s%s\n' "$D" "$*" "$R"; }

pretty() {
  if command -v jq >/dev/null 2>&1; then jq . 2>/dev/null || cat
  elif command -v python3 >/dev/null 2>&1; then python3 -m json.tool 2>/dev/null || cat
  else cat; echo
  fi
}

# request METHOD PATH BODY [curl args...]   (BODY may be "")
request() {
  local method=$1 path=$2 body=$3
  shift 3
  local args=(-s -X "$method" -D "$TMP/headers" -o "$TMP/body" -w '%{http_code}')
  if [[ -n "$body" ]]; then args+=(-H 'Content-Type: application/json' -d "$body"); fi

  LAST_STATUS=$(curl "${args[@]}" "$@" "$BASE_URL$path")
  LAST_BODY=$(cat "$TMP/body")
  printf '%s%s %s%s -> %s\n' "$B" "$method" "$path" "$R" "$LAST_STATUS"
  grep -iE '^(etag|location|idempotent-replayed|retry-after):' "$TMP/headers" | sed 's/^/  /' || true
  printf '%s' "$LAST_BODY" | pretty
}

expect() {
  if [[ "$LAST_STATUS" != "$1" ]]; then
    printf 'expected HTTP %s, got %s\n' "$1" "$LAST_STATUS" >&2
    exit 1
  fi
  printf '%s✔ %s%s\n' "$G" "$2" "$R"
}

# field NAME: first "NAME":"value" in the last response body.
field() { printf '%s' "$LAST_BODY" | grep -o "\"$1\":\"[^\"]*\"" | head -1 | cut -d'"' -f4; }

step "0. Waiting for the API at $BASE_URL"
for _ in $(seq 1 60); do
  curl -sf "$BASE_URL/readyz" >/dev/null 2>&1 && break
  sleep 1
done
request GET /readyz ""
expect 200 "API is ready"

step "1. Create a vessel"
request POST /api/v1/vessels "{\"name\":\"Demo Vessel $(date +%s)-$RANDOM\",\"capacity_kg\":20000,\"current_location\":\"Rotterdam\"}"
expect 201 "vessel created"
VESSEL=$(field id)

step "2. Create a cargo with an Idempotency-Key"
KEY="demo-$(date +%s)-$RANDOM"
CARGO_BODY="{\"name\":\"Coffee beans\",\"weight_kg\":1200,\"vessel_id\":\"$VESSEL\"}"
request POST /api/v1/cargoes "$CARGO_BODY" -H "Idempotency-Key: $KEY"
expect 201 "cargo created as pending, located where its vessel is"
CARGO=$(field id)

step "3. The client retries the same request (e.g. after a timeout)"
request POST /api/v1/cargoes "$CARGO_BODY" -H "Idempotency-Key: $KEY"
expect 201 "same response replayed (Idempotent-Replayed: true), no duplicate cargo"
[[ "$(field id)" == "$CARGO" ]] || { echo "replay returned a different cargo" >&2; exit 1; }

step "4. A cargo that does not fit on the vessel"
request POST /api/v1/cargoes "{\"name\":\"Steel\",\"weight_kg\":19000,\"vessel_id\":\"$VESSEL\"}"
expect 409 "capacity is enforced (1200 kg + 19000 kg > 20000 kg)"

step "5. Invalid input is reported field by field"
request POST /api/v1/cargoes '{"name":"","weight_kg":-1}'
expect 422 "validation errors"

step "6. Ship the cargo, using its ETag for optimistic concurrency"
request PATCH "/api/v1/cargoes/$CARGO/status" '{"status":"in_transit","location":"Rotterdam port"}' -H 'If-Match: "1"'
expect 200 "pending -> in_transit (ETag is now \"2\")"

step "7. A client with a stale ETag"
request PATCH "/api/v1/cargoes/$CARGO/status" '{"status":"delivered"}' -H 'If-Match: "1"'
expect 412 "stale version rejected, nothing changed"

step "8. Status can only move forward"
request PATCH "/api/v1/cargoes/$CARGO/status" '{"status":"pending"}'
expect 409 "in_transit -> pending is not allowed, and no event is emitted"

step "9. The vessel sails: cargo on board moves with it"
request PATCH "/api/v1/vessels/$VESSEL/location" '{"current_location":"Suez Canal"}'
expect 200 "vessel moved"
request PATCH "/api/v1/vessels/$VESSEL/location" '{"current_location":"Singapore"}'
expect 200 "vessel moved again"
request GET "/api/v1/cargoes/$CARGO" ""
expect 200 "current_location follows the vessel"

step "10. Deliver"
request PATCH "/api/v1/cargoes/$CARGO/status" '{"status":"delivered","location":"Singapore terminal"}'
expect 200 "in_transit -> delivered"
request POST "/api/v1/cargoes/$CARGO/tracking" '{"location":"somewhere else"}'
expect 409 "a delivered cargo accepts no more movements"

step "11. Complete, append-only movement history"
request GET "/api/v1/cargoes/$CARGO/tracking" ""
expect 200 "history: registered, shipped, two vessel moves, delivered"

step "12. Events published to RabbitMQ and stored by the consumer"
for _ in $(seq 1 30); do
  curl -s "$BASE_URL/api/v1/cargoes/$CARGO/events" | grep -q '"new_status":"delivered"' && break
  sleep 0.5
done
request GET "/api/v1/cargoes/$CARGO/events" ""
expect 200 "two events: pending->in_transit and in_transit->delivered"

step "Done. Explore further:"
say "  Swagger UI   $BASE_URL/docs"
say "  Grafana      http://localhost:3000  (dashboard 'Logistics API': latency per endpoint)"
say "  Jaeger       http://localhost:16686 (service 'logistics-api': one trace from HTTP to consumer)"
say "  RabbitMQ     http://localhost:15672 (deus_user / deus_pass)"
