package rabbitmq

import (
	"context"
	"hash/fnv"
	"sync"
)

// shardedPool processes items on a fixed number of worker goroutines.
// Items with the same key always go to the same worker, so they are handled
// one at a time and in arrival order, while items with different keys are
// handled in parallel. For cargo events the key is the cargo id: events of
// one cargo keep their order, different cargoes do not wait for each other.
type shardedPool[T any] struct {
	queues []chan T
	wg     sync.WaitGroup
}

// newShardedPool starts one goroutine per worker. buffer is how many items
// may wait for each worker before dispatch blocks (backpressure).
func newShardedPool[T any](workers, buffer int, handle func(T)) *shardedPool[T] {
	p := &shardedPool[T]{queues: make([]chan T, workers)}
	for i := range p.queues {
		queue := make(chan T, buffer)
		p.queues[i] = queue
		p.wg.Go(func() {
			for item := range queue {
				handle(item)
			}
		})
	}
	return p
}

// dispatch hands item to the worker that owns key. It returns false when ctx
// is cancelled before the worker could accept it.
func (p *shardedPool[T]) dispatch(ctx context.Context, key string, item T) bool {
	select {
	case p.queues[shardFor(key, len(p.queues))] <- item:
		return true
	case <-ctx.Done():
		return false
	}
}

// stop lets every worker finish the items it already accepted, then returns.
// dispatch must not be called after stop.
func (p *shardedPool[T]) stop() {
	for _, queue := range p.queues {
		close(queue)
	}
	p.wg.Wait()
}

// shardFor maps a key to a worker index; the same key always gets the same worker.
func shardFor(key string, workers int) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum32() % uint32(workers)) //nolint:gosec // workers is a small positive count
}
