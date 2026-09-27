package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/Ankitwasnik/tenant-service/internal/domain"
	"github.com/Ankitwasnik/tenant-service/internal/messaging"
)

// Unit tests: fake publisher and a fake acknowledger that write to one shared
// log, so the order of publishes and the ack is visible.

type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *eventLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

type fakePublisher struct {
	log     *eventLog
	failOn  domain.TaskStatus // this status fails to publish
	updates []messaging.TaskUpdate
}

func (f *fakePublisher) PublishUpdate(_ context.Context, u messaging.TaskUpdate) error {
	if u.Status == f.failOn {
		return errors.New("broker unreachable")
	}
	f.updates = append(f.updates, u)
	f.log.add("publish " + string(u.Status))
	return nil
}

// fakeAck implements amqp.Acknowledger.
type fakeAck struct{ log *eventLog }

func (a fakeAck) Ack(uint64, bool) error { a.log.add("ack"); return nil }
func (a fakeAck) Nack(_ uint64, _ bool, requeue bool) error {
	if requeue {
		a.log.add("nack requeue")
	} else {
		a.log.add("nack dead-letter")
	}
	return nil
}
func (a fakeAck) Reject(uint64, bool) error { a.log.add("reject"); return nil }

func newTestProcessor(cfg Config, log *eventLog, random float64) (*Processor, *fakePublisher) {
	pub := &fakePublisher{log: log}
	p := NewProcessor(cfg, pub, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	p.random = func() float64 { return random }
	p.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	return p, pub
}

func delivery(t *testing.T, log *eventLog, e messaging.TaskEvent) amqp.Delivery {
	t.Helper()
	body, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return amqp.Delivery{Acknowledger: fakeAck{log: log}, DeliveryTag: 1, Body: body}
}

func taskEvent() messaging.TaskEvent {
	return messaging.TaskEvent{ID: uuid.New(), TenantID: uuid.New(), Type: domain.TaskDeploy, Status: domain.TaskAccepted}
}

func TestHandleDeliverySuccessAcksAfterBothPublishes(t *testing.T) {
	log := &eventLog{}
	p, pub := newTestProcessor(Config{FailRate: 0}, log, 0.999)
	e := taskEvent()

	if err := p.HandleDelivery(context.Background(), delivery(t, log, e)); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}

	want := []string{"publish in_progress", "publish done", "ack"}
	assertEvents(t, log, want)
	// Deterministic ids: a second run of this task would produce the same ones.
	if pub.updates[0].UpdateID != messaging.UpdateID(e.ID, domain.TaskInProgress) ||
		pub.updates[1].UpdateID != messaging.UpdateID(e.ID, domain.TaskDone) {
		t.Errorf("update ids = %s, %s: not the deterministic ones", pub.updates[0].UpdateID, pub.updates[1].UpdateID)
	}
	if pub.updates[1].Error != "" {
		t.Errorf("done update carries error %q", pub.updates[1].Error)
	}
}

func TestFailRateOneAlwaysFails(t *testing.T) {
	log := &eventLog{}
	p, pub := newTestProcessor(Config{FailRate: 1}, log, 0.999) // the highest random value still fails
	if err := p.HandleDelivery(context.Background(), delivery(t, log, taskEvent())); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}
	assertEvents(t, log, []string{"publish in_progress", "publish failed", "ack"})
	if pub.updates[1].Error != FailureMessage {
		t.Errorf("failed update error = %q", pub.updates[1].Error)
	}
}

func TestFailRateZeroNeverFails(t *testing.T) {
	log := &eventLog{}
	p, _ := newTestProcessor(Config{FailRate: 0}, log, 0) // the lowest random value still succeeds
	if err := p.HandleDelivery(context.Background(), delivery(t, log, taskEvent())); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}
	assertEvents(t, log, []string{"publish in_progress", "publish done", "ack"})
}

func TestFailRateThreshold(t *testing.T) {
	for _, tt := range []struct {
		random float64
		want   string
	}{
		{0.29, "publish failed"},
		{0.30, "publish done"},
	} {
		log := &eventLog{}
		p, _ := newTestProcessor(Config{FailRate: 0.3}, log, tt.random)
		if err := p.HandleDelivery(context.Background(), delivery(t, log, taskEvent())); err != nil {
			t.Fatal(err)
		}
		if got := log.get()[1]; got != tt.want {
			t.Errorf("random %.2f with fail rate 0.3: %s, want %s", tt.random, got, tt.want)
		}
	}
}

