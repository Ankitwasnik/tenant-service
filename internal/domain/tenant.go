package domain

import (
	"time"

	"github.com/google/uuid"
)

// Tenant is a customer environment, mutated only through tasks.
type Tenant struct {
	ID        uuid.UUID
	Slug      string
	Name      string
	Status    TenantStatus
	Version   int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CanPatch reports whether a tenant in status s accepts a PATCH.
func CanPatch(s TenantStatus) bool {
	return s == TenantActive
}

// CanDelete reports whether a tenant in status s accepts a DELETE.
func CanDelete(s TenantStatus) bool {
	return s == TenantActive || s == TenantFailed
}

// PatchConflict explains why a guarded PATCH (DESIGN.md §5) matched no row,
// given the tenant as it is now. The version is checked before the status:
// a stale version means the client's view is out of date, and it makes racing
// PATCHes report tenant_version_conflict rather than tenant_update_not_allowed.
//
// It returns nil if the PATCH would be allowed against current. The caller
// then retries the UPDATE; that can only happen if the row changed between
// the UPDATE and the read that produced current.
func PatchConflict(expectedVersion int, current Tenant) error {
	if current.Version != expectedVersion {
		return TenantVersionConflict(expectedVersion, current.Version)
	}
	if !CanPatch(current.Status) {
		return TenantUpdateNotAllowed("PATCH", current.Status)
	}
	return nil
}

// DeleteConflict explains why a guarded DELETE matched no row, given the
// tenant as it is now. DELETE takes no version (DESIGN.md §5), so only the
// status is checked. Like PatchConflict, nil means "allowed now, retry".
func DeleteConflict(current Tenant) error {
	if !CanDelete(current.Status) {
		return TenantUpdateNotAllowed("DELETE", current.Status)
	}
	return nil
}
