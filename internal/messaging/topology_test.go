package messaging_test

import (
	"context"
	"errors"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/Ankitwasnik/tenant-service/internal/messaging"
	"github.com/Ankitwasnik/tenant-service/internal/testutil"
)

// Integration tests against a real RabbitMQ, one vhost per test.

func TestDeclareTopologyIsIdempotent(t *testing.T) {
	t.Parallel()
	url := testutil.NewVhost(t)
	ch := openChannel(t, url)

	// The control plane and the worker both declare it, on every connect.
	for i := range 3 {
		if err := messaging.DeclareTopology(ch); err != nil {
			t.Fatalf("declaration %d: %v", i+1, err)
		}
	}

	m := testutil.ManagementFor(t, url)
	for _, name := range []string{
		messaging.WorkerTasksQueue, messaging.ControlPlaneUpdateQueue,
		messaging.DeadLetterQueue(messaging.WorkerTasksQueue), messaging.DeadLetterQueue(messaging.ControlPlaneUpdateQueue),
	} {
		q := m.Queue(t, name)
		if q.Type != "quorum" || !q.Durable {
			t.Errorf("%s: type=%s durable=%v, want a durable quorum queue", name, q.Type, q.Durable)
		}
	}
	main := m.Queue(t, messaging.ControlPlaneUpdateQueue)
	if main.Arguments["x-delivery-limit"] != float64(messaging.MaxDeliveries) ||
		main.Arguments["x-dead-letter-exchange"] != "dlx" ||
		main.Arguments["x-dead-letter-routing-key"] != messaging.ControlPlaneUpdateQueue {
		t.Errorf("%s arguments = %v", messaging.ControlPlaneUpdateQueue, main.Arguments)
	}
}

// Why there must be one definition: a declaration that differs is rejected.
func TestConflictingDeclarationIsRejected(t *testing.T) {
	t.Parallel()
	url := testutil.NewVhost(t)
	if err := messaging.DeclareTopology(openChannel(t, url)); err != nil {
		t.Fatal(err)
	}

	ch := openChannel(t, url)
	_, err := ch.QueueDeclare(messaging.WorkerTasksQueue, true, false, false, false, amqp.Table{
		amqp.QueueTypeArg: amqp.QueueTypeQuorum, "x-delivery-limit": 3,
	})
	var amqpErr *amqp.Error
	if !errors.As(err, &amqpErr) || amqpErr.Code != amqp.PreconditionFailed {
		t.Fatalf("err = %v, want PRECONDITION_FAILED", err)
	}
}

func TestRouting(t *testing.T) {
	t.Parallel()
	ch := declaredChannel(t)

	publish(t, ch, messaging.TasksExchange, "task.deploy", "a task")
	publish(t, ch, messaging.TaskUpdatesExchange, "task.update.done", "an update")

	if got := getOne(t, ch, messaging.WorkerTasksQueue); got != "a task" {
		t.Errorf("worker.tasks got %q", got)
	}
	if got := getOne(t, ch, messaging.ControlPlaneUpdateQueue); got != "an update" {
		t.Errorf("controlplane.task-updates got %q", got)
	}
	// Each message reached only its own queue.
	assertEmpty(t, ch, messaging.WorkerTasksQueue)
	assertEmpty(t, ch, messaging.ControlPlaneUpdateQueue)
}

func TestRejectedMessageIsDeadLettered(t *testing.T) {
	t.Parallel()
	ch := declaredChannel(t)

	for _, q := range []struct{ exchange, key, queue string }{
		{messaging.TasksExchange, "task.deploy", messaging.WorkerTasksQueue},
		{messaging.TaskUpdatesExchange, "task.update.done", messaging.ControlPlaneUpdateQueue},
	} {
		publish(t, ch, q.exchange, q.key, "poison")
		d := get(t, ch, q.queue)
		if err := d.Nack(false, false); err != nil { // requeue=false: a poison message
			t.Fatal(err)
		}
		if got := getOne(t, ch, messaging.DeadLetterQueue(q.queue)); got != "poison" {
			t.Errorf("%s DLQ got %q", q.queue, got)
		}
	}
}

