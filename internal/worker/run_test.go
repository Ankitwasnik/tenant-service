package worker_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/Ankitwasnik/tenant-service/internal/domain"
	"github.com/Ankitwasnik/tenant-service/internal/messaging"
	"github.com/Ankitwasnik/tenant-service/internal/testutil"
	"github.com/Ankitwasnik/tenant-service/internal/worker"
)

// Integration: the real worker against a real RabbitMQ (one vhost per test).

func TestRunProcessesTaskEndToEnd(t *testing.T) {
	t.Parallel()
	conn, ch, stop := startWorker(t, worker.Config{MinDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond, FailRate: 0})

	event := messaging.TaskEvent{ID: uuid.New(), TenantID: uuid.New(), Type: domain.TaskDeploy, Status: domain.TaskAccepted}
	publishTask(t, conn, event)

	// Two updates, in order, each with its deterministic id as message_id.
	for _, status := range []domain.TaskStatus{domain.TaskInProgress, domain.TaskDone} {
		d := receive(t, ch, messaging.ControlPlaneUpdateQueue)
		u, err := messaging.DecodeTaskUpdate(d.Body)
		if err != nil {
			t.Fatalf("decode update: %v: %s", err, d.Body)
		}
		wantID := messaging.UpdateID(event.ID, status)
		if u.Status != status || u.TaskID != event.ID || u.UpdateID != wantID {
			t.Errorf("update = %+v, want %s for task %s with id %s", u, status, event.ID, wantID)
		}
		if d.MessageId != wantID.String() || d.Type != messaging.TaskUpdateType || d.RoutingKey != "task.update."+string(status) {
			t.Errorf("delivery: message_id %s type %s routing key %s", d.MessageId, d.Type, d.RoutingKey)
		}
	}

	// The task was acked: once the worker stops, nothing comes back.
	stop()
	assertQueueEmpty(t, ch, messaging.WorkerTasksQueue)
}

func TestRunDeadLettersMalformedTask(t *testing.T) {
	t.Parallel()
	conn, ch, _ := startWorker(t, worker.Config{})

	pub, err := messaging.NewConfirmPublisher(conn, messaging.TasksExchange, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pub.Publish(context.Background(), []messaging.Message{{RoutingKey: "task.deploy", MessageID: "poison", Body: []byte("not json")}}); err != nil {
		t.Fatal(err)
	}

	if d := receive(t, ch, messaging.DeadLetterQueue(messaging.WorkerTasksQueue)); string(d.Body) != "not json" {
		t.Errorf("DLQ got %q", d.Body)
	}
	assertQueueEmpty(t, ch, messaging.ControlPlaneUpdateQueue)
}

// ---- helpers ----

// startWorker runs worker.Run on a fresh vhost and returns a connection and a
// channel for the test's own publishing and reading, plus a func that stops
// the worker and waits for it (also run at cleanup).
func startWorker(t *testing.T, cfg worker.Config) (*amqp.Connection, *amqp.Channel, func()) {
	t.Helper()
	url := testutil.NewVhost(t)

	workerConn, err := messaging.Dial(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = workerConn.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- worker.Run(ctx, workerConn, cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	}()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("worker.Run returned %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("worker did not stop after cancel")
		}
	}
	t.Cleanup(stop)

	conn, err := messaging.Dial(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ch, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	if err := messaging.DeclareTopology(ch); err != nil {
		t.Fatal(err)
	}
	return conn, ch, stop
}

func publishTask(t *testing.T, conn *amqp.Connection, e messaging.TaskEvent) {
	t.Helper()
	pub, err := messaging.NewConfirmPublisher(conn, messaging.TasksExchange, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	body, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pub.Publish(context.Background(), []messaging.Message{{
		RoutingKey: messaging.TaskRoutingKey(e.Type), MessageID: e.ID.String(), Type: messaging.TaskEventType, Body: body,
	}}); err != nil {
		t.Fatal(err)
	}
}

func receive(t *testing.T, ch *amqp.Channel, queue string) amqp.Delivery {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
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

func assertQueueEmpty(t *testing.T, ch *amqp.Channel, queue string) {
	t.Helper()
	q, err := ch.QueueDeclarePassive(queue, true, false, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if q.Messages != 0 {
		t.Errorf("%s has %d messages, want 0", queue, q.Messages)
	}
}
