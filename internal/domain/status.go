// Package domain holds the tenant and task model and their state machines
// (DESIGN.md §4). It is pure: no I/O, no database or broker types.
package domain

// TenantStatus is a tenant's position in its state machine.
type TenantStatus string

const (
	TenantProvisioning TenantStatus = "provisioning"
	TenantActive       TenantStatus = "active"
	TenantUpdating     TenantStatus = "updating"
	TenantDestroying   TenantStatus = "destroying"
	TenantDestroyed    TenantStatus = "destroyed"
	TenantFailed       TenantStatus = "failed"
)

// Valid reports whether s is one of the defined tenant statuses.
func (s TenantStatus) Valid() bool {
	switch s {
	case TenantProvisioning, TenantActive, TenantUpdating, TenantDestroying, TenantDestroyed, TenantFailed:
		return true
	}
	return false
}

// TaskStatus is a task's position in its state machine.
type TaskStatus string

const (
	TaskAccepted   TaskStatus = "accepted"
	TaskInProgress TaskStatus = "in_progress"
	TaskDone       TaskStatus = "done"
	TaskFailed     TaskStatus = "failed"
)

// Valid reports whether s is one of the defined task statuses.
func (s TaskStatus) Valid() bool {
	return s.rank() >= 0
}

// Terminal reports whether s is final: done or failed.
func (s TaskStatus) Terminal() bool {
	return s == TaskDone || s == TaskFailed
}

// rank orders task statuses for the forward-only rule (DESIGN.md §4):
// accepted(0) < in_progress(1) < done|failed(2). Unknown statuses rank -1.
func (s TaskStatus) rank() int {
	switch s {
	case TaskAccepted:
		return 0
	case TaskInProgress:
		return 1
	case TaskDone, TaskFailed:
		return 2
	}
	return -1
}

// TaskType is the kind of operation a task performs on its tenant.
type TaskType string

const (
	TaskDeploy  TaskType = "deploy"
	TaskUpdate  TaskType = "update"
	TaskDestroy TaskType = "destroy"
)

// Valid reports whether t is one of the defined task types.
func (t TaskType) Valid() bool {
	switch t {
	case TaskDeploy, TaskUpdate, TaskDestroy:
		return true
	}
	return false
}
