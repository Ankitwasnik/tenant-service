package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Ankitwasnik/tenant-service/internal/domain"
	"github.com/Ankitwasnik/tenant-service/internal/store/sqlcgen"
)

func (s *Store) GetTask(ctx context.Context, id uuid.UUID) (domain.Task, error) {
	row, err := sqlcgen.New(s.pool).GetTask(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Task{}, domain.TaskNotFound(id)
	}
	if err != nil {
		return domain.Task{}, fmt.Errorf("get task: %w", err)
	}
	return toTask(row), nil
}

// ListTasks returns one page of tasks, newest first, for one tenant if
// tenantID is set. An unknown tenant gives an empty page, not
// tenant_not_found: a filter that matches nothing isn't a missing resource
// (DESIGN.md §7).
func (s *Store) ListTasks(ctx context.Context, tenantID *uuid.UUID, p Page) ([]domain.Task, *uuid.UUID, error) {
	limit, err := pageLimit(p)
	if err != nil {
		return nil, nil, err
	}
	rows, err := sqlcgen.New(s.pool).ListTasks(ctx, sqlcgen.ListTasksParams{
		TenantID:  tenantID,
		Cursor:    p.Cursor,
		PageLimit: limit,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("list tasks: %w", err)
	}
	rows, next := trimPage(rows, p.Limit, func(r sqlcgen.Task) uuid.UUID { return r.ID })

	tasks := make([]domain.Task, len(rows))
	for i, r := range rows {
		tasks[i] = toTask(r)
	}
	return tasks, next, nil
}
