package outbox_test

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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Ankitwasnik/tenant-service/internal/outbox"
	"github.com/Ankitwasnik/tenant-service/internal/store"
	"github.com/Ankitwasnik/tenant-service/internal/testutil"
)

// Relay tests: real Postgres (one database per test), fake publisher, so
// broker failures can be scripted exactly.

func TestStepPublishesCommittedEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, pool := newRepo(t)
	tasks := createTenants(t, repo, "t-one", "t-two", "t-three")
	pub := &fakePublisher{}
	relay := newRelay(repo, pub)

	claimed, marked, err := relay.Step(ctx)
	if err != nil || claimed != 3 || marked != 3 {
		t.Fatalf("Step = (%d, %d, %v), want (3, 3, nil)", claimed, marked, err)
	}

	// Published in commit (outbox id) order, carrying the task event.
	got := pub.all()
	if len(got) != 3 {
		t.Fatalf("published %d messages, want 3", len(got))
	}
	for i, m := range got {
		if m.TaskID != tasks[i] || m.RoutingKey != "task.deploy" {
			t.Errorf("message %d = task %s key %q, want task %s key task.deploy", i, m.TaskID, m.RoutingKey, tasks[i])
		}
		var event map[string]string
		if err := json.Unmarshal(m.Payload, &event); err != nil || event["id"] != tasks[i].String() || event["status"] != "accepted" {
			t.Errorf("message %d payload = %s (%v)", i, m.Payload, err)
		}
	}
	assertUnpublished(t, pool, 0)

	// Nothing is sent twice.
	if claimed, marked, err := relay.Step(ctx); claimed != 0 || marked != 0 || err != nil {
		t.Fatalf("second Step = (%d, %d, %v), want nothing to do", claimed, marked, err)
	}
	if n := len(pub.all()); n != 3 {
		t.Fatalf("published %d messages in total, want 3", n)
	}
}

func TestStepBrokerDownKeepsRowsForRetry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, pool := newRepo(t)
	createTenants(t, repo, "t-one", "t-two")
	pub := &fakePublisher{fail: errors.New("broker unreachable")}
	relay := newRelay(repo, pub)

	claimed, marked, err := relay.Step(ctx)
	if err == nil || claimed != 2 || marked != 0 {
		t.Fatalf("Step = (%d, %d, %v), want (2, 0, error)", claimed, marked, err)
	}
	assertUnpublished(t, pool, 2)

	// The broker is back: the same rows go out.
	pub.setFail(nil)
	if _, marked, err := relay.Step(ctx); err != nil || marked != 2 {
		t.Fatalf("retry Step marked %d (err %v), want 2", marked, err)
	}
	assertUnpublished(t, pool, 0)
}

func TestStepPartialBatchMarksOnlyConfirmed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, pool := newRepo(t)
	tasks := createTenants(t, repo, "t-one", "t-two", "t-three")
	pub := &fakePublisher{confirmFirst: 2, fail: errors.New("third message nacked")}
	relay := newRelay(repo, pub)

	_, marked, err := relay.Step(ctx)
	if err == nil || marked != 2 {
		t.Fatalf("Step marked %d (err %v), want 2 and an error", marked, err)
	}
	assertUnpublished(t, pool, 1)

	// Only the unconfirmed one is sent again.
	pub.reset()
	if _, marked, err := relay.Step(ctx); err != nil || marked != 1 {
		t.Fatalf("retry Step marked %d (err %v), want 1", marked, err)
	}
	if got := pub.all(); len(got) != 1 || got[0].TaskID != tasks[2] {
		t.Fatalf("retry published %v, want only the third task", got)
	}
}

func TestStepIgnoresIDsOutsideTheBatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, pool := newRepo(t)
	createTenants(t, repo, "t-one")
	relay := newRelay(repo, confirmFunc(func([]store.OutboxMessage) []int64 { return []int64{987654} }))

	if _, marked, err := relay.Step(ctx); err != nil || marked != 0 {
		t.Fatalf("Step marked %d (err %v), want 0", marked, err)
	}
	assertUnpublished(t, pool, 1)
}

func TestStepPublishesOnlyCommittedMutations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, _ := newRepo(t)
	tenant, deploy, err := repo.CreateTenant(ctx, "acme", "Acme")
	if err != nil {
		t.Fatal(err)
	}

	// Rejected (the tenant is still provisioning): no outbox row, so nothing to relay.
	if _, _, err := repo.PatchTenant(ctx, tenant.ID, "Renamed", 1); err == nil {
		t.Fatal("PATCH on a provisioning tenant succeeded")
	}

	pub := &fakePublisher{}
	if _, marked, err := newRelay(repo, pub).Step(ctx); err != nil || marked != 1 {
		t.Fatalf("Step marked %d (err %v), want 1", marked, err)
	}
	if got := pub.all(); len(got) != 1 || got[0].TaskID != deploy.ID {
		t.Fatalf("published %v, want only the deploy task", got)
	}
}

