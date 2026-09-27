package messaging

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// DefaultConfirmTimeout bounds the wait for a batch's confirms (DESIGN.md §6).
const DefaultConfirmTimeout = 5 * time.Second

// returnsBuffer must hold every return the broker can send for one batch:
// amqp091 delivers returns synchronously from its reader goroutine, so a full
// channel would stall the connection. It is well above any batch we publish.
const returnsBuffer = 1024

// Message is one message to publish. MessageID must be unique within a batch:
// it is how a return from the broker is matched to its message.
type Message struct {
	RoutingKey string
	MessageID  string
	Type       string
	Body       []byte
}

// ConfirmPublisher publishes persistent JSON messages to one exchange with
// publisher confirms and mandatory=true. It is used by both the outbox relay
// and the worker, so the confirm and return handling exists once.
//
// It owns one AMQP channel and is not safe for concurrent Publish calls:
// give each goroutine its own.
type ConfirmPublisher struct {
	ch       *amqp.Channel
	exchange string
	returns  chan amqp.Return
	timeout  time.Duration
}

// NewConfirmPublisher opens a channel on conn in confirm mode, declares the
// topology on it, and publishes to exchange.
func NewConfirmPublisher(conn *amqp.Connection, exchange string, confirmTimeout time.Duration) (*ConfirmPublisher, error) {
	ch, err := conn.Channel()
	if err != nil {
		return nil, fmt.Errorf("open publisher channel: %w", err)
	}
	if err := DeclareTopology(ch); err != nil {
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
	return &ConfirmPublisher{
		ch:       ch,
		exchange: exchange,
		returns:  ch.NotifyReturn(make(chan amqp.Return, returnsBuffer)),
		timeout:  confirmTimeout,
	}, nil
}

// Publish sends the whole batch, then waits for every confirm, bounded by the
// confirm timeout. It returns the MessageIDs the broker acked and did not
// return as unroutable; every other message is reported in err. Waiting once
// for the batch, not per message, saves a broker round trip per message.
func (p *ConfirmPublisher) Publish(ctx context.Context, msgs []Message) ([]string, error) {
	p.drainReturns() // stale returns from an earlier batch mean nothing now

	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	type pending struct {
		msg     Message
		confirm *amqp.DeferredConfirmation
	}
	sent := make([]pending, 0, len(msgs))
	var publishErr error
	for _, m := range msgs {
		dc, err := p.ch.PublishWithDeferredConfirmWithContext(ctx, p.exchange, m.RoutingKey,
			true,  // mandatory: an unroutable message is returned, not silently dropped
			false, // immediate: not supported by RabbitMQ
			amqp.Publishing{
				ContentType:  "application/json",
				DeliveryMode: amqp.Persistent,
				MessageId:    m.MessageID,
				Type:         m.Type,
				Timestamp:    time.Now(),
				Body:         m.Body,
			})
		if err != nil {
			// Typically the channel or connection is gone: stop here, and
			// still collect confirms for what was already sent.
			publishErr = fmt.Errorf("publish %s: %w", m.MessageID, err)
			break
		}
		sent = append(sent, pending{msg: m, confirm: dc})
	}

	var acked []string
	var failures []error
	for _, s := range sent {
		ok, err := s.confirm.WaitContext(ctx)
		switch {
		case err != nil:
			failures = append(failures, fmt.Errorf("%s: no confirm: %w", s.msg.MessageID, err))
		case !ok:
			failures = append(failures, fmt.Errorf("%s: nacked by the broker", s.msg.MessageID))
		default:
			acked = append(acked, s.msg.MessageID)
		}
	}

	// The broker sends a mandatory message's return before its ack, and
	// amqp091 hands returns over before it processes later frames, so by now
	// every return for this batch is in the channel.
	returned := p.drainReturns()
	confirmed := make([]string, 0, len(acked))
	for _, s := range sent {
		if !slices.Contains(acked, s.msg.MessageID) {
			continue
		}
		if returned[s.msg.MessageID] {
			failures = append(failures, fmt.Errorf("%s: returned as unroutable (routing key %q)", s.msg.MessageID, s.msg.RoutingKey))
			continue
		}
		confirmed = append(confirmed, s.msg.MessageID)
	}

	if publishErr != nil {
		failures = append(failures, publishErr)
	}
	return confirmed, errors.Join(failures...)
}

// Close closes the publisher's channel.
func (p *ConfirmPublisher) Close() error {
	return p.ch.Close()
}

// drainReturns empties the returns channel and returns the message ids seen.
//
// amqp091 closes the channel when the AMQP channel closes (say, the broker
// went away). A receive on a closed Go channel succeeds at once, forever, so
// the !ok check is what ends the loop; without it, this would spin.
func (p *ConfirmPublisher) drainReturns() map[string]bool {
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
