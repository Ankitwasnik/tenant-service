package testutil

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Ankitwasnik/tenant-service/internal/domain"
)

// FinishTask stands in for the worker and the update consumer (PLAN.md 6.1):
// it closes the tenant's open task (done, or failed if status is failed) and
// moves the tenant to status, bumping the version as every tenant write does.
func FinishTask(t *testing.T, pool *pgxpool.Pool, tenantID uuid.UUID, status domain.TenantStatus) {
	t.Helper()
	taskStatus := domain.TaskDone
	if status == domain.TenantFailed {
		taskStatus = domain.TaskFailed
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE tasks SET status = $2, updated_at = now() WHERE tenant_id = $1 AND status IN ('accepted', 'in_progress')`,
		tenantID, string(taskStatus)); err != nil {
		t.Fatalf("close open task: %v", err)
	}
	SetTenantStatus(t, pool, tenantID, status)
}

// SetTenantStatus moves a tenant to status directly, bumping its version.
func SetTenantStatus(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, status domain.TenantStatus) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE tenants SET status = $2, version = version + 1, updated_at = now() WHERE id = $1`,
		id, string(status)); err != nil {
		t.Fatalf("set tenant status: %v", err)
	}
}