// A message that crashes its consumer every time (the channel closes with the
// message unacked) is dead-lettered once it has been redelivered
// MaxDeliveries times, instead of looping forever: 1 + MaxDeliveries
// deliveries in all.
func TestDeliveryLimitStopsCrashLoop(t *testing.T) {
	t.Parallel()
	url := testutil.NewVhost(t)
	setup := openChannel(t, url)
	if err := messaging.DeclareTopology(setup); err != nil {
		t.Fatal(err)
	}
	publish(t, setup, messaging.TaskUpdatesExchange, "task.update.done", "crashes its consumer")

	deliveries := 0
	for deliveries < 3*messaging.MaxDeliveries {
		ch := openChannel(t, url)
		if _, ok := tryGet(t, ch, messaging.ControlPlaneUpdateQueue); !ok {
			break // dead-lettered
		}
		deliveries++
		if err := ch.Close(); err != nil { // unacked: the "crash"
			t.Fatal(err)
		}
	}

	if want := 1 + messaging.MaxDeliveries; deliveries != want {
		t.Errorf("delivered %d times before dead-lettering, want %d", deliveries, want)
	}
	d := get(t, setup, messaging.DeadLetterQueue(messaging.ControlPlaneUpdateQueue))
	if string(d.Body) != "crashes its consumer" || d.Headers["x-first-death-reason"] != "delivery_limit" {
		t.Errorf("DLQ got %q, reason %v", d.Body, d.Headers["x-first-death-reason"])
	}
}

// Pins a RabbitMQ 4.x behavior the design relies on: a requeue by nack does
// not count toward the delivery limit. So a shutdown that requeues an
// in-flight message costs it nothing, and, equally, requeueing must never be
// used as a retry loop, because the limit would never stop it (DESIGN.md §6).
func TestRequeueByNackIsNotCounted(t *testing.T) {
	t.Parallel()
	ch := declaredChannel(t)
	publish(t, ch, messaging.TaskUpdatesExchange, "task.update.done", "requeued on shutdown")

	for range messaging.MaxDeliveries + 2 {
		d := get(t, ch, messaging.ControlPlaneUpdateQueue)
		if err := d.Nack(false, true); err != nil { // requeue=true
			t.Fatal(err)
		}
	}
	if got := getOne(t, ch, messaging.ControlPlaneUpdateQueue); got != "requeued on shutdown" {
		t.Errorf("main queue got %q", got)
	}
	assertEmpty(t, ch, messaging.DeadLetterQueue(messaging.ControlPlaneUpdateQueue))
}

// ---- helpers ----

func openChannel(t *testing.T, url string) *amqp.Channel {
	t.Helper()
	conn, err := messaging.Dial(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ch, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	return ch
}

func declaredChannel(t *testing.T) *amqp.Channel {
	t.Helper()
	ch := openChannel(t, testutil.NewVhost(t))
	if err := messaging.DeclareTopology(ch); err != nil {
		t.Fatal(err)
	}
	return ch
}

func publish(t *testing.T, ch *amqp.Channel, exchange, key, body string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ch.PublishWithContext(ctx, exchange, key, true, false, amqp.Publishing{
		DeliveryMode: amqp.Persistent, Body: []byte(body),
	}); err != nil {
		t.Fatal(err)
	}
}

// tryGet polls the queue briefly: a published or requeued message becomes
// visible to basic.get asynchronously.
func tryGet(t *testing.T, ch *amqp.Channel, queue string) (amqp.Delivery, bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		d, ok, err := ch.Get(queue, false)
		if err != nil {
			t.Fatalf("get from %s: %v", queue, err)
		}
		if ok || time.Now().After(deadline) {
			return d, ok
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func get(t *testing.T, ch *amqp.Channel, queue string) amqp.Delivery {
	t.Helper()
	d, ok := tryGet(t, ch, queue)
	if !ok {
		t.Fatalf("no message in %s", queue)
	}
	return d
}

// getOne takes one message, acks it, and returns its body.
func getOne(t *testing.T, ch *amqp.Channel, queue string) string {
	t.Helper()
	d := get(t, ch, queue)
	if err := d.Ack(false); err != nil {
		t.Fatal(err)
	}
	return string(d.Body)
}

func assertEmpty(t *testing.T, ch *amqp.Channel, queue string) {
	t.Helper()
	if d, ok, err := ch.Get(queue, false); err != nil || ok {
		t.Errorf("%s not empty: %q (err %v)", queue, d.Body, err)
	}
}
