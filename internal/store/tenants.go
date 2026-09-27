package store

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Ankitwasnik/tenant-service/internal/domain"
	"github.com/Ankitwasnik/tenant-service/internal/store/sqlcgen"
)

// CreateTenant inserts a tenant in provisioning, its deploy task, and the
// task's outbox row, all in one tx. name must already be normalized
// (domain.ValidateCreate). A taken slug, including a destroyed tenant's,
// returns tenant_already_exists: the unique index makes concurrent creates
// yield exactly one winner (DESIGN.md §5).
func (s *Store) CreateTenant(ctx context.Context, slug, name string) (domain.Tenant, domain.Task, error) {
	var tenant domain.Tenant
	var task domain.Task
	err := s.inTx(ctx, func(q *sqlcgen.Queries) error {
		row, err := q.InsertTenant(ctx, sqlcgen.InsertTenantParams{Slug: slug, Name: name})
		if isUniqueViolation(err, "tenants_slug_key") {
			return domain.TenantAlreadyExists(slug)
		}
		if err != nil {
			return fmt.Errorf("insert tenant: %w", err)
		}
		tenant = toTenant(row)
		task, err = insertTaskWithEvent(ctx, q, tenant.ID, domain.TaskDeploy)
		return err
	})
	if err != nil {
		return domain.Tenant{}, domain.Task{}, err
	}
	return tenant, task, nil
}

// PatchTenant renames an active tenant whose version is expectedVersion,
// moves it to updating, and creates the update task and its outbox row. name
// must already be normalized (domain.NormalizeName).
func (s *Store) PatchTenant(ctx context.Context, id uuid.UUID, name string, expectedVersion int) (domain.Tenant, domain.Task, error) {
	return s.mutate(ctx, id, guardedMutation{
		op:       "patch",
		taskType: domain.TaskUpdate,
		guard: func(ctx context.Context, q *sqlcgen.Queries) (sqlcgen.Tenant, error) {
			if expectedVersion < 1 || expectedVersion > math.MaxInt32 {
				// No row can have this version; let the diagnosis explain.
				return sqlcgen.Tenant{}, pgx.ErrNoRows
			}
			return q.UpdateTenantIfActive(ctx, sqlcgen.UpdateTenantIfActiveParams{
				ID:              id,
				Name:            name,
				ExpectedVersion: int32(expectedVersion),
			})
		},
		diagnose: func(current domain.Tenant) error {
			return domain.PatchConflict(expectedVersion, current)
		},
	})
}

// DeleteTenant moves an active or failed tenant to destroying and creates the
// destroy task and its outbox row. It takes no version (DESIGN.md §5).
func (s *Store) DeleteTenant(ctx context.Context, id uuid.UUID) (domain.Tenant, domain.Task, error) {
	return s.mutate(ctx, id, guardedMutation{
		op:       "delete",
		taskType: domain.TaskDestroy,
		guard: func(ctx context.Context, q *sqlcgen.Queries) (sqlcgen.Tenant, error) {
			return q.MarkTenantDestroying(ctx, id)
		},
		diagnose: domain.DeleteConflict,
	})
}

func (s *Store) GetTenant(ctx context.Context, id uuid.UUID) (domain.Tenant, error) {
	row, err := sqlcgen.New(s.pool).GetTenant(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Tenant{}, domain.TenantNotFound(id)
	}
	if err != nil {
		return domain.Tenant{}, fmt.Errorf("get tenant: %w", err)
	}
	return toTenant(row), nil
}

// ListTenants returns one page of tenants in every status, newest first, and
// the cursor for the next page (nil on the last page).
func (s *Store) ListTenants(ctx context.Context, p Page) ([]domain.Tenant, *uuid.UUID, error) {
	limit, err := pageLimit(p)
	if err != nil {
		return nil, nil, err
	}
	rows, err := sqlcgen.New(s.pool).ListTenants(ctx, sqlcgen.ListTenantsParams{Cursor: p.Cursor, PageLimit: limit})
	if err != nil {
		return nil, nil, fmt.Errorf("list tenants: %w", err)
	}
	rows, next := trimPage(rows, p.Limit, func(r sqlcgen.Tenant) uuid.UUID { return r.ID })

	tenants := make([]domain.Tenant, len(rows))
	for i, r := range rows {
		tenants[i] = toTenant(r)
	}
	return tenants, next, nil
}

// maxGuardAttempts bounds the retry in mutate. A retry needs the tenant to
// change between the guarded UPDATE and the diagnosing read, so a second
// attempt almost always settles it.
const maxGuardAttempts = 3

// errGuardRetry signals, inside mutate, that the guard missed but the
// operation is allowed against the tenant as it is now.
var errGuardRetry = errors.New("tenant changed during the guarded update")

// guardedMutation is one API mutation (PATCH or DELETE): a conditional UPDATE
// on the tenant row, the domain rule that explains a miss, and the task it
// creates on success.
type guardedMutation struct {
	op       string
	taskType domain.TaskType
	// guard runs the conditional UPDATE. pgx.ErrNoRows means it matched nothing.
	guard func(ctx context.Context, q *sqlcgen.Queries) (sqlcgen.Tenant, error)
	// diagnose picks the error for a miss, given the tenant now; nil means
	// the operation is allowed now, so the UPDATE is retried.
	diagnose func(current domain.Tenant) error
}

// mutate runs m in one tx: guard, then task and outbox inserts, then commit
// (DESIGN.md §5). On a miss it reads the tenant and lets m.diagnose choose
// tenant_not_found / tenant_version_conflict / tenant_update_not_allowed. If
// the tenant changed in between so that the operation is now allowed, it
// retries, at most maxGuardAttempts times.
func (s *Store) mutate(ctx context.Context, id uuid.UUID, m guardedMutation) (domain.Tenant, domain.Task, error) {
	for range maxGuardAttempts {
		var tenant domain.Tenant
		var task domain.Task
		err := s.inTx(ctx, func(q *sqlcgen.Queries) error {
			row, err := m.guard(ctx, q)
			if errors.Is(err, pgx.ErrNoRows) {
				return diagnoseMiss(ctx, q, id, m.diagnose)
			}
			if err != nil {
				return fmt.Errorf("%s tenant: %w", m.op, err)
			}
			tenant = toTenant(row)
			task, err = insertTaskWithEvent(ctx, q, tenant.ID, m.taskType)
			return err
		})
		if errors.Is(err, errGuardRetry) {
			continue
		}
		if err != nil {
			return domain.Tenant{}, domain.Task{}, err
		}
		return tenant, task, nil
	}
	return domain.Tenant{}, domain.Task{}, fmt.Errorf("%s tenant %s: tenant changed on each of %d attempts", m.op, id, maxGuardAttempts)
}

// diagnoseMiss reads the tenant after a guard miss and returns the error to
// report, or errGuardRetry if the operation is allowed now.
func diagnoseMiss(ctx context.Context, q *sqlcgen.Queries, id uuid.UUID, diagnose func(domain.Tenant) error) error {
	row, err := q.GetTenant(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.TenantNotFound(id)
	}
	if err != nil {
		return fmt.Errorf("read tenant after guard miss: %w", err)
	}
	if err := diagnose(toTenant(row)); err != nil {
		return err
	}
	return errGuardRetry
}
