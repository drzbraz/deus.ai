// Package config loads application settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds every runtime setting. Defaults target local development.
type Config struct {
	HTTPAddr        string
	RequestTimeout  time.Duration
	ShutdownTimeout time.Duration
	LogLevel        string

	DatabaseURL        string
	DBMaxConns         int32
	DBStatementTimeout time.Duration
	DBConnectAttempts  int

	RabbitMQURL     string
	RelayEnabled    bool
	ConsumerEnabled bool
	OutboxInterval  time.Duration
	OutboxBatchSize int

	IdempotencyTTL time.Duration

	OTelEndpoint    string
	OTelServiceName string
}

// Load reads the configuration, collecting every invalid variable into one error.
func Load() (Config, error) {
	r := reader{lookup: os.LookupEnv}

	cfg := Config{
		HTTPAddr:        r.string("HTTP_ADDR", ":8080"),
		RequestTimeout:  r.duration("REQUEST_TIMEOUT", 5*time.Second),
		ShutdownTimeout: r.duration("SHUTDOWN_TIMEOUT", 15*time.Second),
		LogLevel:        r.string("LOG_LEVEL", "info"),

		DatabaseURL:        r.string("DATABASE_URL", "postgres://deus:deus@localhost:5433/logistics?sslmode=disable"),
		DBMaxConns:         r.int32("DB_MAX_CONNS", 20),
		DBStatementTimeout: r.duration("DB_STATEMENT_TIMEOUT", 5*time.Second),
		DBConnectAttempts:  r.int("DB_CONNECT_ATTEMPTS", 10),

		RabbitMQURL:     r.string("RABBITMQ_URL", "amqp://deus_user:deus_pass@localhost:5672/"),
		RelayEnabled:    r.bool("RELAY_ENABLED", true),
		ConsumerEnabled: r.bool("CONSUMER_ENABLED", true),
		OutboxInterval:  r.duration("OUTBOX_POLL_INTERVAL", 500*time.Millisecond),
		OutboxBatchSize: r.int("OUTBOX_BATCH_SIZE", 100),

		IdempotencyTTL: r.duration("IDEMPOTENCY_TTL", 24*time.Hour),

		OTelEndpoint:    r.string("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		OTelServiceName: r.string("OTEL_SERVICE_NAME", "logistics-api"),
	}

	return cfg, r.err()
}

type reader struct {
	lookup func(string) (string, bool)
	errs   []error
}

func (r *reader) string(key, fallback string) string {
	if value, ok := r.lookup(key); ok && value != "" {
		return value
	}
	return fallback
}

func (r *reader) int(key string, fallback int) int {
	return parse(r, key, fallback, strconv.Atoi)
}

func (r *reader) int32(key string, fallback int32) int32 {
	return parse(r, key, fallback, func(raw string) (int32, error) {
		value, err := strconv.ParseInt(raw, 10, 32)
		return int32(value), err
	})
}

func (r *reader) bool(key string, fallback bool) bool {
	return parse(r, key, fallback, strconv.ParseBool)
}

func (r *reader) duration(key string, fallback time.Duration) time.Duration {
	return parse(r, key, fallback, time.ParseDuration)
}

func (r *reader) err() error {
	return errors.Join(r.errs...)
}

func parse[T any](r *reader, key string, fallback T, parseFn func(string) (T, error)) T {
	raw, ok := r.lookup(key)
	if !ok || raw == "" {
		return fallback
	}
	value, err := parseFn(raw)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("invalid %s=%q: %w", key, raw, err))
		return fallback
	}
	return value
}
