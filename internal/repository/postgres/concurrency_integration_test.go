//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drzbraz/deus.ai/internal/domain"
	"github.com/drzbraz/deus.ai/internal/events"
	"github.com/drzbraz/deus.ai/internal/idempotency"
	"github.com/drzbraz/deus.ai/internal/repository/postgres"
	"github.com/drzbraz/deus.ai/internal/service"
)

type noopRecorder struct{}

func (noopRecorder) RecordTransition(context.Context, domain.Status, domain.Status) {}

// Regression: two clients racing to move the same cargo must produce exactly
// one transition, one tracking entry and one event.
func TestConcurrentStatusUpdates_ExactlyOneWins(t *testing.T) {
	ctx := context.Background()
	svc := service.NewCargoService(repos(), service.SystemClock, noopRecorder{})
	vessel := newVessel(t, 100)
	cargo, err := svc.Create(ctx, service.CreateCargoInput{Name: "race", WeightKg: 1, VesselID: vessel.ID})
	require.NoError(t, err)

	const workers = 10
	results := runConcurrently(workers, func() error {
		_, err := svc.UpdateStatus(ctx, service.UpdateStatusInput{CargoID: cargo.ID, Status: "in_transit"})
		return err
	})

	assert.Equal(t, 1, count(results, nil))
	assert.Equal(t, workers-1, countIs(results, domain.ErrInvalidTransition))

	history, err := postgres.NewTrackingRepository(testPool).ListByCargo(ctx, cargo.ID)
	require.NoError(t, err)
	assert.Len(t, history, 2, "creation + one transition")

	var outboxRows int
	require.NoError(t, testPool.QueryRow(ctx, `SELECT COUNT(*) FROM outbox WHERE aggregate_id = $1`, cargo.ID).Scan(&outboxRows))
	assert.Equal(t, 1, outboxRows)
}

// Regression: concurrent cargo creation must never overbook a vessel.
func TestConcurrentCargoCreation_NeverExceedsCapacity(t *testing.T) {
	ctx := context.Background()
	svc := service.NewCargoService(repos(), service.SystemClock, noopRecorder{})
	vessel := newVessel(t, 100)

	const workers = 12 // 12 x 10kg against 100kg capacity
	results := runConcurrently(workers, func() error {
		_, err := svc.Create(ctx, service.CreateCargoInput{Name: "box", WeightKg: 10, VesselID: vessel.ID})
		return err
	})

	assert.Equal(t, 10, count(results, nil))
	assert.Equal(t, 2, countIs(results, domain.ErrCapacityExceeded))

	got, err := postgres.NewVesselRepository(testPool).Get(ctx, vessel.ID)
	require.NoError(t, err)
	assert.Equal(t, 100.0, got.LoadKg)
}

// Regression: a movement cannot sneak in concurrently with the delivery.
func TestConcurrentMovementAndDelivery_HistoryEndsAtDelivery(t *testing.T) {
	ctx := context.Background()
	cargoes := service.NewCargoService(repos(), service.SystemClock, noopRecorder{})
	tracking := service.NewTrackingService(repos(), service.SystemClock)
	cargo, err := cargoes.Create(ctx, service.CreateCargoInput{Name: "x", WeightKg: 1, VesselID: newVessel(t, 10).ID})
	require.NoError(t, err)
	_, err = cargoes.UpdateStatus(ctx, service.UpdateStatusInput{CargoID: cargo.ID, Status: "in_transit"})
	require.NoError(t, err)

	runConcurrently(20, func() error {
		_, err := tracking.RecordMovement(ctx, service.RecordMovementInput{CargoID: cargo.ID, Location: "sea"})
		return err
	})
	_, err = cargoes.UpdateStatus(ctx, service.UpdateStatusInput{CargoID: cargo.ID, Status: "delivered"})
	require.NoError(t, err)
	_, err = tracking.RecordMovement(ctx, service.RecordMovementInput{CargoID: cargo.ID, Location: "late"})
	assert.ErrorIs(t, err, domain.ErrCargoDelivered)

	history, err := tracking.History(ctx, cargo.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusDelivered, history[len(history)-1].Status)
}

func TestOutbox_EnqueueFetchAndMark(t *testing.T) {
	ctx := context.Background()
	drainOutbox(t)
	outbox := postgres.NewOutboxRepository(testPool)
	tx := postgres.NewTxManager(testPool)
	cargo := newCargo(t, newVessel(t, 10).ID, 1)
	event := statusChanged(cargo.ID)

	require.NoError(t, outbox.Enqueue(ctx, event))
	pending, err := outbox.CountPending(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), pending)

	err = tx.WithinTx(ctx, func(ctx context.Context) error {
		messages, err := outbox.FetchUnpublished(ctx, 10)
		require.NoError(t, err)
		require.Len(t, messages, 1)
		assert.Equal(t, event.EventID, messages[0].ID)
		assert.Equal(t, events.RoutingKeyStatusChanged, messages[0].RoutingKey)

		decoded, err := events.DecodeStatusChanged(messages[0].Payload)
		require.NoError(t, err)
		assert.Equal(t, event.CargoID, decoded.CargoID)

		require.NoError(t, outbox.MarkFailed(ctx, event.EventID, strings.Repeat("x", 2000)))
		return outbox.MarkPublished(ctx, event.EventID, service.SystemClock())
	})
	require.NoError(t, err)

	pending, err = outbox.CountPending(ctx)
	require.NoError(t, err)
	assert.Zero(t, pending)

	var attempts int
	require.NoError(t, testPool.QueryRow(ctx, `SELECT attempts FROM outbox WHERE id = $1`, event.EventID).Scan(&attempts))
	assert.Equal(t, 1, attempts)
}

