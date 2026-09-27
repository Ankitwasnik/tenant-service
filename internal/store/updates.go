package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Ankitwasnik/tenant-service/internal/domain"
	"github.com/Ankitwasnik/tenant-service/internal/messaging"
	"github.com/Ankitwasnik/tenant-service/internal/store/sqlcgen"
)

// UpdateResult is what ApplyUpdate did with a worker update.
type UpdateResult string

const (
	// UpdateApplied: the task moved forward, and on a terminal outcome so did its tenant.
	UpdateApplied UpdateResult = "applied"
	// UpdateDuplicate: this update_id was already processed; nothing changed.
	UpdateDuplicate UpdateResult = "duplicate"
	// UpdateStale: a new update that doesn't move the task forward (it arrived
	// out of order, or the task is already terminal); nothing changed.
	UpdateStale UpdateResult = "stale"
)

// Permanent failures: retrying the same update can never succeed, so the
// consumer dead-letters it. In both cases the tx is rolled back, inbox row
// included, so a replay from the DLQ is processed afresh.
var (
	// ErrTaskNotFound: the update names a task that doesn't exist. The relay
	// publishes only committed tasks, so this means a hand-made or foreign message.
	ErrTaskNotFound = errors.New("task not found")
	// ErrInvariant: a terminal outcome found its tenant in the wrong status.
	// The API only moves a tenant when it has no open task, so this can't
	// happen unless a bug broke the guards (DESIGN.md §5).
	ErrInvariant = errors.New("tenant not in the expected status")
)

// ApplyUpdate applies one worker update in one tx (DESIGN.md §6):
//
//  1. Insert the update_id into the inbox; if it is already there, the
//     update is a duplicate and nothing else happens.
//  2. Lock the task row, so concurrent updates to one task take turns and each
//     sees the others' committed result.
//  3. Apply only a forward move (domain.ShouldApply); anything else is stale.
//  4. On a terminal status, move the tenant too, guarded on its expected
//     status; a miss rolls everything back.
//
// Duplicates and stale updates commit (the inbox row stays), so a redelivery
// is recognised as a duplicate next time.
func (s *Store) ApplyUpdate(ctx context.Context, u messaging.TaskUpdate) (UpdateResult, error) {
	var result UpdateResult
	err := s.inTx(ctx, func(q *sqlcgen.Queries) error {
		inserted, err := q.InsertInbox(ctx, sqlcgen.InsertInboxParams{UpdateID: u.UpdateID, TaskID: u.TaskID})
		if err != nil {
			return fmt.Errorf("record update in inbox: %w", err)
		}
		if inserted == 0 {
			result = UpdateDuplicate
			return nil
		}

		row, err := q.LockTask(ctx, u.TaskID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("update %s: %w: %s", u.UpdateID, ErrTaskNotFound, u.TaskID)
		}
		if err != nil {
			return fmt.Errorf("lock task: %w", err)
		}
		task := toTask(row)

		if !domain.ShouldApply(task.Status, u.Status) {
			result = UpdateStale
			return nil
		}

		var errMsg *string
		if u.Status == domain.TaskFailed && u.Error != "" {
			errMsg = &u.Error
		}
		if _, err := q.UpdateTaskStatus(ctx, sqlcgen.UpdateTaskStatusParams{
			ID: task.ID, Status: string(u.Status), Error: errMsg,
		}); err != nil {
			return fmt.Errorf("update task: %w", err)
		}

		if next, expected, terminal := domain.TerminalOutcome(task.Type, u.Status); terminal {
			_, err := q.ApplyTenantOutcome(ctx, sqlcgen.ApplyTenantOutcomeParams{
				ID: task.TenantID, NextStatus: string(next), ExpectedStatus: string(expected),
			})
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%s task %s %s: %w: want %s", task.Type, task.ID, u.Status, ErrInvariant, expected)
			}
			if err != nil {
				return fmt.Errorf("update tenant: %w", err)
			}
		}
		result = UpdateApplied
		return nil
	})
	if err != nil {
		return "", err
	}
	return result, nil
}
