package store

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Ankitwasnik/tenant-service/internal/domain"
	"github.com/Ankitwasnik/tenant-service/internal/store/sqlcgen"
	"github.com/Ankitwasnik/tenant-service/internal/testutil"
)

// The retry in mutate needs the tenant to change between the guarded UPDATE
// and the diagnosing read, a window too narrow to hit reliably with real
// concurrency. These tests drive it with a guard that misses on purpose.

func TestMutateRetriesWhenAllowedAfterMiss(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openInternal(t)

	tenant, _, err := s.CreateTenant(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET status = 'failed' WHERE tenant_id = $1`, tenant.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE tenants SET status = 'failed', version = version + 1 WHERE id = $1`, tenant.ID); err != nil {
		t.Fatal(err)
	}

	calls := 0
	got, task, err := s.mutate(ctx, tenant.ID, guardedMutation{
		op:       "delete",
		taskType: domain.TaskDestroy,
		guard: func(ctx context.Context, q *sqlcgen.Queries) (sqlcgen.Tenant, error) {
			calls++
			if calls == 1 {
				// First attempt: pretend the UPDATE ran while the tenant was
				// still provisioning. The diagnosis then sees "failed".
				return sqlcgen.Tenant{}, pgx.ErrNoRows
			}
			return q.MarkTenantDestroying(ctx, tenant.ID)
		},
		diagnose: domain.DeleteConflict,
	})
	if err != nil {
		t.Fatalf("mutate: %v", err)
	}
	if calls != 2 {
		t.Errorf("guard ran %d times, want 2 (one retry)", calls)
	}
	if got.Status != domain.TenantDestroying || task.Type != domain.TaskDestroy {
		t.Errorf("tenant %+v, task %+v", got, task)
	}
}

func TestMutateGivesUpAfterMaxAttempts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openInternal(t)

	tenant, _, err := s.CreateTenant(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}

	calls := 0
	_, _, err = s.mutate(ctx, tenant.ID, guardedMutation{
		op:       "delete",
		taskType: domain.TaskDestroy,
		guard: func(context.Context, *sqlcgen.Queries) (sqlcgen.Tenant, error) {
			calls++
			return sqlcgen.Tenant{}, pgx.ErrNoRows
		},
		diagnose: func(domain.Tenant) error { return nil }, // "allowed now", every time
	})
	if err == nil || !strings.Contains(err.Error(), "attempts") {
		t.Fatalf("err = %v, want give-up error", err)
	}
	if calls != maxGuardAttempts {
		t.Errorf("guard ran %d times, want %d", calls, maxGuardAttempts)
	}
	if code := domain.CodeOf(err); code != "" {
		t.Errorf("give-up error has contract code %q; it is an internal error", code)
	}

	var outboxRows int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&outboxRows); err != nil {
		t.Fatal(err)
	}
	if outboxRows != 1 {
		t.Errorf("outbox has %d rows, want only the create's", outboxRows)
	}
}

func openInternal(t *testing.T) *Store {
	t.Helper()
	pool, err := Open(context.Background(), testutil.NewDatabase(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(pool.Close)
	return New(pool)
}
