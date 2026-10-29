package backoff_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/drzbraz/deus.ai/internal/backoff"
)

func TestExponential_DelayStaysWithinGrowingCeiling(t *testing.T) {
	policy := backoff.Exponential{Initial: 100 * time.Millisecond, Max: time.Second}

	ceilings := map[int]time.Duration{1: 100 * time.Millisecond, 2: 200 * time.Millisecond, 3: 400 * time.Millisecond, 10: time.Second}
	for attempt, ceiling := range ceilings {
		for range 50 {
			delay := policy.Delay(attempt)
			assert.Positive(t, delay)
			assert.LessOrEqual(t, delay, ceiling, "attempt %d", attempt)
		}
	}
}

func TestExponential_ZeroInitialMeansNoDelay(t *testing.T) {
	assert.Zero(t, backoff.Exponential{}.Delay(3))
	assert.Zero(t, backoff.None{}.Delay(3))
	assert.Equal(t, 10*time.Second, backoff.Default().Max)
}

func TestRetry_SucceedsAfterTransientFailures(t *testing.T) {
	calls := 0
	err := backoff.Retry(context.Background(), backoff.None{}, 5, func(context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("not yet")
		}
		return nil
	})

	assert.NoError(t, err)
	assert.Equal(t, 3, calls)
}

func TestRetry_GivesUpAfterMaxAttempts(t *testing.T) {
	calls := 0
	boom := errors.New("down")
	err := backoff.Retry(context.Background(), backoff.None{}, 3, func(context.Context) error {
		calls++
		return boom
	})

	assert.ErrorIs(t, err, boom)
	assert.Equal(t, 3, calls)
}

func TestRetry_StopsWhenContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	policy := backoff.Exponential{Initial: time.Hour, Max: time.Hour}

	err := backoff.Retry(ctx, policy, 3, func(context.Context) error {
		cancel()
		return errors.New("down")
	})

	assert.ErrorIs(t, err, context.Canceled)
}

func TestSleep(t *testing.T) {
	assert.NoError(t, backoff.Sleep(context.Background(), time.Millisecond))
	assert.NoError(t, backoff.Sleep(context.Background(), 0))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.ErrorIs(t, backoff.Sleep(ctx, time.Hour), context.Canceled)
}
