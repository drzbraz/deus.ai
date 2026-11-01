package rabbitmq

import (
	"context"
	"errors"
	"fmt"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel"
	"go.uber.org/zap"

	"github.com/drzbraz/deus.ai/internal/backoff"
	"github.com/drzbraz/deus.ai/internal/events"
)

const (
	consumerTag = "logistics-api"
	// DefaultWorkers is how many events are stored in parallel.
	DefaultWorkers = 4
	// prefetchPerWorker sizes the broker's prefetch window: at most
	// workers*prefetchPerWorker messages are unacknowledged at any time.
	prefetchPerWorker = 5
)

var errDeliveriesClosed = errors.New("delivery channel closed by broker")

// MessageHandler decides the fate of one delivery (see events.Handler).
type MessageHandler interface {
	Handle(ctx context.Context, payload []byte) events.Decision
}

// Consumer is the background worker that reads cargo status events. It
// stores events on several goroutines, keeping each cargo's events in order
// (see shardedPool), and reconnects with backoff whenever the broker
// connection is lost.
type Consumer struct {
	url     string
	handler MessageHandler
	log     *zap.Logger
	backoff backoff.Policy
	workers int
}

type ConsumerOption func(*Consumer)

// WithWorkers sets how many events are processed in parallel (minimum 1).
func WithWorkers(n int) ConsumerOption {
	return func(c *Consumer) { c.workers = max(n, 1) }
}

func NewConsumer(url string, handler MessageHandler, log *zap.Logger, policy backoff.Policy, opts ...ConsumerOption) *Consumer {
	c := &Consumer{url: url, handler: handler, log: log, backoff: policy, workers: DefaultWorkers}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Run consumes until ctx is cancelled.
func (c *Consumer) Run(ctx context.Context) {
	failures := 0
	for {
		connected, err := c.consume(ctx)
		if ctx.Err() != nil {
			return
		}
		if connected {
			failures = 0
		}
		failures++

		delay := c.backoff.Delay(failures)
		c.log.Warn("event consumer disconnected, reconnecting", zap.Error(err), zap.Duration("retry_in", delay))
		if backoff.Sleep(ctx, delay) != nil {
			return
		}
	}
}

// consume processes deliveries on one connection until it breaks. connected
// reports whether the connection was established, to reset the backoff.
func (c *Consumer) consume(ctx context.Context) (connected bool, err error) {
	conn, err := dial(c.url)
	if err != nil {
		return false, err
	}
	defer conn.Close()

	prefetch := c.workers * prefetchPerWorker
	deliveries, err := subscribe(conn, prefetch)
	if err != nil {
		return false, err
	}
	c.log.Info("event consumer connected", zap.String("queue", Queue), zap.Int("workers", c.workers))

	// Each worker can buffer the whole prefetch window. The broker never has
	// more than prefetch unacknowledged messages out, so dispatch never blocks
	// on one busy worker while the others sit idle (a slow cargo cannot
	// starve the rest). Deferred after conn.Close, so it runs first: workers
	// finish (and ack) the messages they hold while the channel is still open.
	pool := newShardedPool(c.workers, prefetch, func(d amqp.Delivery) { c.process(ctx, d) })
	defer pool.stop()

	for {
		select {
		case <-ctx.Done():
			return true, nil
		case delivery, ok := <-deliveries:
			if !ok {
				return true, errDeliveriesClosed
			}
			if !pool.dispatch(ctx, orderingKey(delivery), delivery) {
				c.acknowledge(delivery, delivery.Nack(false, true)) // shutting down: give it back
			}
		}
	}
}

// orderingKey is the cargo id set by the publisher. Messages without it
// (e.g. published by another tool) are spread by message id.
func orderingKey(d amqp.Delivery) string {
	if key, ok := d.Headers[headerAggregateID].(string); ok && key != "" {
		return key
	}
	return d.MessageId
}

func subscribe(conn *amqp.Connection, prefetch int) (<-chan amqp.Delivery, error) {
	ch, err := conn.Channel()
	if err != nil {
		return nil, fmt.Errorf("open channel: %w", err)
	}
	if err := declareTopology(ch); err != nil {
		return nil, err
	}
	if err := ch.Qos(prefetch, 0, false); err != nil {
		return nil, fmt.Errorf("set prefetch: %w", err)
	}
	deliveries, err := ch.Consume(Queue, consumerTag, false, false, false, false, nil)
	if err != nil {
		return nil, fmt.Errorf("start consuming: %w", err)
	}
	return deliveries, nil
}

func (c *Consumer) process(ctx context.Context, d amqp.Delivery) {
	// Shutting down: give queued messages back without running the handler.
	// An explicit requeue does not count towards the queue's delivery limit
	// (a connection close does), so restarts cannot dead-letter good events.
	if ctx.Err() != nil {
		c.acknowledge(d, d.Nack(false, true))
		return
	}

	ctx = otel.GetTextMapPropagator().Extract(ctx, headerCarrier(d.Headers))

	var err error
	switch c.handler.Handle(ctx, d.Body) {
	case events.Ack:
		err = d.Ack(false)
	case events.Reject:
		err = d.Reject(false) // routed to the dead-letter exchange
	case events.Requeue:
		err = d.Nack(false, true)
	}
	c.acknowledge(d, err)
}

// acknowledge logs a failed ack/nack. The broker redelivers unacknowledged
// messages when the channel closes, and the handler is idempotent, so logging
// is enough.
func (c *Consumer) acknowledge(d amqp.Delivery, err error) {
	if err != nil {
		c.log.Warn("acknowledge delivery", zap.Error(err), zap.String("message_id", d.MessageId))
	}
}
