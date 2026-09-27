// Package e2e holds end-to-end tests: the whole system, in-process, against
// real Postgres and RabbitMQ. The HTTP API, outbox relay and update consumer
// run as they do in cmd/controlplane, and the worker as in cmd/worker; the
// tests drive and observe everything through the HTTP API only, as a client
// would (the brief's "Setup Guide" scenario).
package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ankitwasnik/tenant-service/internal/api"
	"github.com/Ankitwasnik/tenant-service/internal/consumer"
	"github.com/Ankitwasnik/tenant-service/internal/messaging"
	"github.com/Ankitwasnik/tenant-service/internal/outbox"
	"github.com/Ankitwasnik/tenant-service/internal/store"
	"github.com/Ankitwasnik/tenant-service/internal/testutil"
	"github.com/Ankitwasnik/tenant-service/internal/worker"
)

// Fast worker: the lifecycle takes milliseconds, not seconds.
var (
	succeed = worker.Config{MinDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond, FailRate: 0}
	fail    = worker.Config{MinDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond, FailRate: 1}
)

// Every tenant transition that has a success path: deploy, update, destroy.
func TestLifecycleHappyPath(t *testing.T) {
	t.Parallel()
	s := startStack(t, succeed)

	// Create: provisioning → active, driven by the worker with no manual steps.
	created := s.create(t, "acme", "Acme Corp")
	if created.Tenant.Status != "provisioning" || created.Task.Status != "accepted" {
		t.Fatalf("create returned %s / %s", created.Tenant.Status, created.Task.Status)
	}
	id := created.Tenant.ID
	tn := s.waitFor(t, id, "active")
	if tn.Version != 2 {
		t.Errorf("after deploy: version %d, want 2", tn.Version)
	}
	s.assertTask(t, created.Task.ID, "done", "")

	// Update: active → updating → active, with the new name.
	patched := s.mutate(t, http.MethodPatch, "/v1/tenants/"+id, `{"name":"Acme Renamed","version":2}`)
	if patched.Tenant.Status != "updating" {
		t.Fatalf("PATCH returned %s", patched.Tenant.Status)
	}
	tn = s.waitFor(t, id, "active")
	if tn.Name != "Acme Renamed" || tn.Version != 4 {
		t.Errorf("after update: %q v%d, want \"Acme Renamed\" v4", tn.Name, tn.Version)
	}
	s.assertTask(t, patched.Task.ID, "done", "")

	// Destroy: active → destroying → destroyed; the tenant stays readable.
	deleted := s.mutate(t, http.MethodDelete, "/v1/tenants/"+id, "")
	if deleted.Tenant.Status != "destroying" {
		t.Fatalf("DELETE returned %s", deleted.Tenant.Status)
	}
	tn = s.waitFor(t, id, "destroyed")
	if tn.Version != 6 {
		t.Errorf("after destroy: version %d, want 6", tn.Version)
	}

	// The history, through the task list: three tasks, newest first, all done.
	tasks := s.tasks(t, id)
	if len(tasks) != 3 || tasks[0].Type != "destroy" || tasks[1].Type != "update" || tasks[2].Type != "deploy" {
		t.Fatalf("tasks = %+v, want destroy, update, deploy", tasks)
	}
	for _, task := range tasks {
		if task.Status != "done" {
			t.Errorf("%s task is %s, want done", task.Type, task.Status)
		}
	}
}

// Failure scenarios, as reproduced in the README: restart the worker with
// --fail-rate=1, then back to 0.
func TestLifecycleFailureAndRecovery(t *testing.T) {
	t.Parallel()
	s := startStack(t, fail)

	// A failed deploy: provisioning → failed, and the task carries the error.
	created := s.create(t, "doomed", "Doomed")
	id := created.Tenant.ID
	s.waitFor(t, id, "failed")
	s.assertTask(t, created.Task.ID, "failed", worker.FailureMessage)

	// A failed tenant can only be deleted. With the worker still failing, the
	// destroy fails too: failed → destroying → failed.
	firstDelete := s.mutate(t, http.MethodDelete, "/v1/tenants/"+id, "")
	s.waitFor(t, id, "failed")
	s.assertTask(t, firstDelete.Task.ID, "failed", worker.FailureMessage)

	// A PATCH on a failed tenant is refused.
	if status, body := s.do(t, http.MethodPatch, "/v1/tenants/"+id, `{"name":"X","version":4}`); status != http.StatusConflict || !strings.Contains(body, "tenant_update_not_allowed") {
		t.Errorf("PATCH on a failed tenant: %d %s", status, body)
	}

	// Fix the worker, delete again: this time it is destroyed.
	s.restartWorker(t, succeed)
	secondDelete := s.mutate(t, http.MethodDelete, "/v1/tenants/"+id, "")
	tn := s.waitFor(t, id, "destroyed")
	s.assertTask(t, secondDelete.Task.ID, "done", "")
	if tn.Version != 6 { // create 1, deploy failed 2, delete 3, destroy failed 4, delete 5, destroyed 6
		t.Errorf("version = %d, want 6", tn.Version)
	}

	// A destroyed tenant's slug stays taken.
	if status, body := s.do(t, http.MethodPost, "/v1/tenants", `{"slug":"doomed","name":"Again"}`); status != http.StatusConflict || !strings.Contains(body, "tenant_already_exists") {
		t.Errorf("re-create with a destroyed slug: %d %s", status, body)
	}
}

