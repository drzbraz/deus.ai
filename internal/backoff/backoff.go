// Package backoff provides retry delay strategies shared by every component
// that talks to an external dependency (database, broker).
package backoff

import (
	"context"
	"math/rand/v2"
	"time"
)

// Policy decides how long to wait before the given retry attempt (starting at 1).
type Policy interface {
	Delay(attempt int) time.Duration
}

// Exponential doubles the delay on every attempt up to Max and applies "full
// jitter" so that many instances recovering at once do not retry in lockstep.
type Exponential struct {
	Initial time.Duration
	Max     time.Duration
}

// Default is a sensible policy for reconnecting to infrastructure.
func Default() Exponential {
	return Exponential{Initial: 200 * time.Millisecond, Max: 10 * time.Second}
}

func (e Exponential) Delay(attempt int) time.Duration {
	ceiling := e.ceiling(attempt)
	if ceiling <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(ceiling))) + 1 //nolint:gosec // jitter, not security: no need for crypto/rand
}

func (e Exponential) ceiling(attempt int) time.Duration {
	delay := e.Initial
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= e.Max {
			return e.Max
		}
	}
	return min(delay, e.Max)
}

// None never waits. It keeps retry tests fast and deterministic.
type None struct{}

func (None) Delay(int) time.Duration { return 0 }

// Sleep waits for d or until ctx is done, whichever happens first.
func Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Retry calls fn until it succeeds, maxAttempts is reached or ctx is done.
func Retry(ctx context.Context, policy Policy, maxAttempts int, fn func(ctx context.Context) error) error {
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err = fn(ctx); err == nil {
			return nil
		}
		if attempt == maxAttempts {
			break
		}
		if sleepErr := Sleep(ctx, policy.Delay(attempt)); sleepErr != nil {
			return sleepErr
		}
	}
	return err
}
