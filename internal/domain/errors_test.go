package domain_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/Ankitwasnik/tenant-service/internal/domain"
)

func TestConstructorCodes(t *testing.T) {
	id := uuid.MustParse("01920000-0000-7000-8000-000000000001")

	tests := []struct {
		err  error
		want domain.Code
	}{
		{domain.TenantNotFound(id), "tenant_not_found"},
		{domain.TenantAlreadyExists("acme"), "tenant_already_exists"},
		{domain.TenantUpdateNotAllowed("PATCH", domain.TenantUpdating), "tenant_update_not_allowed"},
		{domain.TenantVersionConflict(1, 2), "tenant_version_conflict"},
		{domain.TaskNotFound(id), "task_not_found"},
	}
	for _, tt := range tests {
		t.Run(string(tt.want), func(t *testing.T) {
			// The literal strings are the contract: a renamed constant must fail here.
			assertCode(t, tt.err, tt.want)
		})
	}
}

func TestCodeOf(t *testing.T) {
	wrapped := fmt.Errorf("patch tenant: %w", domain.TenantVersionConflict(1, 2))
	if got := domain.CodeOf(wrapped); got != domain.CodeTenantVersionConflict {
		t.Errorf("CodeOf(wrapped) = %q", got)
	}
	if got := domain.CodeOf(errors.New("boom")); got != "" {
		t.Errorf("CodeOf(plain error) = %q, want empty", got)
	}
	if got := domain.CodeOf(nil); got != "" {
		t.Errorf("CodeOf(nil) = %q, want empty", got)
	}
}

func TestErrorString(t *testing.T) {
	err := domain.TenantUpdateNotAllowed("DELETE", domain.TenantProvisioning)
	want := "tenant_update_not_allowed: DELETE is not allowed while the tenant is provisioning"
	if err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
	if got := err.Details["status"]; got != "provisioning" {
		t.Errorf("details.status = %v", got)
	}
}

func TestFieldErrors(t *testing.T) {
	t.Run("none is a true nil", func(t *testing.T) {
		if err := (domain.FieldErrors{}).Err(); err != nil {
			t.Fatalf("Err() = %v, want nil", err)
		}
	})

	t.Run("lists every field", func(t *testing.T) {
		fe := domain.FieldErrors{}
		fe.Add("slug", "must match the slug pattern")
		fe.Add("name", "must not be empty")
		fe.Add("name", "ignored: first message wins")

		de := asDomainError(t, fe.Err())
		if de.Code != domain.CodeValidation {
			t.Errorf("code = %q", de.Code)
		}
		if de.Message != "invalid name, slug" {
			t.Errorf("message = %q, want fields sorted", de.Message)
		}
		if len(de.Details) != 2 || de.Details["name"] != "must not be empty" || de.Details["slug"] != "must match the slug pattern" {
			t.Errorf("details = %v", de.Details)
		}
	})
}

func assertCode(t *testing.T, err error, want domain.Code) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatalf("got error %v, want nil", err)
		}
		return
	}
	if got := domain.CodeOf(err); got != want {
		t.Fatalf("code = %q (err %v), want %q", got, err, want)
	}
}

func asDomainError(t *testing.T, err error) *domain.Error {
	t.Helper()
	var de *domain.Error
	if !errors.As(err, &de) {
		t.Fatalf("error %v is not a *domain.Error", err)
	}
	return de
}
