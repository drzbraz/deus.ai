package domain_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drzbraz/deus.ai/internal/domain"
)

func TestStatus_TransitionMatrix(t *testing.T) {
	tests := []struct {
		from, to domain.Status
		allowed  bool
	}{
		{domain.StatusPending, domain.StatusPending, false},
		{domain.StatusPending, domain.StatusInTransit, true},
		{domain.StatusPending, domain.StatusDelivered, false}, // cannot skip a step
		{domain.StatusInTransit, domain.StatusPending, false}, // cannot go back
		{domain.StatusInTransit, domain.StatusInTransit, false},
		{domain.StatusInTransit, domain.StatusDelivered, true},
		{domain.StatusDelivered, domain.StatusPending, false},
		{domain.StatusDelivered, domain.StatusInTransit, false},
		{domain.StatusDelivered, domain.StatusDelivered, false},
	}

	for _, tt := range tests {
		t.Run(string(tt.from)+"->"+string(tt.to), func(t *testing.T) {
			assert.Equal(t, tt.allowed, tt.from.CanTransitionTo(tt.to))
		})
	}
}

func TestParseStatus(t *testing.T) {
	for _, raw := range []string{"pending", "in_transit", "delivered"} {
		status, err := domain.ParseStatus(raw)
		require.NoError(t, err)
		assert.Equal(t, domain.Status(raw), status)
	}

	for _, raw := range []string{"", "PENDING", "lost", " pending"} {
		_, err := domain.ParseStatus(raw)
		assert.ErrorIs(t, err, domain.ErrValidation, "input %q", raw)
	}
}

func TestStatus_IsFinal(t *testing.T) {
	assert.False(t, domain.StatusPending.IsFinal())
	assert.False(t, domain.StatusInTransit.IsFinal())
	assert.True(t, domain.StatusDelivered.IsFinal())
}
