package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Ankitwasnik/tenant-service/internal/domain"
	"github.com/Ankitwasnik/tenant-service/internal/messaging"
	"github.com/Ankitwasnik/tenant-service/internal/store/sqlcgen"
)

// Store is the repository. It maps sqlcgen rows to domain types, so callers
// never see sqlc or pgx types (DESIGN.md §3).
type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Page is a keyset page request: rows with an id below Cursor (all rows if
// Cursor is nil), newest first, at most Limit of them. The caller validates
// Limit (DESIGN.md §7).
type Page struct {
	Cursor *uuid.UUID
	Limit  int
}

// inTx runs fn in one transaction: committed if fn returns nil, rolled back
// otherwise. Every statement of a unit of work uses the q it is given.
func (s *Store) inTx(ctx context.Context, fn func(q *sqlcgen.Queries) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return fn(sqlcgen.New(tx))
	})
}

// insertTaskWithEvent inserts an accepted task and its outbox row. It runs
// inside the caller's tx, so the event exists only if the whole mutation
// commits (DESIGN.md §6).
func insertTaskWithEvent(ctx context.Context, q *sqlcgen.Queries, tenantID uuid.UUID, typ domain.TaskType) (domain.Task, error) {
	row, err := q.InsertTask(ctx, sqlcgen.InsertTaskParams{TenantID: tenantID, Type: string(typ)})
	if err != nil {
		return domain.Task{}, fmt.Errorf("insert task: %w", err)
	}
	task := toTask(row)

	payload, err := json.Marshal(messaging.NewTaskEvent(task))
	if err != nil {
		return domain.Task{}, fmt.Errorf("encode task event: %w", err)
	}
	err = q.InsertOutbox(ctx, sqlcgen.InsertOutboxParams{
		TaskID:     task.ID,
		RoutingKey: messaging.TaskRoutingKey(task.Type),
		Payload:    payload,
	})
	if err != nil {
		return domain.Task{}, fmt.Errorf("insert outbox: %w", err)
	}
	return task, nil
}

func toTenant(r sqlcgen.Tenant) domain.Tenant {
	return domain.Tenant{
		ID:        r.ID,
		Slug:      r.Slug,
		Name:      r.Name,
		Status:    domain.TenantStatus(r.Status),
		Version:   int(r.Version),
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
	}
}

func toTask(r sqlcgen.Task) domain.Task {
	t := domain.Task{
		ID:        r.ID,
		TenantID:  r.TenantID,
		Type:      domain.TaskType(r.Type),
		Status:    domain.TaskStatus(r.Status),
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
	}
	if r.Error != nil {
		t.Error = *r.Error
	}
	return t
}

// isUniqueViolation reports whether err is a 23505 on the named constraint.
// The name is checked so that, say, the one-open-task safety net is never
// mistaken for a duplicate slug.
func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

// pageLimit converts a page size for SQL. It fetches one extra row, so the
// caller can tell whether another page exists without a count query.
func pageLimit(p Page) (int32, error) {
	if p.Limit < 1 || p.Limit > maxPageLimit {
		return 0, fmt.Errorf("page limit %d out of range 1-%d", p.Limit, maxPageLimit)
	}
	return int32(p.Limit + 1), nil //nolint:gosec // bounded above: at most maxPageLimit+1
}

// maxPageLimit bounds a page at the store level; the API applies its own,
// smaller maximum (DESIGN.md §7).
const maxPageLimit = 1000

// trimPage cuts the extra row fetched by pageLimit and returns the cursor for
// the next page: the last returned row's id, or nil on the last page.
func trimPage[T any](rows []T, limit int, id func(T) uuid.UUID) ([]T, *uuid.UUID) {
	if len(rows) <= limit {
		return rows, nil
	}
	rows = rows[:limit]
	next := id(rows[limit-1])
	return rows, &next
}
