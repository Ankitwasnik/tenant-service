package messaging_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/Ankitwasnik/tenant-service/internal/messaging"
	"github.com/Ankitwasnik/tenant-service/internal/testutil"
)

// The broker force-closes the connection (as a restart or a network cut
// would): Run notices, redials, and starts the session again on the new one.
func TestRunReconnectsAfterBrokerClosesConnection(t *testing.T) {
	t.Parallel()
	url := testutil.NewVhost(t)
	r := startRun(t, url, nil)

	first := r.waitForSession(t, 1)
	if n := testutil.ManagementFor(t, url).CloseConnections(t); n < 1 {
		t.Fatalf("closed %d connections", n)
	}
	second := r.waitForSession(t, 2)

	if first == second {
		t.Fatal("the second session got the same connection")
	}
	if !first.IsClosed() || second.IsClosed() {
		t.Errorf("first closed=%v, second closed=%v; want the old one closed and the new one open", first.IsClosed(), second.IsClosed())
	}
	if !r.sessionCtxDone(1) {
		t.Error("the first session's context wasn't cancelled when its connection was lost")
	}
}

// A queue deleted while disconnected is back before the next session runs.
func TestRunRedeclaresTopologyOnEveryConnect(t *testing.T) {
	t.Parallel()
	url := testutil.NewVhost(t)
	r := startRun(t, url, nil)

	conn := r.waitForSession(t, 1)
	ch, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ch.QueueDelete(messaging.WorkerTasksQueue, false, false, false); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close() // drop the connection: Run reconnects

	conn = r.waitForSession(t, 2)
	ch, err = conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ch.QueueDeclarePassive(messaging.WorkerTasksQueue, true, false, false, false, nil); err != nil {
		t.Fatalf("worker.tasks not re-declared on reconnect: %v", err)
	}
}

// A session that ends with an error while the connection is still up (say,
// its channel was closed by the broker) also gets a fresh connection.
func TestRunRestartsASessionThatFails(t *testing.T) {
	t.Parallel()
	url := testutil.NewVhost(t)
	var calls atomic.Int32
	r := startRun(t, url, func(ctx context.Context, _ *amqp.Connection) error {
		if calls.Add(1) == 1 {
			return errors.New("channel closed by the broker")
		}
		<-ctx.Done()
		return nil
	})
	r.waitForSession(t, 2)
}

// The broker isn't reachable (not up yet, say): dialing is retried with a
// growing backoff, and the session starts once a dial succeeds.
func TestRunRetriesFailedDialsWithGrowingBackoff(t *testing.T) {
	t.Parallel()
	url := testutil.NewVhost(t)

	var mu sync.Mutex
	var attempts []time.Time
	dial := func(u string) (*amqp.Connection, error) {
		mu.Lock()
		attempts = append(attempts, time.Now())
		n := len(attempts)
		mu.Unlock()
		if n <= 3 {
			return nil, errors.New("connection refused")
		}
		return messaging.Dial(u)
	}
	r := startRunWith(t, messaging.ReconnectConfig{
		URL: url, Dial: dial, MinBackoff: 40 * time.Millisecond, MaxBackoff: time.Second,
	}, nil)
	r.waitForSession(t, 1)

	mu.Lock()
	defer mu.Unlock()
	if len(attempts) != 4 {
		t.Fatalf("%d dial attempts, want 4 (3 failures, then success)", len(attempts))
	}
	// Waits are jittered into [d/2, d] for d = 40, 80, 160ms, so each gap is
	// at least 20, 40 and 80ms: the lower bounds double.
	for i, min := range []time.Duration{20, 40, 80} {
		if gap := attempts[i+1].Sub(attempts[i]); gap < min*time.Millisecond {
			t.Errorf("wait %d = %v, want at least %v", i+1, gap, min*time.Millisecond)
		}
	}
}

func TestRunStopsWhenCancelledWhileRetrying(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- messaging.Run(ctx, messaging.ReconnectConfig{
			URL:        "amqp://unused",
			Logger:     discard(),
			Dial:       func(string) (*amqp.Connection, error) { return nil, errors.New("connection refused") },
			MinBackoff: time.Hour, // it would wait an hour; cancel must cut that short
		}, func(context.Context, *amqp.Connection) error { return nil })
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v after cancel, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
}

// ---- harness ----

// runner records each session: its connection, and whether its context was
// cancelled when it ended.
type runner struct {
	mu       sync.Mutex
	conns    []*amqp.Connection
	ctxDone  []bool
	sessions chan struct{}
}

func startRun(t *testing.T, url string, session messaging.Session) *runner {
	t.Helper()
	return startRunWith(t, messaging.ReconnectConfig{URL: url, MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond}, session)
}

func startRunWith(t *testing.T, cfg messaging.ReconnectConfig, session messaging.Session) *runner {
	t.Helper()
	cfg.Logger = discard()
	r := &runner{sessions: make(chan struct{}, 100)}
	wrapped := func(ctx context.Context, conn *amqp.Connection) error {
		r.mu.Lock()
		r.conns = append(r.conns, conn)
		r.ctxDone = append(r.ctxDone, false)
		i := len(r.conns) - 1
		r.mu.Unlock()
		r.sessions <- struct{}{}

		var err error
		if session != nil {
			err = session(ctx, conn)
		} else {
			<-ctx.Done() // a long-running session, like the consumer
		}
		r.mu.Lock()
		r.ctxDone[i] = ctx.Err() != nil
		r.mu.Unlock()
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- messaging.Run(ctx, cfg, wrapped) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Run did not stop after cancel")
		}
	})
	return r
}

// waitForSession waits until session n (1-based) has started and returns its connection.
func (r *runner) waitForSession(t *testing.T, n int) *amqp.Connection {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		r.mu.Lock()
		if len(r.conns) >= n {
			conn := r.conns[n-1]
			r.mu.Unlock()
			return conn
		}
		r.mu.Unlock()
		select {
		case <-r.sessions:
		case <-deadline:
			t.Fatalf("session %d never started", n)
		}
	}
}

func (r *runner) sessionCtxDone(n int) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		done := r.ctxDone[n-1]
		r.mu.Unlock()
		if done {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func discard() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}
