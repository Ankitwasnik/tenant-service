package api

import (
	"github.com/google/uuid"

	"github.com/Ankitwasnik/tenant-service/internal/domain"
)

// The JSON representations. They are separate from the domain types, so the
// wire format (field names, timestamp format, omitted fields) is decided here
// and can't change by accident when a domain struct does.

type tenantView struct {
	ID        uuid.UUID           `json:"id"`
	Slug      string              `json:"slug"`
	Name      string              `json:"name"`
	Status    domain.TenantStatus `json:"status"`
	Version   int                 `json:"version"`
	CreatedAt string              `json:"created_at"`
	UpdatedAt string              `json:"updated_at"`
}

type taskView struct {
	ID        uuid.UUID         `json:"id"`
	Type      domain.TaskType   `json:"type"`
	TenantID  uuid.UUID         `json:"tenant_id"`
	Status    domain.TaskStatus `json:"status"`
	Error     string            `json:"error,omitempty"` // only on a failed task
	CreatedAt string            `json:"created_at"`
	UpdatedAt string            `json:"updated_at"`
}

// mutationView is the body of POST, PATCH and DELETE: the tenant as it now
// is, and the task that will carry the change out (DESIGN.md §7).
type mutationView struct {
	Tenant tenantView `json:"tenant"`
	Task   taskView   `json:"task"`
}

// pageView is one page of a list. NextCursor is null on the last page.
type pageView[T any] struct {
	Items      []T        `json:"items"`
	NextCursor *uuid.UUID `json:"next_cursor"`
}

func newTenantView(t domain.Tenant) tenantView {
	return tenantView{
		ID:        t.ID,
		Slug:      t.Slug,
		Name:      t.Name,
		Status:    t.Status,
		Version:   t.Version,
		CreatedAt: domain.FormatTime(t.CreatedAt),
		UpdatedAt: domain.FormatTime(t.UpdatedAt),
	}
}

func newTaskView(t domain.Task) taskView {
	return taskView{
		ID:        t.ID,
		Type:      t.Type,
		TenantID:  t.TenantID,
		Status:    t.Status,
		Error:     t.Error,
		CreatedAt: domain.FormatTime(t.CreatedAt),
		UpdatedAt: domain.FormatTime(t.UpdatedAt),
	}
}

// newPageView maps a page of domain values. Items is never nil, so an empty
// page is `"items": []`, not `null`.
func newPageView[D, V any](items []D, next *uuid.UUID, view func(D) V) pageView[V] {
	out := make([]V, len(items))
	for i, item := range items {
		out[i] = view(item)
	}
	return pageView[V]{Items: out, NextCursor: next}
}
