// Package rabbitmq adapts the event pipeline to RabbitMQ.
package rabbitmq

import (
	"fmt"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/drzbraz/deus.ai/internal/events"
)

const (
	Exchange           = "cargo.events"
	Queue              = "cargo.status_changed"
	DeadLetterExchange = "cargo.events.dlx"
	DeadLetterQueue    = "cargo.status_changed.dlq"

	// deliveryLimit dead-letters a message that keeps crashing the consumer
	// (e.g. the process dies while handling it), so it cannot block the queue.
	deliveryLimit = 5

	dialTimeout = 5 * time.Second
	heartbeat   = 10 * time.Second
)

func dial(url string) (*amqp.Connection, error) {
	conn, err := amqp.DialConfig(url, amqp.Config{
		Heartbeat: heartbeat,
		Dial:      amqp.DefaultDial(dialTimeout),
	})
	if err != nil {
		return nil, fmt.Errorf("dial rabbitmq: %w", err)
	}
	return conn, nil
}

// declareTopology is idempotent, so both the publisher and the consumer call
// it on every (re)connect and neither depends on the other starting first.
// Quorum queues are replicated and durable and support a delivery limit.
func declareTopology(ch *amqp.Channel) error {
	steps := []struct {
		name string
		run  func() error
	}{
		{"declare exchange", func() error {
			return ch.ExchangeDeclare(Exchange, amqp.ExchangeTopic, true, false, false, false, nil)
		}},
		{"declare dead-letter exchange", func() error {
			return ch.ExchangeDeclare(DeadLetterExchange, amqp.ExchangeFanout, true, false, false, false, nil)
		}},
		{"declare dead-letter queue", func() error {
			_, err := ch.QueueDeclare(DeadLetterQueue, true, false, false, false,
				amqp.Table{amqp.QueueTypeArg: amqp.QueueTypeQuorum})
			return err //nolint:wrapcheck // wrapped with the step name below
		}},
		{"bind dead-letter queue", func() error {
			return ch.QueueBind(DeadLetterQueue, "", DeadLetterExchange, false, nil)
		}},
		{"declare queue", func() error {
			_, err := ch.QueueDeclare(Queue, true, false, false, false, amqp.Table{
				amqp.QueueTypeArg:        amqp.QueueTypeQuorum,
				"x-delivery-limit":       deliveryLimit,
				"x-dead-letter-exchange": DeadLetterExchange,
			})
			return err //nolint:wrapcheck // wrapped with the step name below
		}},
		{"bind queue", func() error {
			return ch.QueueBind(Queue, events.RoutingKeyStatusChanged, Exchange, false, nil)
		}},
	}

	for _, step := range steps {
		if err := step.run(); err != nil {
			return fmt.Errorf("%s: %w", step.name, err)
		}
	}
	return nil
}

// headerCarrier lets OpenTelemetry read and write trace context in AMQP headers.
type headerCarrier amqp.Table

func (c headerCarrier) Get(key string) string {
	value, _ := c[key].(string)
	return value
}

func (c headerCarrier) Set(key, value string) { c[key] = value }

func (c headerCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for key := range c {
		keys = append(keys, key)
	}
	return keys
}
