package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Ankitwasnik/tenant-service/internal/domain"
	"github.com/Ankitwasnik/tenant-service/internal/messaging"
	"github.com/Ankitwasnik/tenant-service/internal/store"
	"github.com/Ankitwasnik/tenant-service/internal/testutil"
)

func TestApplyUpdateLifecycle(t *testing.T) {
	t.Parallel()
	s, pool := newRepo(t)
	tenant, task := createWithTask(t, s, "acme")

	// in_progress moves only the task: the tenant row, and so its version, is untouched.
	mustApply(t, s, update(task.ID, domain.TaskInProgress), store.UpdateApplied)
	assertTask(t, s, task.ID, domain.TaskInProgress, "")
	assertTenant(t, s, tenant.ID, domain.TenantProvisioning, 1)

	// done is terminal: the deploy moves the tenant to active and bumps its version.
	mustApply(t, s, update(task.ID, domain.TaskDone), store.UpdateApplied)
	assertTask(t, s, task.ID, domain.TaskDone, "")
	assertTenant(t, s, tenant.ID, domain.TenantActive, 2)
	assertInboxRows(t, pool, 2)
}

func TestApplyUpdateDuplicateChangesNothing(t *testing.T) {
	t.Parallel()
	s, pool := newRepo(t)
	tenant, task := createWithTask(t, s, "acme")
	done := update(task.ID, domain.TaskDone)
	mustApply(t, s, done, store.UpdateApplied)
	before := mustGet(t, s, tenant.ID)
	beforeTask := mustGetTask(t, s, task.ID)

	// The same update delivered again (at-least-once): same update_id.
	mustApply(t, s, done, store.UpdateDuplicate)

	after := mustGet(t, s, tenant.ID)
	if after.Version != before.Version || !after.UpdatedAt.Equal(before.UpdatedAt) || after.Status != before.Status {
		t.Errorf("tenant changed on a duplicate: %+v → %+v", before, after)
	}
	if afterTask := mustGetTask(t, s, task.ID); !afterTask.UpdatedAt.Equal(beforeTask.UpdatedAt) {
		t.Errorf("task updated_at changed on a duplicate")
	}
	assertInboxRows(t, pool, 1)
}

func TestApplyUpdateOutOfOrder(t *testing.T) {
	t.Parallel()
	s, _ := newRepo(t)

	t.Run("in_progress after done is stale", func(t *testing.T) {
		tenant, task := createWithTask(t, s, "late-progress")
		mustApply(t, s, update(task.ID, domain.TaskDone), store.UpdateApplied)
		mustApply(t, s, update(task.ID, domain.TaskInProgress), store.UpdateStale)
		assertTask(t, s, task.ID, domain.TaskDone, "")
		assertTenant(t, s, tenant.ID, domain.TenantActive, 2)
	})

	t.Run("accepted to done jump is applied", func(t *testing.T) {
		tenant, task := createWithTask(t, s, "jump")
		mustApply(t, s, update(task.ID, domain.TaskDone), store.UpdateApplied)
		assertTask(t, s, task.ID, domain.TaskDone, "")
		assertTenant(t, s, tenant.ID, domain.TenantActive, 2)
	})

	t.Run("any update to a terminal task is stale", func(t *testing.T) {
		tenant, task := createWithTask(t, s, "terminal")
		mustApply(t, s, update(task.ID, domain.TaskDone), store.UpdateApplied)
		// A redelivered task re-run with the other outcome: a different
		// update_id (so not a duplicate), but it must not flip the result.
		mustApply(t, s, update(task.ID, domain.TaskFailed), store.UpdateStale)
		assertTask(t, s, task.ID, domain.TaskDone, "")
		assertTenant(t, s, tenant.ID, domain.TenantActive, 2)

		failedTenant, failedTask := createWithTask(t, s, "terminal-failed")
		mustApply(t, s, update(failedTask.ID, domain.TaskFailed), store.UpdateApplied)
		mustApply(t, s, update(failedTask.ID, domain.TaskDone), store.UpdateStale)
		mustApply(t, s, update(failedTask.ID, domain.TaskInProgress), store.UpdateStale)
		assertTask(t, s, failedTask.ID, domain.TaskFailed, "simulated failure")
		assertTenant(t, s, failedTenant.ID, domain.TenantFailed, 2)
	})
}

