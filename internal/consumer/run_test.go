package consumer_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/Ankitwasnik/tenant-service/internal/consumer"
	"github.com/Ankitwasnik/tenant-service/internal/domain"
	"github.com/Ankitwasnik/tenant-service/internal/messaging"
	"github.com/Ankitwasnik/tenant-service/internal/store"
	"github.com/Ankitwasnik/tenant-service/internal/testutil"
)

// Integration: the real consumer, store, Postgres and RabbitMQ.

func TestConsumerAppliesUpdates(t *testing.T) {
	t.Parallel()
	env := start(t)
	tenant, task := env.createTenant(t, "acme")

	env.publishUpdate(t, messaging.NewTaskUpdate(task.ID, domain.TaskInProgress, ""))
	env.publishUpdate(t, messaging.NewTaskUpdate(task.ID, domain.TaskDone, ""))

	env.waitForTenant(t, tenant.ID, domain.TenantActive)
	if got, _ := env.repo.GetTask(context.Background(), task.ID); got.Status != domain.TaskDone {
		t.Errorf("task status = %s, want done", got.Status)
	}
}

// Poison messages are dead-lettered, and the consumer keeps going: a valid
// message sent afterwards is still applied.
func TestConsumerDeadLettersPoisonAndCarriesOn(t *testing.T) {
	t.Parallel()
	env := start(t)
	tenant, task := env.createTenant(t, "acme")

	poison := map[string][]byte{
		"malformed":    []byte("not json"),
		"accepted":     mustJSON(t, map[string]string{"update_id": uuid.NewString(), "task_id": task.ID.String(), "status": "accepted"}),
		"unknown task": mustJSON(t, messaging.NewTaskUpdate(uuid.New(), domain.TaskDone, "")),
	}
	for name, body := range poison {
		env.publishRaw(t, name, body)
	}
	env.publishUpdate(t, messaging.NewTaskUpdate(task.ID, domain.TaskDone, ""))

	env.waitForTenant(t, tenant.ID, domain.TenantActive)

	dlq := messaging.DeadLetterQueue(messaging.ControlPlaneUpdateQueue)
	got := map[string]bool{}
	for range poison {
		d := env.receive(t, dlq)
		got[d.MessageId] = true
	}
	for name := range poison {
		if !got[name] {
			t.Errorf("%s message not in the DLQ (got %v)", name, got)
		}
	}
}

// At-least-once delivery: the same update published twice is applied once.
func TestConsumerIgnoresDuplicateDelivery(t *testing.T) {
	t.Parallel()
	env := start(t)
	tenant, task := env.createTenant(t, "acme")
	done := messaging.NewTaskUpdate(task.ID, domain.TaskDone, "")

	env.publishUpdate(t, done)
	env.publishUpdate(t, done)
	env.waitForTenant(t, tenant.ID, domain.TenantActive)
	env.waitForEmptyQueue(t, messaging.ControlPlaneUpdateQueue)

	got, err := env.repo.GetTenant(context.Background(), tenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 2 {
		t.Errorf("version = %d, want 2: the duplicate was applied again", got.Version)
	}
}

// ---- harness ----

type env struct {
	repo *store.Store
	pub  *messaging.ConfirmPublisher
	ch   *amqp.Channel
}

func start(t *testing.T) *env {
	t.Helper()
	pool, err := store.Open(context.Background(), testutil.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repo := store.New(pool)

	url := testutil.NewVhost(t)
	consumerConn, err := messaging.Dial(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = consumerConn.Close() })

	c := consumer.New(repo, slog.New(slog.NewJSONHandler(io.Discard, nil)), consumer.Config{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx, consumerConn) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("consumer.Run returned %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("consumer did not stop after cancel")
		}
	})

	conn, err := messaging.Dial(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	pub, err := messaging.NewConfirmPublisher(conn, messaging.TaskUpdatesExchange, 0)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	return &env{repo: repo, pub: pub, ch: ch}
}

func (e *env) createTenant(t *testing.T, slug string) (domain.Tenant, domain.Task) {
	t.Helper()
	tenant, task, err := e.repo.CreateTenant(context.Background(), slug, "Tenant "+slug)
	if err != nil {
		t.Fatal(err)
	}
	return tenant, task
}

func (e *env) publishUpdate(t *testing.T, u messaging.TaskUpdate) {
	t.Helper()
	e.publish(t, messaging.Message{
		RoutingKey: messaging.UpdateRoutingKey(u.Status), MessageID: u.UpdateID.String(),
		Type: messaging.TaskUpdateType, Body: mustJSON(t, u),
	})
}

// publishRaw publishes body as-is, with messageID to find it in the DLQ.
func (e *env) publishRaw(t *testing.T, messageID string, body []byte) {
	t.Helper()
	e.publish(t, messaging.Message{RoutingKey: "task.update.done", MessageID: messageID, Type: messaging.TaskUpdateType, Body: body})
}

func (e *env) publish(t *testing.T, m messaging.Message) {
	t.Helper()
	if confirmed, err := e.pub.Publish(context.Background(), []messaging.Message{m}); err != nil || len(confirmed) != 1 {
		t.Fatalf("publish %s: confirmed %v, err %v", m.MessageID, confirmed, err)
	}
}

func (e *env) waitForTenant(t *testing.T, id uuid.UUID, want domain.TenantStatus) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err := e.repo.GetTenant(context.Background(), id)
		if err == nil && got.Status == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("tenant status = %s (err %v), want %s", got.Status, err, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e *env) waitForEmptyQueue(t *testing.T, queue string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		q, err := e.ch.QueueDeclarePassive(queue, true, false, false, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		if q.Messages == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s still has %d messages", queue, q.Messages)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e *env) receive(t *testing.T, queue string) amqp.Delivery {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		d, ok, err := e.ch.Get(queue, true)
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

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