// Regression: two relays (e.g. two API replicas) never claim the same message.
func TestOutbox_SkipLockedSplitsWorkBetweenRelays(t *testing.T) {
	ctx := context.Background()
	drainOutbox(t)
	outbox := postgres.NewOutboxRepository(testPool)
	tx := postgres.NewTxManager(testPool)
	cargo := newCargo(t, newVessel(t, 10).ID, 1)
	for range 3 {
		require.NoError(t, outbox.Enqueue(ctx, statusChanged(cargo.ID)))
	}

	firstClaimed := make(chan []events.OutboxMessage)
	release := make(chan struct{})
	go func() {
		_ = tx.WithinTx(ctx, func(ctx context.Context) error {
			messages, _ := outbox.FetchUnpublished(ctx, 1)
			firstClaimed <- messages
			<-release
			return nil
		})
	}()
	first := <-firstClaimed

	var second []events.OutboxMessage
	err := tx.WithinTx(ctx, func(ctx context.Context) error {
		var err error
		second, err = outbox.FetchUnpublished(ctx, 10)
		return err
	})
	close(release)

	require.NoError(t, err)
	require.Len(t, first, 1)
	assert.Len(t, second, 2)
	for _, msg := range second {
		assert.NotEqual(t, first[0].ID, msg.ID)
	}
}

func TestIdempotency_ClaimCompleteAndReplay(t *testing.T) {
	ctx := context.Background()
	store := postgres.NewIdempotencyRepository(testPool)
	tx := postgres.NewTxManager(testPool)
	key := uuid.NewString()

	err := tx.WithinTx(ctx, func(ctx context.Context) error {
		_, claimed, err := store.Claim(ctx, "create_cargo", key, "hash-1")
		require.NoError(t, err)
		assert.True(t, claimed)
		return store.Complete(ctx, idempotencyRecord(key, 201))
	})
	require.NoError(t, err)

	err = tx.WithinTx(ctx, func(ctx context.Context) error {
		existing, claimed, err := store.Claim(ctx, "create_cargo", key, "hash-1")
		require.NoError(t, err)
		assert.False(t, claimed)
		assert.Equal(t, 201, existing.StatusCode)
		assert.Equal(t, "hash-1", existing.RequestHash)
		assert.Equal(t, `{"ok":true}`, string(existing.Body))
		assert.Equal(t, map[string]string{"ETag": `"1"`}, existing.Headers)
		return nil
	})
	require.NoError(t, err)

	err = tx.WithinTx(ctx, func(ctx context.Context) error {
		return store.Complete(ctx, idempotencyRecord(uuid.NewString(), 200))
	})
	assert.ErrorIs(t, err, domain.ErrNotFound, "completing an unclaimed key is a bug")

	deleted, err := store.DeleteExpired(ctx, time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.GreaterOrEqual(t, deleted, int64(1))
}

// Regression: a duplicate arriving while the first request is still running
// waits for it and then replays its response instead of running again.
func TestIdempotency_ConcurrentDuplicateWaitsForFirst(t *testing.T) {
	for _, firstCommits := range []bool{true, false} {
		name := map[bool]string{true: "first commits", false: "first rolls back"}[firstCommits]
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := postgres.NewIdempotencyRepository(testPool)
			tx := postgres.NewTxManager(testPool)
			key := uuid.NewString()

			firstClaimed, finishFirst := make(chan struct{}), make(chan struct{})
			go func() {
				_ = tx.WithinTx(ctx, func(ctx context.Context) error {
					_, _, _ = store.Claim(ctx, "scope", key, "h")
					close(firstClaimed)
					<-finishFirst
					if !firstCommits {
						return errors.New("first request failed")
					}
					rec := idempotencyRecord(key, 201)
					rec.Scope = "scope"
					return store.Complete(ctx, rec)
				})
			}()
			<-firstClaimed

			secondDone := make(chan struct{})
			var existing idempotencyResult
			go func() {
				defer close(secondDone)
				_ = tx.WithinTx(ctx, func(ctx context.Context) error {
					rec, claimed, err := store.Claim(ctx, "scope", key, "h")
					existing = idempotencyResult{rec.StatusCode, claimed, err}
					return nil
				})
			}()

			select {
			case <-secondDone:
				t.Fatal("duplicate must block while the first request holds the key")
			case <-time.After(200 * time.Millisecond):
			}
			close(finishFirst)
			<-secondDone

			require.NoError(t, existing.err)
			if firstCommits {
				assert.False(t, existing.claimed)
				assert.Equal(t, 201, existing.status)
			} else {
				assert.True(t, existing.claimed, "key is free again after rollback")
			}
		})
	}
}

type idempotencyResult struct {
	status  int
	claimed bool
	err     error
}

func idempotencyRecord(key string, status int) idempotency.Record {
	return idempotency.Record{Scope: "create_cargo", Key: key, RequestHash: "hash-1", StatusCode: status,
		Headers: map[string]string{"ETag": `"1"`}, Body: []byte(`{"ok":true}`)}
}

func drainOutbox(t *testing.T) {
	t.Helper()
	_, err := testPool.Exec(context.Background(), `UPDATE outbox SET published_at = now() WHERE published_at IS NULL`)
	require.NoError(t, err)
}

func runConcurrently(n int, fn func() error) []error {
	results := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i] = fn()
		}()
	}
	close(start)
	wg.Wait()
	return results
}

func count(errs []error, want error) int {
	n := 0
	for _, err := range errs {
		if errors.Is(err, want) {
			n++
		}
	}
	return n
}

func countIs(errs []error, target error) int {
	n := 0
	for _, err := range errs {
		if errors.Is(err, target) {
			n++
		}
	}
	return n
}
