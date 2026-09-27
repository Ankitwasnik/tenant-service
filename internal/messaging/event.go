// Package messaging holds what the control plane and the worker share over
// RabbitMQ: message envelopes and (from PLAN.md 4.1) the topology and
// connection handling.
package messaging

import (
	"github.com/google/uuid"

	"github.com/Ankitwasnik/tenant-service/internal/domain"
)

// TaskEvent is the outbound task message (DESIGN.md §6). The task is the
// event: a consumer recovers the task's id, type, tenant_id and status from
// it. The id doubles as the AMQP message_id.
type TaskEvent struct {
	ID        uuid.UUID         `json:"id"`
	Type      domain.TaskType   `json:"type"`
	TenantID  uuid.UUID         `json:"tenant_id"`
	Status    domain.TaskStatus `json:"status"`
	CreatedAt string            `json:"created_at"`
}

// NewTaskEvent builds the event for a task as it is when committed.
func NewTaskEvent(t domain.Task) TaskEvent {
	return TaskEvent{
		ID:        t.ID,
		Type:      t.Type,
		TenantID:  t.TenantID,
		Status:    t.Status,
		CreatedAt: domain.FormatTime(t.CreatedAt),
	}
}

// TaskRoutingKey is the routing key a task of type t is published with.
func TaskRoutingKey(t domain.TaskType) string {
	return "task." + string(t)
}
