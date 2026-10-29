# Coverage gate for `make coverage` (unit + integration, all internal packages).
COVERAGE_MIN  := 80
TEST_COMPOSE  := docker compose -f docker-compose.test.yml
TEST_ENV      := TEST_DATABASE_URL=postgres://deus:deus@localhost:55432/logistics_test?sslmode=disable \
                 TEST_RABBITMQ_URL=amqp://deus_user:deus_pass@localhost:56720/

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

## --- Run --------------------------------------------------------------------

.PHONY: docker-run
docker-run: ## Build and start the full stack (API on http://localhost:8080)
	docker compose up -d --build --wait api
	docker compose up -d
	@echo "API         http://localhost:8080"
	@echo "Grafana     http://localhost:3000"
	@echo "Jaeger      http://localhost:16686"
	@echo "Prometheus  http://localhost:9090"
	@echo "RabbitMQ    http://localhost:15672 (deus_user / deus_pass)"

.PHONY: docker-down
docker-down: ## Stop the stack and delete its volumes
	docker compose down -v

.PHONY: docker-logs
docker-logs: ## Follow the API logs
	docker compose logs -f api

.PHONY: deps-up
deps-up: ## Start only Postgres and RabbitMQ (for `make run`)
	docker compose up -d --wait postgres rabbitmq

.PHONY: run
run: ## Run the API locally against `make deps-up`
	go run ./cmd/api

.PHONY: build
build: ## Build the binary into bin/api
	CGO_ENABLED=0 go build -o bin/api ./cmd/api

## --- Quality ----------------------------------------------------------------

.PHONY: lint
lint: ## gofmt check + go vet
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed on:"; gofmt -l .; exit 1)
	go vet -tags=integration ./...

.PHONY: test
test: ## Unit tests (no infrastructure needed)
	go test -race -count=1 ./...

.PHONY: test-infra-up
test-infra-up:
	$(TEST_COMPOSE) up -d --wait postgres-test rabbitmq-test

.PHONY: test-infra-down
test-infra-down:
	$(TEST_COMPOSE) down -v

.PHONY: test-integration
test-integration: test-infra-up ## Unit + integration tests against disposable Postgres/RabbitMQ
	$(TEST_ENV) go test -race -tags=integration -p 1 -count=1 ./... ; \
	status=$$?; $(MAKE) test-infra-down; exit $$status

.PHONY: test-docker
test-docker: ## Run the whole test suite inside Docker (no local Go needed)
	$(TEST_COMPOSE) run --rm --build tests ; \
	status=$$?; $(MAKE) test-infra-down; exit $$status

.PHONY: coverage
coverage: test-infra-up ## Combined coverage report; fails below COVERAGE_MIN%
	$(TEST_ENV) go test -tags=integration -p 1 -count=1 -coverpkg=./internal/... -coverprofile=coverage.out ./... ; \
	status=$$?; $(MAKE) test-infra-down; test $$status -eq 0 || exit $$status
	go tool cover -html=coverage.out -o coverage.html
	@total=$$(go tool cover -func=coverage.out | awk '/^total:/ {sub("%","",$$3); print $$3}'); \
	echo "total coverage: $$total% (minimum $(COVERAGE_MIN)%)"; \
	awk -v t=$$total -v m=$(COVERAGE_MIN) 'BEGIN { exit (t+0 < m+0) }'
