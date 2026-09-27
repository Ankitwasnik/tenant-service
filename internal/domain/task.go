package domain

import (
	"time"

	"github.com/google/uuid"
)

// Task is an asynchronous operation on a tenant. It is also the event that is
// published to the worker: its ID doubles as the event id.
type Task struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	Type      TaskType
	Status    TaskStatus
	Error     string // set when the worker reports failed; empty otherwise
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ShouldApply reports whether an update moving a task from current to
// incoming is applied. Only forward moves are (DESIGN.md §4), which makes
// duplicates, out-of-order deliveries and updates to terminal tasks no-ops.
// A jump such as accepted → done is forward, so it applies.
func ShouldApply(current, incoming TaskStatus) bool {
	if !current.Valid() || !incoming.Valid() {
		return false
	}
	return incoming.rank() > current.rank()
}

// TerminalOutcome returns what a terminal task status does to its tenant
// (DESIGN.md §4): the tenant's new status, and the status it must be in for
// the change to apply. ok is false if s is not terminal or t is unknown.
func TerminalOutcome(t TaskType, s TaskStatus) (next, expected TenantStatus, ok bool) {
	if !s.Terminal() {
		return "", "", false
	}

	switch t {
	case TaskDeploy:
		expected, next = TenantProvisioning, TenantActive
	case TaskUpdate:
		expected, next = TenantUpdating, TenantActive
	case TaskDestroy:
		expected, next = TenantDestroying, TenantDestroyed
	default:
		return "", "", false
	}

	if s == TaskFailed {
		next = TenantFailed
	}
	return next, expected, true
}
