package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad_Defaults(t *testing.T) {
	t.Setenv("HTTP_ADDR", "")

	cfg, err := Load()

	require.NoError(t, err)
	assert.Equal(t, ":8080", cfg.HTTPAddr)
	assert.Equal(t, 5*time.Second, cfg.RequestTimeout)
	assert.True(t, cfg.RelayEnabled)
	assert.Equal(t, 24*time.Hour, cfg.IdempotencyTTL)
}

func TestLoad_Overrides(t *testing.T) {
	t.Setenv("HTTP_ADDR", ":9090")
	t.Setenv("DB_MAX_CONNS", "7")
	t.Setenv("CONSUMER_ENABLED", "false")
	t.Setenv("OUTBOX_POLL_INTERVAL", "2s")

	cfg, err := Load()

	require.NoError(t, err)
	assert.Equal(t, ":9090", cfg.HTTPAddr)
	assert.Equal(t, int32(7), cfg.DBMaxConns)
	assert.False(t, cfg.ConsumerEnabled)
	assert.Equal(t, 2*time.Second, cfg.OutboxInterval)
}

func TestLoad_ReportsEveryInvalidVariable(t *testing.T) {
	t.Setenv("DB_MAX_CONNS", "many")
	t.Setenv("RELAY_ENABLED", "maybe")
	t.Setenv("REQUEST_TIMEOUT", "5")
	t.Setenv("DB_STATEMENT_TIMEOUT", "soon")

	_, err := Load()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "DB_MAX_CONNS")
	assert.Contains(t, err.Error(), "RELAY_ENABLED")
	assert.Contains(t, err.Error(), "REQUEST_TIMEOUT")
}

func TestLoad_RejectsPoolSizeOutOfInt32Range(t *testing.T) {
	t.Setenv("DB_MAX_CONNS", "3000000000")

	_, err := Load()

	assert.ErrorContains(t, err, "DB_MAX_CONNS")
}
