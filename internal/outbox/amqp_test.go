package outbox_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/Ankitwasnik/tenant-service/internal/domain"
	"github.com/Ankitwasnik/tenant-service/internal/messaging"
	"github.com/Ankitwasnik/tenant-service/internal/outbox"
	"github.com/Ankitwasnik/tenant-service/internal/store"
	"github.com/Ankitwasnik/tenant-service/internal/testutil"
)

// Publisher tests against a real RabbitMQ, one vhost per test.

func TestAMQPPublisherConfirmsAndDelivers(t *testing.T) {
	t.Parallel()
	pub, ch := newPublisher(t, outbox.DefaultConfirmTimeout)
	msgs := []store.OutboxMessage{message(1, "task.deploy"), message(2, "task.destroy")}

	confirmed, err := pub.Publish(context.Background(), msgs)
	if err != nil || len(confirmed) != 2 {
		t.Fatalf("Publish = %v, %v; want both confirmed", confirmed, err)
	}

	for _, want := range msgs {
		d := receive(t, ch, messaging.WorkerTasksQueue)
		if d.MessageId != want.TaskID.String() || d.Type != messaging.TaskEventType ||
			d.DeliveryMode != amqp.Persistent || d.ContentType != "application/json" || string(d.Body) != string(want.Payload) {
			t.Errorf("delivery = id %s type %s mode %d ct %s body %s", d.MessageId, d.Type, d.DeliveryMode, d.ContentType, d.Body)
		}
	}
}

// With mandatory=true an unroutable message comes back as a return, so it is
// not counted as published, even though the broker acks it.
func TestAMQPPublisherUnroutableIsNotConfirmed(t *testing.T) {
	t.Parallel()
	pub, _ := newPublisher(t, outbox.DefaultConfirmTimeout)
	routable, unroutable := message(1, "task.deploy"), message(2, "nothing.binds.this")

	confirmed, err := pub.Publish(context.Background(), []store.OutboxMessage{routable, unroutable})
	if len(confirmed) != 1 || confirmed[0] != routable.ID {
		t.Fatalf("confirmed = %v, want only %d", confirmed, routable.ID)
	}
	if err == nil || !strings.Contains(err.Error(), "unroutable") {
		t.Fatalf("err = %v, want an unroutable error", err)
	}
}

// A confirm that doesn't arrive in time counts as not published.
func TestAMQPPublisherTimeoutIsNotConfirmed(t *testing.T) {
	t.Parallel()
	pub, _ := newPublisher(t, time.Nanosecond)

	confirmed, err := pub.Publish(context.Background(), []store.OutboxMessage{message(1, "task.deploy")})
	if len(confirmed) != 0 || err == nil {
		t.Fatalf("Publish = %v, %v; want nothing confirmed and an error", confirmed, err)
	}
}

func TestAMQPPublisherClosedChannel(t *testing.T) {
	t.Parallel()
	pub, _ := newPublisher(t, outbox.DefaultConfirmTimeout)
	if err := pub.Close(); err != nil {
		t.Fatal(err)
	}

	confirmed, err := pub.Publish(context.Background(), []store.OutboxMessage{message(1, "task.deploy")})
	if len(confirmed) != 0 || err == nil {
		t.Fatalf("Publish = %v, %v; want nothing confirmed and an error", confirmed, err)
	}
}

// End to end through the real store and broker: a committed create becomes
// exactly one message carrying the task, and a rejected PATCH adds nothing.
func TestRelayPublishesCommittedCreateToWorkerQueue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, pool := newRepo(t)
	pub, ch := newPublisher(t, outbox.DefaultConfirmTimeout)

	tenant, task, err := repo.CreateTenant(ctx, "acme", "Acme")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.PatchTenant(ctx, tenant.ID, "Renamed", 1); err == nil {
		t.Fatal("PATCH on a provisioning tenant succeeded")
	}

	if _, marked, err := newRelay(repo, pub).Step(ctx); err != nil || marked != 1 {
		t.Fatalf("Step marked %d (err %v), want 1", marked, err)
	}
	assertUnpublished(t, pool, 0)

	d := receive(t, ch, messaging.WorkerTasksQueue)
	var event messaging.TaskEvent
	if err := json.Unmarshal(d.Body, &event); err != nil {
		t.Fatalf("decode event: %v: %s", err, d.Body)
	}
	if event.ID != task.ID || event.Type != domain.TaskDeploy || event.TenantID != tenant.ID || event.Status != domain.TaskAccepted {
		t.Errorf("event = %+v, want task %s deploy/accepted for tenant %s", event, task.ID, tenant.ID)
	}
	if d.MessageId != task.ID.String() {
		t.Errorf("message_id = %s, want the task id", d.MessageId)
	}
	if extra, ok, _ := ch.Get(messaging.WorkerTasksQueue, true); ok {
		t.Errorf("a second message was published: %s", extra.Body)
	}
}

// ---- helpers ----

// newPublisher returns a publisher on a fresh vhost (with the topology
// declared) and a separate channel for reading what it sent.
func newPublisher(t *testing.T, timeout time.Duration) (*outbox.AMQPPublisher, *amqp.Channel) {
	t.Helper()
	conn, err := messaging.Dial(testutil.NewVhost(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	pub, err := outbox.NewAMQPPublisher(conn, timeout)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	return pub, ch
}

func message(id int64, routingKey string) store.OutboxMessage {
	taskID := uuid.New()
	return store.OutboxMessage{
		ID: id, TaskID: taskID, RoutingKey: routingKey,
		Payload: []byte(`{"id":"` + taskID.String() + `"}`),
	}
}

// receive takes one message from queue, waiting briefly for it to arrive.
func receive(t *testing.T, ch *amqp.Channel, queue string) amqp.Delivery {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		d, ok, err := ch.Get(queue, true)
		if err != nil {
			t.Fatalf("get from %s: %v", queue, err)
		}
		if ok {
			return d
		}
		if time.Now().After(deadline) {
			t.Fatalf("no message in %s", queue)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
