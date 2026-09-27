package outbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/Ankitwasnik/tenant-service/internal/messaging"
	"github.com/Ankitwasnik/tenant-service/internal/store"
)

// DefaultConfirmTimeout bounds the wait for a batch's confirms (DESIGN.md §6).
const DefaultConfirmTimeout = 5 * time.Second

// returnsBuffer must hold every return the broker can send for one batch:
// amqp091 delivers returns synchronously from its reader goroutine, so a full
// channel would stall the connection. It is well above the relay's batch size.
const returnsBuffer = 1024

// AMQPPublisher publishes task events to the tasks exchange with publisher
// confirms and mandatory=true.
type AMQPPublisher struct {
	ch      *amqp.Channel
	returns chan amqp.Return
	timeout time.Duration
}

// NewAMQPPublisher opens a channel on conn in confirm mode and declares the
// topology on it. The publisher uses that one channel; it is not safe for
// concurrent Publish calls, and the relay never makes any.
func NewAMQPPublisher(conn *amqp.Connection, confirmTimeout time.Duration) (*AMQPPublisher, error) {
	ch, err := conn.Channel()
	if err != nil {
		return nil, fmt.Errorf("open publisher channel: %w", err)
	}
	if err := messaging.DeclareTopology(ch); err != nil {
		_ = ch.Close()
		return nil, err
	}
	if err := ch.Confirm(false); err != nil {
		_ = ch.Close()
		return nil, fmt.Errorf("enable publisher confirms: %w", err)
	}
	if confirmTimeout <= 0 {
		confirmTimeout = DefaultConfirmTimeout
	}
	return &AMQPPublisher{
		ch:      ch,
		returns: ch.NotifyReturn(make(chan amqp.Return, returnsBuffer)),
		timeout: confirmTimeout,
	}, nil
}

// Publish sends the whole batch, then waits for every confirm, bounded by the
// confirm timeout. A message counts as published only if the broker acked it
// and did not return it as unroutable. The rest are left out of confirmed and
// reported in err.
func (p *AMQPPublisher) Publish(ctx context.Context, msgs []store.OutboxMessage) ([]int64, error) {
	p.drainReturns() // stale returns from an earlier batch mean nothing now

	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	type pending struct {
		msg     store.OutboxMessage
		confirm *amqp.DeferredConfirmation
	}
	sent := make([]pending, 0, len(msgs))
	var publishErr error
	for _, m := range msgs {
		dc, err := p.ch.PublishWithDeferredConfirmWithContext(ctx, messaging.TasksExchange, m.RoutingKey,
			true,  // mandatory: an unroutable message is returned, not silently dropped
			false, // immediate: not supported by RabbitMQ
			amqp.Publishing{
				ContentType:  "application/json",
				DeliveryMode: amqp.Persistent,
				MessageId:    m.TaskID.String(), // the task id is the event id
				Type:         messaging.TaskEventType,
				Timestamp:    time.Now(),
				Body:         m.Payload,
			})
		if err != nil {
			// Typically the channel or connection is gone: stop here, and
			// still collect confirms for what was already sent.
			publishErr = fmt.Errorf("publish outbox row %d: %w", m.ID, err)
			break
		}
		sent = append(sent, pending{msg: m, confirm: dc})
	}

	var acked []int64
	var failures []error
	for _, s := range sent {
		ok, err := s.confirm.WaitContext(ctx)
		switch {
		case err != nil:
			failures = append(failures, fmt.Errorf("outbox row %d: no confirm: %w", s.msg.ID, err))
		case !ok:
			failures = append(failures, fmt.Errorf("outbox row %d: nacked by the broker", s.msg.ID))
		default:
			acked = append(acked, s.msg.ID)
		}
	}

	// The broker sends a mandatory message's return before its ack, and
	// amqp091 hands returns over before it processes later frames, so by now
	// every return for this batch is in the channel.
	returned := p.drainReturns()
	confirmed := make([]int64, 0, len(acked))
	for _, s := range sent {
		if !slices.Contains(acked, s.msg.ID) {
			continue
		}
		if returned[s.msg.TaskID.String()] {
			failures = append(failures, fmt.Errorf("outbox row %d: returned as unroutable (routing key %q)", s.msg.ID, s.msg.RoutingKey))
			continue
		}
		confirmed = append(confirmed, s.msg.ID)
	}

	if publishErr != nil {
		failures = append(failures, publishErr)
	}
	return confirmed, errors.Join(failures...)
}

// Close closes the publisher's channel.
func (p *AMQPPublisher) Close() error {
	return p.ch.Close()
}

// drainReturns empties the returns channel and returns the message ids seen.
//
// amqp091 closes the channel when the AMQP channel closes (say, the broker
// went away). A receive on a closed Go channel succeeds at once, forever, so
// the !ok check is what ends the loop; without it, this would spin.
func (p *AMQPPublisher) drainReturns() map[string]bool {
	seen := map[string]bool{}
	for {
		select {
		case r, ok := <-p.returns:
			if !ok {
				return seen
			}
			seen[r.MessageId] = true
		default:
			return seen
		}
	}
}
