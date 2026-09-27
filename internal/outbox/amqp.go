package outbox

import (
	"context"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/Ankitwasnik/tenant-service/internal/messaging"
	"github.com/Ankitwasnik/tenant-service/internal/store"
)

// DefaultConfirmTimeout bounds the wait for a batch's confirms (DESIGN.md §6).
const DefaultConfirmTimeout = messaging.DefaultConfirmTimeout

// AMQPPublisher publishes outbox rows as task events to the tasks exchange,
// through a messaging.ConfirmPublisher (confirms, mandatory=true).
type AMQPPublisher struct {
	p *messaging.ConfirmPublisher
}

func NewAMQPPublisher(conn *amqp.Connection, confirmTimeout time.Duration) (*AMQPPublisher, error) {
	p, err := messaging.NewConfirmPublisher(conn, messaging.TasksExchange, confirmTimeout)
	if err != nil {
		return nil, err
	}
	return &AMQPPublisher{p: p}, nil
}

// Publish sends the rows and returns the outbox ids of those the broker
// confirmed. The AMQP message_id is the task id: the task is the event.
func (a *AMQPPublisher) Publish(ctx context.Context, rows []store.OutboxMessage) ([]int64, error) {
	msgs := make([]messaging.Message, len(rows))
	byMessageID := make(map[string]int64, len(rows))
	for i, r := range rows {
		id := r.TaskID.String()
		msgs[i] = messaging.Message{RoutingKey: r.RoutingKey, MessageID: id, Type: messaging.TaskEventType, Body: r.Payload}
		byMessageID[id] = r.ID
	}

	confirmed, err := a.p.Publish(ctx, msgs)
	ids := make([]int64, 0, len(confirmed))
	for _, messageID := range confirmed {
		ids = append(ids, byMessageID[messageID])
	}
	return ids, err
}

// Close closes the publisher's channel.
func (a *AMQPPublisher) Close() error {
	return a.p.Close()
}
