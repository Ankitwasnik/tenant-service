package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Ankitwasnik/tenant-service/internal/domain"
)

type createTenantRequest struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// patchTenantRequest uses pointers so that a missing field can be told apart
// from an empty one. Slug is accepted only so that sending it gets a clear
// "cannot be changed" rather than "not a known field".
type patchTenantRequest struct {
	Name    *string `json:"name"`
	Version *int    `json:"version"`
	Slug    *string `json:"slug"`
}

// POST /v1/tenants → 201: the tenant exists right away, in provisioning.
func (h *handlers) createTenant(c *gin.Context) {
	var req createTenantRequest
	if err := decodeJSON(c, &req); err != nil {
		respondError(c, err)
		return
	}
	name, err := domain.ValidateCreate(req.Slug, req.Name)
	if err != nil {
		respondError(c, err)
		return
	}

	tenant, task, err := h.store.CreateTenant(c.Request.Context(), req.Slug, name)
	if err != nil {
		respondError(c, err)
		return
	}
	c.Header("Location", "/v1/tenants/"+tenant.ID.String())
	c.JSON(http.StatusCreated, mutationView{Tenant: newTenantView(tenant), Task: newTaskView(task)})
}

// GET /v1/tenants: every status, destroyed included, newest first.
func (h *handlers) listTenants(c *gin.Context) {
	fe := domain.FieldErrors{}
	page := parsePage(c, fe)
	if err := fe.Err(); err != nil {
		respondError(c, err)
		return
	}

	tenants, next, err := h.store.ListTenants(c.Request.Context(), page)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, newPageView(tenants, next, newTenantView))
}

// GET /v1/tenants/:id
func (h *handlers) getTenant(c *gin.Context) {
	id, ok := pathID(c, domain.TenantNotFound)
	if !ok {
		return
	}
	tenant, err := h.store.GetTenant(c.Request.Context(), id)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, newTenantView(tenant))
}

// PATCH /v1/tenants/:id → 202: the rename completes asynchronously.
func (h *handlers) patchTenant(c *gin.Context) {
	id, ok := pathID(c, domain.TenantNotFound)
	if !ok {
		return
	}
	var req patchTenantRequest
	if err := decodeJSON(c, &req); err != nil {
		respondError(c, err)
		return
	}

	fe := domain.FieldErrors{}
	var name string
	if req.Name == nil {
		fe.Add("name", "is required")
	} else if n, err := domain.NormalizeName(*req.Name); err != nil {
		fe.Add("name", err.Error())
	} else {
		name = n
	}
	switch {
	case req.Version == nil:
		fe.Add("version", "is required")
	case *req.Version < 1:
		fe.Add("version", "must be a positive integer")
	}
	if req.Slug != nil {
		fe.Add("slug", "cannot be changed")
	}
	if err := fe.Err(); err != nil {
		respondError(c, err)
		return
	}

	tenant, task, err := h.store.PatchTenant(c.Request.Context(), id, name, *req.Version)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, mutationView{Tenant: newTenantView(tenant), Task: newTaskView(task)})
}

// DELETE /v1/tenants/:id → 202: the tenant is destroyed asynchronously, and
// stays readable as destroyed afterwards.
func (h *handlers) deleteTenant(c *gin.Context) {
	id, ok := pathID(c, domain.TenantNotFound)
	if !ok {
		return
	}
	tenant, task, err := h.store.DeleteTenant(c.Request.Context(), id)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, mutationView{Tenant: newTenantView(tenant), Task: newTaskView(task)})
}
