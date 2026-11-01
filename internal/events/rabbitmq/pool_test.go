package rabbitmq

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type item struct {
	key string
	seq int
}

func TestShardFor_IsStableAndInRange(t *testing.T) {
	for i := range 1000 {
		key := fmt.Sprintf("cargo-%d", i)
		shard := shardFor(key, 7)
		assert.GreaterOrEqual(t, shard, 0)
		assert.Less(t, shard, 7)
		assert.Equal(t, shard, shardFor(key, 7), "same key, same worker")
	}
}

// Regression: items of one key are handled in the order they arrived, even
// when the handler takes a random amount of time.
func TestShardedPool_PreservesOrderPerKey(t *testing.T) {
	var mu sync.Mutex
	seen := map[string][]int{}
	pool := newShardedPool(4, 2, func(it item) {
		time.Sleep(time.Duration(rand.IntN(300)) * time.Microsecond)
		mu.Lock()
		seen[it.key] = append(seen[it.key], it.seq)
		mu.Unlock()
	})

	const keys, perKey = 20, 25
	for seq := range perKey {
		for k := range keys {
			key := fmt.Sprintf("cargo-%d", k)
			require.True(t, pool.dispatch(context.Background(), key, item{key, seq}))
		}
	}
	pool.stop()

	require.Len(t, seen, keys)
	for key, seqs := range seen {
		require.Len(t, seqs, perKey, key)
		for i := range seqs {
			assert.Equal(t, i, seqs[i], "%s handled out of order: %v", key, seqs)
		}
	}
}

// Different keys must not wait for each other: two items on different
// workers can only both reach the barrier if they run at the same time.
func TestShardedPool_DifferentKeysRunInParallel(t *testing.T) {
	keyA, keyB := keysOnDifferentWorkers(t, 4)
	var arrived sync.WaitGroup
	arrived.Add(2)
	bothRunning := make(chan struct{})
	go func() { arrived.Wait(); close(bothRunning) }()

	pool := newShardedPool(4, 1, func(item) {
		arrived.Done()
		select {
		case <-bothRunning:
		case <-time.After(2 * time.Second):
		}
	})
	pool.dispatch(context.Background(), keyA, item{keyA, 0})
	pool.dispatch(context.Background(), keyB, item{keyB, 0})

	select {
	case <-bothRunning:
	case <-time.After(time.Second):
		t.Fatal("items for different keys were not processed concurrently")
	}
	pool.stop()
}

// Regression: a worker stuck on one key must not stop items for other keys
// from being dispatched, as long as its buffer can absorb its backlog.
func TestShardedPool_BusyWorkerDoesNotStarveOthers(t *testing.T) {
	keyA, keyB := keysOnDifferentWorkers(t, 2)
	release := make(chan struct{})
	handledB := make(chan struct{})
	pool := newShardedPool(2, 20, func(it item) {
		if it.key == keyA {
			<-release
			return
		}
		close(handledB)
	})

	dispatched := make(chan struct{})
	go func() {
		for i := range 10 { // a backlog for the stuck key, within its buffer
			pool.dispatch(context.Background(), keyA, item{keyA, i})
		}
		pool.dispatch(context.Background(), keyB, item{keyB, 0})
		close(dispatched)
	}()

	select {
	case <-handledB:
	case <-time.After(time.Second):
		t.Fatal("an item for an idle worker waited behind a busy one")
	}
	<-dispatched
	close(release)
	pool.stop()
}

func TestShardedPool_StopWaitsForAcceptedItems(t *testing.T) {
	var handled atomic.Int32
	pool := newShardedPool(2, 10, func(item) {
		time.Sleep(5 * time.Millisecond)
		handled.Add(1)
	})
	for i := range 10 {
		pool.dispatch(context.Background(), fmt.Sprint(i), item{})
	}

	pool.stop()

	assert.Equal(t, int32(10), handled.Load(), "nothing accepted is dropped on shutdown")
}

func TestShardedPool_DispatchGivesUpWhenCancelled(t *testing.T) {
	release := make(chan struct{})
	pool := newShardedPool(1, 0, func(item) { <-release })
	require.True(t, pool.dispatch(context.Background(), "k", item{})) // worker busy now

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.False(t, pool.dispatch(ctx, "k", item{}), "a full worker must not block shutdown")

	close(release)
	pool.stop()
}

func keysOnDifferentWorkers(t *testing.T, workers int) (string, string) {
	t.Helper()
	first := "cargo-0"
	for i := 1; i < 1000; i++ {
		key := fmt.Sprintf("cargo-%d", i)
		if shardFor(key, workers) != shardFor(first, workers) {
			return first, key
		}
	}
	t.Fatal("no keys on different workers")
	return "", ""
}
