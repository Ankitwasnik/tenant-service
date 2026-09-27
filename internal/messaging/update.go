package messaging

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/Ankitwasnik/tenant-service/internal/domain"
)

// TaskUpdate is the inbound progress message, worker → control plane
// (DESIGN.md §6). UpdateID is the idempotency key; TaskID says which task it
// updates. The AMQP message_id is the UpdateID.
type TaskUpdate struct {
	UpdateID uuid.UUID         `json:"update_id"`
	TaskID   uuid.UUID         `json:"task_id"`
	Status   domain.TaskStatus `json:"status"`
	Error    string            `json:"error,omitempty"` // only with status failed, and optional there
}

// ErrInvalidMessage marks a message that can never be processed, however
// often it is retried. The consumer dead-letters it.
var ErrInvalidMessage = errors.New("invalid message")

// updateIDNamespace is the UUIDv5 namespace for update ids. It is fixed: a
// different value would give a re-run of the same task different ids, and
// the inbox would no longer recognise the duplicates.
var updateIDNamespace = uuid.MustParse("3fdf24c7-f60c-465a-855c-5c8d6bb88c9c")

// UpdateID is the deterministic id of the update moving taskID to status:
// UUIDv5 of "<task id>:<status>". A task delivered to the worker twice (the
// relay is at-least-once) produces the same ids the second time, so the inbox
// drops the repeats with no coordination between runs.
func UpdateID(taskID uuid.UUID, status domain.TaskStatus) uuid.UUID {
	return uuid.NewSHA1(updateIDNamespace, []byte(taskID.String()+":"+string(status)))
}

// NewTaskUpdate builds the update moving taskID to status, with its
// deterministic id. errMsg is only kept for a failed update.
func NewTaskUpdate(taskID uuid.UUID, status domain.TaskStatus, errMsg string) TaskUpdate {
	u := TaskUpdate{UpdateID: UpdateID(taskID, status), TaskID: taskID, Status: status}
	if status == domain.TaskFailed {
		u.Error = errMsg
	}
	return u
}

// UpdateRoutingKey is the routing key an update to status is published with.
func UpdateRoutingKey(status domain.TaskStatus) string {
	return "task.update." + string(status)
}

// DecodeTaskUpdate parses and validates a message body. Any failure wraps
// ErrInvalidMessage: a message that doesn't decode now never will.
func DecodeTaskUpdate(body []byte) (TaskUpdate, error) {
	var u TaskUpdate
	if err := json.Unmarshal(body, &u); err != nil {
		return TaskUpdate{}, fmt.Errorf("%w: decode task update: %w", ErrInvalidMessage, err)
	}
	if err := u.Validate(); err != nil {
		return TaskUpdate{}, err
	}
	return u, nil
}

// Validate checks the rules in DESIGN.md §6. Unknown JSON fields are ignored,
// so a newer worker can add fields without breaking an older control plane.
func (u TaskUpdate) Validate() error {
	switch {
	case u.UpdateID == uuid.Nil:
		return fmt.Errorf("%w: update_id is missing", ErrInvalidMessage)
	case u.TaskID == uuid.Nil:
		return fmt.Errorf("%w: task_id is missing", ErrInvalidMessage)
	}
	switch u.Status {
	case domain.TaskInProgress, domain.TaskDone, domain.TaskFailed:
	case domain.TaskAccepted:
		// Only the control plane creates tasks, and they start accepted.
		return fmt.Errorf("%w: status accepted is never a valid update", ErrInvalidMessage)
	default:
		return fmt.Errorf("%w: unknown status %q", ErrInvalidMessage, u.Status)
	}
	if u.Error != "" && u.Status != domain.TaskFailed {
		return fmt.Errorf("%w: error is only allowed with status failed, not %s", ErrInvalidMessage, u.Status)
	}
	return nil
}
