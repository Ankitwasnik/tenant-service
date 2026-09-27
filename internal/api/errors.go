package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Ankitwasnik/tenant-service/internal/domain"
)

// Codes the API adds beyond the domain's (DESIGN.md §7).
const (
	codeNotFound           domain.Code = "not_found" // unknown route
	codeMethodNotAllowed   domain.Code = "method_not_allowed"
	codeInternal           domain.Code = "internal_error"
	codeServiceUnavailable domain.Code = "service_unavailable" // /healthz with the database down
)

// statusFor maps a code to its HTTP status. It is the single place where
// that mapping lives; the README documents the same table.
var statusFor = map[domain.Code]int{
	domain.CodeValidation:             http.StatusBadRequest,
	domain.CodeTenantNotFound:         http.StatusNotFound,
	domain.CodeTaskNotFound:           http.StatusNotFound,
	domain.CodeTenantAlreadyExists:    http.StatusConflict,
	domain.CodeTenantUpdateNotAllowed: http.StatusConflict,
	domain.CodeTenantVersionConflict:  http.StatusConflict,
	codeNotFound:                      http.StatusNotFound,
	codeMethodNotAllowed:              http.StatusMethodNotAllowed,
	codeInternal:                      http.StatusInternalServerError,
	codeServiceUnavailable:            http.StatusServiceUnavailable,
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    domain.Code    `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// respondError writes err as the structured error body and aborts the
// request. Every error response goes through here; handlers never write
// error JSON themselves (DESIGN.md §7).
//
// A *domain.Error is sent as-is. Anything else is an internal error: it is
// logged with the request id, and the client gets a generic message, so no
// internals (SQL, hostnames, stack traces) leak.
func respondError(c *gin.Context, err error) {
	var de *domain.Error
	if !errors.As(err, &de) || statusFor[de.Code] == 0 {
		loggerFrom(c).Error("internal error", "error", err)
		de = &domain.Error{Code: codeInternal, Message: "internal server error"}
	}
	c.AbortWithStatusJSON(statusFor[de.Code], errorBody{Error: errorDetail{
		Code:    de.Code,
		Message: de.Message,
		Details: de.Details,
	}})
}