func TestApplyUpdateOutcomesPerTaskType(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newRepo(t)

	tests := []struct {
		name       string
		typ        domain.TaskType
		outcome    domain.TaskStatus
		wantTenant domain.TenantStatus
	}{
		{"deploy failed", domain.TaskDeploy, domain.TaskFailed, domain.TenantFailed},
		{"update done", domain.TaskUpdate, domain.TaskDone, domain.TenantActive},
		{"update failed", domain.TaskUpdate, domain.TaskFailed, domain.TenantFailed},
		{"destroy done", domain.TaskDestroy, domain.TaskDone, domain.TenantDestroyed},
		{"destroy failed", domain.TaskDestroy, domain.TaskFailed, domain.TenantFailed},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tenant, task := createWithTask(t, s, fmt.Sprintf("outcome-%d", i))
			if tt.typ != domain.TaskDeploy {
				mustApply(t, s, update(task.ID, domain.TaskDone), store.UpdateApplied) // active, version 2
				var err error
				if tt.typ == domain.TaskUpdate {
					_, task, err = s.PatchTenant(ctx, tenant.ID, "Renamed", 2)
				} else {
					_, task, err = s.DeleteTenant(ctx, tenant.ID)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			before := mustGet(t, s, tenant.ID).Version

			mustApply(t, s, update(task.ID, tt.outcome), store.UpdateApplied)

			wantErr := ""
			if tt.outcome == domain.TaskFailed {
				wantErr = "simulated failure"
			}
			assertTask(t, s, task.ID, tt.outcome, wantErr)
			assertTenant(t, s, tenant.ID, tt.wantTenant, before+1)
		})
	}
}

func TestApplyUpdateFailedWithoutMessage(t *testing.T) {
	t.Parallel()
	s, _ := newRepo(t)
	_, task := createWithTask(t, s, "acme")

	u := messaging.NewTaskUpdate(task.ID, domain.TaskFailed, "") // the error is optional
	mustApply(t, s, u, store.UpdateApplied)
	assertTask(t, s, task.ID, domain.TaskFailed, "")
}

func TestApplyUpdateUnknownTask(t *testing.T) {
	t.Parallel()
	s, pool := newRepo(t)

	_, err := s.ApplyUpdate(context.Background(), update(uuid.New(), domain.TaskDone))
	if !errors.Is(err, store.ErrTaskNotFound) {
		t.Fatalf("err = %v, want ErrTaskNotFound", err)
	}
	if store.IsTransient(err) {
		t.Error("ErrTaskNotFound classified as transient; it must be dead-lettered")
	}
	// Rolled back, inbox row included: a replay would be processed afresh.
	assertInboxRows(t, pool, 0)
}

func TestApplyUpdateInvariantViolationRollsBack(t *testing.T) {
	t.Parallel()
	s, pool := newRepo(t)
	tenant, task := createWithTask(t, s, "acme")
	mustApply(t, s, update(task.ID, domain.TaskInProgress), store.UpdateApplied)

	// Break the invariant behind the guards' back: the tenant leaves
	// provisioning while its deploy task is still open.
	testutil.SetTenantStatus(t, pool, tenant.ID, domain.TenantActive) // version 2

	_, err := s.ApplyUpdate(context.Background(), update(task.ID, domain.TaskDone))
	if !errors.Is(err, store.ErrInvariant) {
		t.Fatalf("err = %v, want ErrInvariant", err)
	}
	if store.IsTransient(err) {
		t.Error("ErrInvariant classified as transient; it must be dead-lettered")
	}
	// Nothing was applied: the task isn't terminal, and task and tenant still agree with the DB before.
	assertTask(t, s, task.ID, domain.TaskInProgress, "")
	assertTenant(t, s, tenant.ID, domain.TenantActive, 2)
	assertInboxRows(t, pool, 1) // only the in_progress update's
}

