package domain

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/google/uuid"
)

// Code is a machine-readable error code. The contract codes are fixed by the
// brief; clients branch on them.
type Code string

const (
	CodeTenantNotFound         Code = "tenant_not_found"
	CodeTenantAlreadyExists    Code = "tenant_already_exists"
	CodeTenantUpdateNotAllowed Code = "tenant_update_not_allowed"
	CodeTenantVersionConflict  Code = "tenant_version_conflict"
	CodeTaskNotFound           Code = "task_not_found"
	CodeValidation             Code = "validation_error"
)

// Error is a domain error: a code, a human-readable message and optional
// structured details. The API maps Code to an HTTP status (DESIGN.md §7).
type Error struct {
	Code    Code
	Message string
	Details map[string]any
}

func (e *Error) Error() string {
	return string(e.Code) + ": " + e.Message
}

// CodeOf returns the code of the first *Error in err's chain, or "" if there
// is none.
func CodeOf(err error) Code {
	var de *Error
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}

func TenantNotFound(id uuid.UUID) *Error {
	return &Error{Code: CodeTenantNotFound, Message: fmt.Sprintf("tenant %s not found", id)}
}

func TenantAlreadyExists(slug string) *Error {
	return &Error{
		Code:    CodeTenantAlreadyExists,
		Message: fmt.Sprintf("a tenant with slug %q already exists", slug),
		Details: map[string]any{"slug": slug},
	}
}

// TenantUpdateNotAllowed reports that operation (PATCH or DELETE) is not
// allowed from the tenant's current status.
func TenantUpdateNotAllowed(operation string, current TenantStatus) *Error {
	return &Error{
		Code:    CodeTenantUpdateNotAllowed,
		Message: fmt.Sprintf("%s is not allowed while the tenant is %s", operation, current),
		Details: map[string]any{"status": string(current)},
	}
}

func TenantVersionConflict(expected, current int) *Error {
	return &Error{
		Code:    CodeTenantVersionConflict,
		Message: fmt.Sprintf("expected version %d, current is %d", expected, current),
		Details: map[string]any{"current_version": current},
	}
}

func TaskNotFound(id uuid.UUID) *Error {
	return &Error{Code: CodeTaskNotFound, Message: fmt.Sprintf("task %s not found", id)}
}

// FieldErrors collects per-field validation problems: field name → message.
type FieldErrors map[string]string

// Add records a problem with field. The first message for a field wins.
func (f FieldErrors) Add(field, message string) {
	if _, ok := f[field]; !ok {
		f[field] = message
	}
}

// Err returns a validation_error listing every field, or nil if there are none.
// It returns error rather than *Error so that "no problems" is a true nil.
func (f FieldErrors) Err() error {
	if len(f) == 0 {
		return nil
	}
	fields := slices.Sorted(maps.Keys(f))
	details := make(map[string]any, len(f))
	for field, msg := range f {
		details[field] = msg
	}
	return &Error{
		Code:    CodeValidation,
		Message: "invalid " + strings.Join(fields, ", "),
		Details: details,
	}
}
