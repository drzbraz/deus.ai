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
	// prefetch bounds unacknowledged messages held by this consumer.
	prefetch = 10
)

var errDeliveriesClosed = errors.New("delivery channel closed by broker")

// MessageHandler decides the fate of one delivery (see events.Handler).
type MessageHandler interface {
	Handle(ctx context.Context, payload []byte) events.Decision
}

// Consumer is the background worker that reads cargo status events. It
// reconnects with backoff whenever the broker connection is lost.
type Consumer struct {
	url     string
	handler MessageHandler
	log     *zap.Logger
	backoff backoff.Policy
}

func NewConsumer(url string, handler MessageHandler, log *zap.Logger, policy backoff.Policy) *Consumer {
	return &Consumer{url: url, handler: handler, log: log, backoff: policy}
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

	deliveries, err := subscribe(conn)
	if err != nil {
		return false, err
	}
	c.log.Info("event consumer connected", zap.String("queue", Queue))

	for {
		select {
		case <-ctx.Done():
			return true, nil
		case delivery, ok := <-deliveries:
			if !ok {
				return true, errDeliveriesClosed
			}
			c.process(ctx, delivery)
		}
	}
}

func subscribe(conn *amqp.Connection) (<-chan amqp.Delivery, error) {
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
	if err != nil {
		// The broker redelivers unacknowledged messages when the channel
		// closes, and the handler is idempotent, so logging is enough.
		c.log.Warn("acknowledge delivery", zap.Error(err), zap.String("message_id", d.MessageId))
	}
}