// The same update delivered to several consumer goroutines at once: exactly
// one applies it, the rest see a duplicate, and the version moves once.
func TestApplyUpdateConcurrentSameUpdateID(t *testing.T) {
	t.Parallel()
	s, pool := newRepo(t)
	tenant, task := createWithTask(t, s, "acme")
	done := update(task.ID, domain.TaskDone)

	results := applyConcurrently(t, s, repeat(done, 10))

	if results[store.UpdateApplied] != 1 || results[store.UpdateDuplicate] != 9 {
		t.Fatalf("results = %v, want 1 applied and 9 duplicates", results)
	}
	assertTenant(t, s, tenant.ID, domain.TenantActive, 2)
	assertInboxRows(t, pool, 1)
}

// in_progress and done for one task at the same time. The task row lock makes
// them take turns: done always ends up applied, and in_progress is applied if
// it went first and stale if it went second. Either way the end state is the
// same.
func TestApplyUpdateConcurrentProgressAndDone(t *testing.T) {
	t.Parallel()
	s, _ := newRepo(t)

	for round := range 10 {
		tenant, task := createWithTask(t, s, fmt.Sprintf("race-%d", round))
		results := applyConcurrently(t, s, []messaging.TaskUpdate{
			update(task.ID, domain.TaskInProgress),
			update(task.ID, domain.TaskDone),
		})

		if results[store.UpdateApplied] < 1 || results[store.UpdateApplied]+results[store.UpdateStale] != 2 {
			t.Fatalf("round %d: results = %v", round, results)
		}
		assertTask(t, s, task.ID, domain.TaskDone, "")
		assertTenant(t, s, tenant.ID, domain.TenantActive, 2)
	}
}

// ---- helpers ----

func update(taskID uuid.UUID, status domain.TaskStatus) messaging.TaskUpdate {
	return messaging.NewTaskUpdate(taskID, status, "simulated failure")
}

func repeat(u messaging.TaskUpdate, n int) []messaging.TaskUpdate {
	out := make([]messaging.TaskUpdate, n)
	for i := range out {
		out[i] = u
	}
	return out
}

func createWithTask(t *testing.T, s *store.Store, slug string) (domain.Tenant, domain.Task) {
	t.Helper()
	tenant, task, err := s.CreateTenant(context.Background(), slug, "Tenant "+slug)
	if err != nil {
		t.Fatalf("CreateTenant(%q): %v", slug, err)
	}
	return tenant, task
}

func mustApply(t *testing.T, s *store.Store, u messaging.TaskUpdate, want store.UpdateResult) {
	t.Helper()
	got, err := s.ApplyUpdate(context.Background(), u)
	if err != nil {
		t.Fatalf("ApplyUpdate(%s): %v", u.Status, err)
	}
	if got != want {
		t.Fatalf("ApplyUpdate(%s) = %s, want %s", u.Status, got, want)
	}
}

// applyConcurrently applies every update at the same moment, one goroutine
// each, and counts the results.
func applyConcurrently(t *testing.T, s *store.Store, updates []messaging.TaskUpdate) map[store.UpdateResult]int {
	t.Helper()
	var wg sync.WaitGroup
	var mu sync.Mutex
	results := map[store.UpdateResult]int{}
	start := make(chan struct{})
	for _, u := range updates {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := s.ApplyUpdate(context.Background(), u)
			if err != nil {
				t.Errorf("ApplyUpdate(%s): %v", u.Status, err)
				return
			}
			mu.Lock()
			results[res]++
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()
	return results
}

func mustGetTask(t *testing.T, s *store.Store, id uuid.UUID) domain.Task {
	t.Helper()
	task, err := s.GetTask(context.Background(), id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	return task
}

func assertTask(t *testing.T, s *store.Store, id uuid.UUID, status domain.TaskStatus, errMsg string) {
	t.Helper()
	task := mustGetTask(t, s, id)
	if task.Status != status || task.Error != errMsg {
		t.Fatalf("task = %s %q, want %s %q", task.Status, task.Error, status, errMsg)
	}
}

func assertTenant(t *testing.T, s *store.Store, id uuid.UUID, status domain.TenantStatus, version int) {
	t.Helper()
	tenant := mustGet(t, s, id)
	if tenant.Status != status || tenant.Version != version {
		t.Fatalf("tenant = %s v%d, want %s v%d", tenant.Status, tenant.Version, status, version)
	}
}

func assertInboxRows(t *testing.T, pool *pgxpool.Pool, want int) {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM inbox`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != want {
		t.Fatalf("inbox has %d rows, want %d", n, want)
	}
}
