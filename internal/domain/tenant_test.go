package domain_test

import (
	"testing"

	"github.com/Ankitwasnik/tenant-service/internal/domain"
)

var allTenantStatuses = []domain.TenantStatus{
	domain.TenantProvisioning,
	domain.TenantActive,
	domain.TenantUpdating,
	domain.TenantDestroying,
	domain.TenantDestroyed,
	domain.TenantFailed,
}

func TestTenantGuards(t *testing.T) {
	tests := []struct {
		status    domain.TenantStatus
		canPatch  bool
		canDelete bool
	}{
		{domain.TenantProvisioning, false, false},
		{domain.TenantActive, true, true},
		{domain.TenantUpdating, false, false},
		{domain.TenantDestroying, false, false},
		{domain.TenantDestroyed, false, false},
		{domain.TenantFailed, false, true},
	}
	if len(tests) != len(allTenantStatuses) {
		t.Fatalf("table covers %d statuses, want all %d", len(tests), len(allTenantStatuses))
	}

	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			if got := domain.CanPatch(tt.status); got != tt.canPatch {
				t.Errorf("CanPatch = %v, want %v", got, tt.canPatch)
			}
			if got := domain.CanDelete(tt.status); got != tt.canDelete {
				t.Errorf("CanDelete = %v, want %v", got, tt.canDelete)
			}
		})
	}
}

func TestPatchConflict(t *testing.T) {
	const expected = 3

	tests := []struct {
		name     string
		current  domain.Tenant
		wantCode domain.Code // "" means nil: allowed now
	}{
		{"stale version, active", tenant(domain.TenantActive, 4), domain.CodeTenantVersionConflict},
		// A racing PATCH that lost: the winner moved the row to updating and
		// bumped the version. Version wins, so the loser sees a version conflict.
		{"stale version, updating", tenant(domain.TenantUpdating, 4), domain.CodeTenantVersionConflict},
		{"stale version, destroyed", tenant(domain.TenantDestroyed, 7), domain.CodeTenantVersionConflict},
		{"current version, active", tenant(domain.TenantActive, expected), ""},
	}
	for _, s := range allTenantStatuses {
		if s != domain.TenantActive {
			tests = append(tests, struct {
				name     string
				current  domain.Tenant
				wantCode domain.Code
			}{"current version, " + string(s), tenant(s, expected), domain.CodeTenantUpdateNotAllowed})
		}
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := domain.PatchConflict(expected, tt.current)
			assertCode(t, err, tt.wantCode)
		})
	}
}

func TestPatchConflictDetails(t *testing.T) {
	err := domain.PatchConflict(3, tenant(domain.TenantActive, 4))

	de := asDomainError(t, err)
	if de.Message != "expected version 3, current is 4" {
		t.Errorf("message = %q", de.Message)
	}
	if got := de.Details["current_version"]; got != 4 {
		t.Errorf("details.current_version = %v, want 4", got)
	}
}

func TestDeleteConflict(t *testing.T) {
	for _, s := range allTenantStatuses {
		t.Run(string(s), func(t *testing.T) {
			var want domain.Code
			if !domain.CanDelete(s) {
				want = domain.CodeTenantUpdateNotAllowed
			}
			// The version is irrelevant to DELETE: any value gives the same answer.
			assertCode(t, domain.DeleteConflict(tenant(s, 99)), want)
		})
	}
}

func tenant(s domain.TenantStatus, version int) domain.Tenant {
	return domain.Tenant{Slug: "acme", Name: "Acme", Status: s, Version: version}
}
