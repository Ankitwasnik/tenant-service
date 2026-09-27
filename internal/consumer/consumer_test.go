package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/Ankitwasnik/tenant-service/internal/domain"
	"github.com/Ankitwasnik/tenant-service/internal/messaging"
	"github.com/Ankitwasnik/tenant-service/internal/store"
)

// Unit tests: a scripted fake store and a fake acknowledger sharing one log.

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

// fakeStore returns the scripted results in turn, then repeats the last one.
type fakeStore struct {
	log     *eventLog
	script  []outcome
	onApply func(call int) // optional hook, e.g. to cancel the context
	calls   int
}

type outcome struct {
	result store.UpdateResult
	err    error
	panic  bool
}

func (f *fakeStore) ApplyUpdate(context.Context, messaging.TaskUpdate) (store.UpdateResult, error) {
	f.calls++
	f.log.add("apply")
	if f.onApply != nil {
		f.onApply(f.calls)
	}
	o := f.script[min(f.calls, len(f.script))-1]
	if o.panic {
		panic("boom in the store")
	}
	return o.result, o.err
}

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

var errTransient = fmt.Errorf("query: %w", &pgconn.PgError{Code: "08006"}) // connection failure

func newTestConsumer(log *eventLog, script ...outcome) (*Consumer, *fakeStore) {
	fs := &fakeStore{log: log, script: script}
	c := New(fs, slog.New(slog.NewJSONHandler(io.Discard, nil)), Config{MinBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond})
	return c, fs
}

func validDelivery(t *testing.T, log *eventLog) amqp.Delivery {
	t.Helper()
	body, err := json.Marshal(messaging.NewTaskUpdate(uuid.New(), domain.TaskDone, ""))
	if err != nil {
		t.Fatal(err)
	}
	return amqp.Delivery{Acknowledger: fakeAck{log: log}, Body: body}
}

func TestProcessedUpdatesAreAcked(t *testing.T) {
	// A duplicate and a stale update change nothing, and aren't failures either.
	for _, result := range []store.UpdateResult{store.UpdateApplied, store.UpdateDuplicate, store.UpdateStale} {
		t.Run(string(result), func(t *testing.T) {
			log := &eventLog{}
			c, _ := newTestConsumer(log, outcome{result: result})
			if err := c.Handle(context.Background(), validDelivery(t, log)); err != nil {
				t.Fatal(err)
			}
			assertEvents(t, log, "apply", "ack")
		})
	}
}

func TestTransientErrorIsRetriedThenAcked(t *testing.T) {
	log := &eventLog{}
	c, fs := newTestConsumer(log,
		outcome{err: errTransient},
		outcome{err: errTransient},
		outcome{result: store.UpdateApplied},
	)
	if err := c.Handle(context.Background(), validDelivery(t, log)); err != nil {
		t.Fatal(err)
	}
	// Held and retried in-process: no nack in between, and the ack only after success.
	assertEvents(t, log, "apply", "apply", "apply", "ack")
	if fs.calls != 3 {
		t.Errorf("ApplyUpdate called %d times, want 3", fs.calls)
	}
}

func TestPermanentErrorIsDeadLetteredAtOnce(t *testing.T) {
	for name, err := range map[string]error{
		"unknown task":     fmt.Errorf("update: %w", store.ErrTaskNotFound),
		"broken invariant": fmt.Errorf("update: %w", store.ErrInvariant),
		"check violation":  &pgconn.PgError{Code: "23514"},
		"plain error":      errors.New("something unexpected"),
	} {
		t.Run(name, func(t *testing.T) {
			log := &eventLog{}
			c, _ := newTestConsumer(log, outcome{err: err})
			if err := c.Handle(context.Background(), validDelivery(t, log)); err != nil {
				t.Fatal(err)
			}
			assertEvents(t, log, "apply", "nack dead-letter")
		})
	}
}

func TestPanicIsRecoveredAndDeadLettered(t *testing.T) {
	log := &eventLog{}
	c, _ := newTestConsumer(log, outcome{panic: true})
	if err := c.Handle(context.Background(), validDelivery(t, log)); err != nil {
		t.Fatal(err)
	}
	assertEvents(t, log, "apply", "nack dead-letter")

	// The consumer is still usable: the next message is handled normally.
	c.store = &fakeStore{log: log, script: []outcome{{result: store.UpdateApplied}}}
	if err := c.Handle(context.Background(), validDelivery(t, log)); err != nil {
		t.Fatal(err)
	}
	assertEvents(t, log, "apply", "nack dead-letter", "apply", "ack")
}

func TestMalformedUpdateIsDeadLetteredUnapplied(t *testing.T) {
	for name, body := range map[string]string{
		"not JSON": `nope`,
		"accepted": `{"update_id":"` + uuid.NewString() + `","task_id":"` + uuid.NewString() + `","status":"accepted"}`,
		"missing":  `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			log := &eventLog{}
			c, _ := newTestConsumer(log, outcome{result: store.UpdateApplied})
			d := amqp.Delivery{Acknowledger: fakeAck{log: log}, Body: []byte(body)}
			if err := c.Handle(context.Background(), d); err != nil {
				t.Fatal(err)
			}
			assertEvents(t, log, "nack dead-letter") // never reached the store
		})
	}
}

func TestShutdownDuringRetryRequeues(t *testing.T) {
	log := &eventLog{}
	ctx, cancel := context.WithCancel(context.Background())
	c, fs := newTestConsumer(log, outcome{err: errTransient})
	fs.onApply = func(call int) {
		if call == 2 {
			cancel() // shutdown arrives while the database is still down
		}
	}
	if err := c.Handle(ctx, validDelivery(t, log)); err != nil {
		t.Fatal(err)
	}
	assertEvents(t, log, "apply", "apply", "nack requeue")
}

func TestDefaults(t *testing.T) {
	cfg := Config{}.withDefaults()
	if cfg.Prefetch != 16 || cfg.Handlers != 4 || cfg.MinBackoff != 100*time.Millisecond || cfg.MaxBackoff != 30*time.Second {
		t.Fatalf("defaults = %+v", cfg)
	}
}

func assertEvents(t *testing.T, log *eventLog, want ...string) {
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
