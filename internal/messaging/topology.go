package messaging

import (
	"fmt"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Topology names (DESIGN.md §6). The control plane and the worker both declare
// the whole topology from this one definition: RabbitMQ rejects a
// re-declaration whose arguments differ (PRECONDITION_FAILED), so there must
// be exactly one source of truth.
const (
	TasksExchange       = "tasks"        // control plane → worker
	TaskUpdatesExchange = "task-updates" // worker → control plane
	DeadLetterExchange  = "dlx"

	WorkerTasksQueue        = "worker.tasks"
	ControlPlaneUpdateQueue = "controlplane.task-updates"

	// MaxDeliveries is the quorum queues' x-delivery-limit: a message that
	// keeps failing (say, it crashes its consumer) is dead-lettered after this
	// many deliveries instead of looping forever.
	MaxDeliveries = 10

	// AMQP message types, so a consumer can reject a message it doesn't expect.
	TaskEventType  = "task.v1"
	TaskUpdateType = "task_update.v1"
)

// DeadLetterQueue is the queue that a main queue's dead-lettered messages go to.
func DeadLetterQueue(queue string) string {
	return queue + ".dlq"
}

// queueSpec is one main queue: where it is bound, and its dead-letter queue.
type queueSpec struct {
	name     string
	exchange string
	pattern  string // topic binding
}

var mainQueues = []queueSpec{
	{name: WorkerTasksQueue, exchange: TasksExchange, pattern: "task.*"},
	{name: ControlPlaneUpdateQueue, exchange: TaskUpdatesExchange, pattern: "task.update.*"},
}

// Dial connects to RabbitMQ. Reconnecting after a lost connection comes later
// (PLAN.md 7.1).
func Dial(url string) (*amqp.Connection, error) {
	conn, err := amqp.Dial(url)
	if err != nil {
		return nil, fmt.Errorf("connect to broker: %w", err)
	}
	return conn, nil
}

// DeclareTopology declares every exchange, queue and binding. All of it is
// durable, and declaring it again with the same arguments is a no-op, so
// every process calls this on each (re)connect.
//
// The main queues are quorum queues (replicated, and the only type with a
// delivery limit). Each dead-letters to dlx with its own name as the routing
// key, and each <queue>.dlq is bound to dlx under that key.
func DeclareTopology(ch *amqp.Channel) error {
	for _, ex := range []struct{ name, kind string }{
		{TasksExchange, amqp.ExchangeTopic},
		{TaskUpdatesExchange, amqp.ExchangeTopic},
		{DeadLetterExchange, amqp.ExchangeDirect},
	} {
		if err := ch.ExchangeDeclare(ex.name, ex.kind, true, false, false, false, nil); err != nil {
			return fmt.Errorf("declare exchange %s: %w", ex.name, err)
		}
	}

	for _, q := range mainQueues {
		if err := declareQueue(ch, q.name, amqp.Table{
			amqp.QueueTypeArg:        amqp.QueueTypeQuorum,
			"x-delivery-limit":       MaxDeliveries,
			"x-dead-letter-exchange": DeadLetterExchange,
			// Keyed by the queue's own name, so each main queue's dead
			// letters reach its own DLQ through the one shared dlx.
			"x-dead-letter-routing-key": q.name,
		}); err != nil {
			return err
		}
		if err := ch.QueueBind(q.name, q.pattern, q.exchange, false, nil); err != nil {
			return fmt.Errorf("bind %s to %s: %w", q.name, q.exchange, err)
		}

		dlq := DeadLetterQueue(q.name)
		if err := declareQueue(ch, dlq, amqp.Table{amqp.QueueTypeArg: amqp.QueueTypeQuorum}); err != nil {
			return err
		}
		if err := ch.QueueBind(dlq, q.name, DeadLetterExchange, false, nil); err != nil {
			return fmt.Errorf("bind %s to %s: %w", dlq, DeadLetterExchange, err)
		}
	}
	return nil
}

func declareQueue(ch *amqp.Channel, name string, args amqp.Table) error {
	// durable, not auto-deleted, not exclusive, wait for the broker's answer.
	if _, err := ch.QueueDeclare(name, true, false, false, false, args); err != nil {
		return fmt.Errorf("declare queue %s: %w", name, err)
	}
	return nil
}
