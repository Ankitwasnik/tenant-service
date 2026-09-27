package store

import (
	"context"
	"fmt"
	"math"

	"github.com/google/uuid"

	"github.com/Ankitwasnik/tenant-service/internal/store/sqlcgen"
)

// OutboxMessage is one committed, not yet published event.
type OutboxMessage struct {
	ID         int64 // outbox row id
	TaskID     uuid.UUID
	RoutingKey string
	Payload    []byte
}

// PublishFunc publishes msgs and returns the ids of those the broker
// confirmed. A non-nil error may come with a partial list: the ids returned
// are still marked.
type PublishFunc func(ctx context.Context, msgs []OutboxMessage) (confirmed []int64, err error)

// RelayOutbox runs one relay step in one tx (DESIGN.md §6): claim up to limit
// unpublished events (FOR UPDATE SKIP LOCKED), publish them, mark the
// confirmed ones, commit. It returns how many were claimed and marked.
//
// The marks are committed even when publish fails part-way, so confirmed
// events are never sent again. The publish error is returned after the
// commit, so the caller backs off outside the tx, never holding its locks.
// Unconfirmed events stay unpublished and are claimed again by a later step.
func (s *Store) RelayOutbox(ctx context.Context, limit int, publish PublishFunc) (claimed, marked int, err error) {
	if limit < 1 || limit > math.MaxInt32 {
		return 0, 0, fmt.Errorf("relay batch size %d out of range", limit)
	}

	var publishErr error
	err = s.inTx(ctx, func(q *sqlcgen.Queries) error {
		rows, err := q.ClaimUnpublished(ctx, int32(limit)) //nolint:gosec // range-checked above
		if err != nil {
			return fmt.Errorf("claim outbox rows: %w", err)
		}
		claimed = len(rows)
		if claimed == 0 {
			return nil
		}

		msgs := make([]OutboxMessage, len(rows))
		inBatch := make(map[int64]bool, len(rows))
		for i, r := range rows {
			msgs[i] = OutboxMessage{ID: r.ID, TaskID: r.TaskID, RoutingKey: r.RoutingKey, Payload: r.Payload}
			inBatch[r.ID] = true
		}

		confirmed, err := publish(ctx, msgs)
		publishErr = err

		// Only ids from this batch: this tx holds no lock on any other row.
		ids := make([]int64, 0, len(confirmed))
		for _, id := range confirmed {
			if inBatch[id] {
				ids = append(ids, id)
				delete(inBatch, id) // a duplicate id is marked once
			}
		}
		marked = len(ids)
		if marked == 0 {
			return nil
		}
		if err := q.MarkPublished(ctx, ids); err != nil {
			return fmt.Errorf("mark outbox rows published: %w", err)
		}
		return nil
	})
	if err != nil {
		// The tx rolled back: nothing is marked, and all of it is claimed again.
		return claimed, 0, err
	}
	return claimed, marked, publishErr
}