// Several tenants at once, so the relay batches, the worker runs tasks in
// parallel, and the consumer's goroutines interleave updates.
func TestLifecycleManyTenantsConcurrently(t *testing.T) {
	t.Parallel()
	s := startStack(t, succeed)

	const n = 20
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids[i] = s.create(t, fmt.Sprintf("bulk-%d", i), "Bulk").Tenant.ID
		}()
	}
	wg.Wait()

	for _, id := range ids {
		if tn := s.waitFor(t, id, "active"); tn.Version != 2 {
			t.Errorf("tenant %s: version %d, want 2", id, tn.Version)
		}
	}
}

// ---- the stack ----

type stack struct {
	api    *httptest.Server
	amqp   string
	logger *slog.Logger

	mu         sync.Mutex
	stopWorker func()
}

// startStack starts the control plane (API, relay, consumer) and a worker
// with cfg, on a fresh database and vhost. Everything stops at cleanup.
func startStack(t *testing.T, cfg worker.Config) *stack {
	t.Helper()
	ctx := context.Background()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	pool, err := store.Open(ctx, testutil.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repo := store.New(pool)

	s := &stack{amqp: testutil.NewVhost(t), logger: logger}
	cpConn, err := messaging.Dial(s.amqp)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cpConn.Close() })
	publisher, err := outbox.NewAMQPPublisher(cpConn, outbox.DefaultConfirmTimeout)
	if err != nil {
		t.Fatal(err)
	}

	relay := outbox.NewRelay(repo, publisher, logger, outbox.Config{PollInterval: 10 * time.Millisecond})
	updates := consumer.New(repo, logger, consumer.Config{})
	run(t, "relay", relay.Run)
	run(t, "consumer", func(ctx context.Context) error { return updates.Run(ctx, cpConn) })

	s.api = httptest.NewServer(api.NewRouter(api.Deps{Logger: logger, DB: pool, Store: repo}))
	t.Cleanup(s.api.Close)

	s.restartWorker(t, cfg)
	t.Cleanup(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.stopWorker()
	})
	return s
}

// restartWorker stops the running worker, if any, and starts one with cfg,
// on its own connection, as `make worker ARGS=...` does.
func (s *stack) restartWorker(t *testing.T, cfg worker.Config) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopWorker != nil {
		s.stopWorker()
	}
	conn, err := messaging.Dial(s.amqp)
	if err != nil {
		t.Fatal(err)
	}
	stop := start(t, "worker", func(ctx context.Context) error { return worker.Run(ctx, conn, cfg, s.logger) })
	s.stopWorker = func() {
		stop()
		_ = conn.Close()
	}
}

// run starts fn in a goroutine until cleanup.
func run(t *testing.T, name string, fn func(context.Context) error) {
	t.Helper()
	t.Cleanup(start(t, name, fn))
}

// start runs fn in a goroutine and returns a func that cancels it and checks
// it stopped cleanly.
func start(t *testing.T, name string, fn func(context.Context) error) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()
	return func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("%s returned %v", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("%s did not stop after cancel", name)
		}
	}
}

// ---- the client ----

type tenantJSON struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	Version int    `json:"version"`
}

type taskJSON struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Status string `json:"status"`
	Error  string `json:"error"`
}

type mutationJSON struct {
	Tenant tenantJSON `json:"tenant"`
	Task   taskJSON   `json:"task"`
}

func (s *stack) do(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, s.api.URL+path, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.api.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func (s *stack) create(t *testing.T, slug, name string) mutationJSON {
	t.Helper()
	status, body := s.do(t, http.MethodPost, "/v1/tenants", `{"slug":"`+slug+`","name":"`+name+`"}`)
	if status != http.StatusCreated {
		t.Fatalf("create %s: %d %s", slug, status, body)
	}
	return decode[mutationJSON](t, body)
}

// mutate sends a PATCH or DELETE that must be accepted (202).
func (s *stack) mutate(t *testing.T, method, path, body string) mutationJSON {
	t.Helper()
	status, raw := s.do(t, method, path, body)
	if status != http.StatusAccepted {
		t.Fatalf("%s %s: %d %s", method, path, status, raw)
	}
	return decode[mutationJSON](t, raw)
}

// waitFor polls GET /v1/tenants/:id until the tenant has status want, and
// fails the test if it doesn't get there in time.
func (s *stack) waitFor(t *testing.T, id, want string) tenantJSON {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last tenantJSON
	for time.Now().Before(deadline) {
		status, body := s.do(t, http.MethodGet, "/v1/tenants/"+id, "")
		if status == http.StatusOK {
			last = decode[tenantJSON](t, body)
			if last.Status == want {
				return last
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("tenant %s is %s after 15s, want %s", id, last.Status, want)
	return last
}

func (s *stack) assertTask(t *testing.T, id, status, errMsg string) {
	t.Helper()
	code, body := s.do(t, http.MethodGet, "/v1/tasks/"+id, "")
	if code != http.StatusOK {
		t.Fatalf("GET task %s: %d %s", id, code, body)
	}
	task := decode[taskJSON](t, body)
	if task.Status != status || task.Error != errMsg {
		t.Errorf("%s task = %s %q, want %s %q", task.Type, task.Status, task.Error, status, errMsg)
	}
}

func (s *stack) tasks(t *testing.T, tenantID string) []taskJSON {
	t.Helper()
	code, body := s.do(t, http.MethodGet, "/v1/tasks?tenant_id="+tenantID, "")
	if code != http.StatusOK {
		t.Fatalf("list tasks: %d %s", code, body)
	}
	return decode[struct {
		Items []taskJSON `json:"items"`
	}](t, body).Items
}

func decode[T any](t *testing.T, body string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("decode %T: %v: %s", v, err, body)
	}
	return v
}