// Several relays (control-plane replicas) on one outbox: SKIP LOCKED gives
// each row to one relay at a time, so nothing is published twice.
func TestConcurrentRelaysPublishEachEventOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, pool := newRepo(t)
	const tenants = 60
	slugs := make([]string, tenants)
	for i := range slugs {
		slugs[i] = "t-" + uuid.NewString()[:8]
	}
	createTenants(t, repo, slugs...)

	pub := &fakePublisher{delay: 20 * time.Millisecond} // holds each batch's locks a while
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			relay := outbox.NewRelay(repo, pub, discardLogger(), outbox.Config{BatchSize: 10})
			for {
				claimed, _, err := relay.Step(ctx)
				if err != nil {
					t.Errorf("Step: %v", err)
					return
				}
				if claimed == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()

	seen := map[int64]int{}
	for _, m := range pub.all() {
		seen[m.ID]++
	}
	if len(seen) != tenants {
		t.Errorf("published %d distinct events, want %d", len(seen), tenants)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("outbox row %d published %d times", id, n)
		}
	}
	assertUnpublished(t, pool, 0)
}

func TestRunBacksOffThenRecovers(t *testing.T) {
	t.Parallel()
	repo, pool := newRepo(t)
	createTenants(t, repo, "t-one", "t-two")

	pub := &fakePublisher{failTimes: 3, fail: errors.New("broker unreachable")}
	relay := outbox.NewRelay(repo, pub, discardLogger(), outbox.Config{
		PollInterval: 5 * time.Millisecond, MinBackoff: 5 * time.Millisecond, MaxBackoff: 20 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- relay.Run(ctx) }()

	waitFor(t, 5*time.Second, func() bool { return unpublished(t, pool) == 0 })
	if attempts := pub.attempts(); attempts < 4 {
		t.Errorf("published after %d attempts, want at least 4 (3 failures, then success)", attempts)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v after cancel, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after its context was cancelled")
	}
}

// ---- fakes and helpers ----

// fakePublisher records what it is asked to publish. By default it confirms
// everything; confirmFirst limits that to the first n of each batch, and fail
// makes it return an error (for failTimes calls, or for every call if 0).
type fakePublisher struct {
	mu           sync.Mutex
	fail         error
	failTimes    int
	confirmFirst int
	delay        time.Duration
	calls        int
	got          []store.OutboxMessage
}

func (f *fakePublisher) Publish(_ context.Context, msgs []store.OutboxMessage) ([]int64, error) {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++

	failing := f.fail != nil && (f.failTimes == 0 || f.calls <= f.failTimes)
	if failing && f.confirmFirst == 0 {
		return nil, f.fail // nothing reached the broker
	}
	n := len(msgs)
	if f.confirmFirst > 0 && f.confirmFirst < n {
		n = f.confirmFirst
	}
	f.got = append(f.got, msgs[:n]...)
	ids := make([]int64, n)
	for i := range n {
		ids[i] = msgs[i].ID
	}
	if failing {
		return ids, f.fail
	}
	return ids, nil
}

func (f *fakePublisher) all() []store.OutboxMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.OutboxMessage(nil), f.got...)
}

func (f *fakePublisher) attempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakePublisher) setFail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = err
}

// reset clears the record and the failure script: the broker is healthy again.
func (f *fakePublisher) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got, f.fail, f.confirmFirst = nil, nil, 0
}

type confirmFunc func([]store.OutboxMessage) []int64

func (f confirmFunc) Publish(_ context.Context, msgs []store.OutboxMessage) ([]int64, error) {
	return f(msgs), nil
}

func newRepo(t *testing.T) (*store.Store, *pgxpool.Pool) {
	t.Helper()
	pool, err := store.Open(context.Background(), testutil.NewDatabase(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(pool.Close)
	return store.New(pool), pool
}

func newRelay(repo *store.Store, pub outbox.Publisher) *outbox.Relay {
	return outbox.NewRelay(repo, pub, discardLogger(), outbox.Config{})
}

// createTenants creates one tenant per slug and returns their deploy tasks' ids.
func createTenants(t *testing.T, repo *store.Store, slugs ...string) []uuid.UUID {
	t.Helper()
	ids := make([]uuid.UUID, len(slugs))
	for i, slug := range slugs {
		_, task, err := repo.CreateTenant(context.Background(), slug, "Tenant "+slug)
		if err != nil {
			t.Fatalf("CreateTenant(%q): %v", slug, err)
		}
		ids[i] = task.ID
	}
	return ids
}

func unpublished(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox WHERE published_at IS NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func assertUnpublished(t *testing.T, pool *pgxpool.Pool, want int) {
	t.Helper()
	if got := unpublished(t, pool); got != want {
		t.Fatalf("%d outbox rows unpublished, want %d", got, want)
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}