func TestMalformedTaskIsDeadLettered(t *testing.T) {
	for name, body := range map[string]string{
		"not JSON":       `nope`,
		"missing id":     `{"type":"deploy","tenant_id":"` + uuid.NewString() + `","status":"accepted"}`,
		"unknown type":   `{"id":"` + uuid.NewString() + `","type":"reboot","tenant_id":"` + uuid.NewString() + `","status":"accepted"}`,
		"not accepted":   `{"id":"` + uuid.NewString() + `","type":"deploy","tenant_id":"` + uuid.NewString() + `","status":"done"}`,
		"missing tenant": `{"id":"` + uuid.NewString() + `","type":"deploy","status":"accepted"}`,
	} {
		t.Run(name, func(t *testing.T) {
			log := &eventLog{}
			p, _ := newTestProcessor(Config{}, log, 0)
			d := amqp.Delivery{Acknowledger: fakeAck{log: log}, Body: []byte(body)}
			if err := p.HandleDelivery(context.Background(), d); err != nil {
				t.Fatalf("HandleDelivery: %v", err)
			}
			assertEvents(t, log, []string{"nack dead-letter"})
		})
	}
}

func TestShutdownMidTaskRequeues(t *testing.T) {
	log := &eventLog{}
	p, _ := newTestProcessor(Config{}, log, 0)
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	p.sleep = func(ctx context.Context, _ time.Duration) error {
		calls++
		if calls == 2 { // shutdown arrives during the second delay
			cancel()
		}
		return ctx.Err()
	}

	if err := p.HandleDelivery(ctx, delivery(t, log, taskEvent())); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}
	// No terminal update, and the task goes back to the queue for the next worker.
	assertEvents(t, log, []string{"publish in_progress", "nack requeue"})
}

func TestPublishFailureLeavesDeliveryUnsettled(t *testing.T) {
	log := &eventLog{}
	p, pub := newTestProcessor(Config{}, log, 0)
	pub.failOn = domain.TaskDone

	if err := p.HandleDelivery(context.Background(), delivery(t, log, taskEvent())); err == nil {
		t.Fatal("HandleDelivery succeeded although the publish failed")
	}
	// Neither ack nor nack: the worker stops, and RabbitMQ redelivers the task.
	assertEvents(t, log, []string{"publish in_progress"})
}

func TestDelayWithinBounds(t *testing.T) {
	cfg := Config{MinDelay: 100 * time.Millisecond, MaxDelay: 300 * time.Millisecond}
	for _, r := range []float64{0, 0.5, 0.999999} {
		p, _ := newTestProcessor(cfg, &eventLog{}, r)
		if d := p.delay(); d < cfg.MinDelay || d > cfg.MaxDelay {
			t.Errorf("random %v: delay %v outside [%v, %v]", r, d, cfg.MinDelay, cfg.MaxDelay)
		}
	}
	fixed, _ := newTestProcessor(Config{MinDelay: 50 * time.Millisecond, MaxDelay: 50 * time.Millisecond}, &eventLog{}, 0.7)
	if d := fixed.delay(); d != 50*time.Millisecond {
		t.Errorf("min == max: delay %v, want 50ms", d)
	}
}

func TestConfigValidate(t *testing.T) {
	for _, tt := range []struct {
		name  string
		cfg   Config
		valid bool
	}{
		{"defaults", Config{MinDelay: 500 * time.Millisecond, MaxDelay: 2 * time.Second}, true},
		{"zero delays", Config{}, true},
		{"min equals max", Config{MinDelay: time.Second, MaxDelay: time.Second}, true},
		{"fail rate 1", Config{FailRate: 1}, true},
		{"min above max", Config{MinDelay: 2 * time.Second, MaxDelay: time.Second}, false},
		{"negative min", Config{MinDelay: -time.Millisecond}, false},
		{"fail rate above 1", Config{FailRate: 1.01}, false},
		{"negative fail rate", Config{FailRate: -0.1}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg.Validate(); (err == nil) != tt.valid {
				t.Fatalf("Validate() = %v, want valid=%v", err, tt.valid)
			}
		})
	}
}

func assertEvents(t *testing.T, log *eventLog, want []string) {
	t.Helper()
	got := log.get()
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events = %v, want %v", got, want)
		}
	}
}
