package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Ankitwasnik/tenant-service/internal/domain"
)

// GET /v1/tasks[?tenant_id=]: newest first. An unknown tenant_id is an empty
// page, not tenant_not_found (DESIGN.md §7).
func (h *handlers) listTasks(c *gin.Context) {
	fe := domain.FieldErrors{}
	page := parsePage(c, fe)
	tenantID := parseQueryUUID(c, "tenant_id", "must be a tenant id (UUID)", fe)
	if err := fe.Err(); err != nil {
		respondError(c, err)
		return
	}

	tasks, next, err := h.store.ListTasks(c.Request.Context(), tenantID, page)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, newPageView(tasks, next, newTaskView))
}

// GET /v1/tasks/:id
func (h *handlers) getTask(c *gin.Context) {
	id, ok := pathID(c, domain.TaskNotFound)
	if !ok {
		return
	}
	task, err := h.store.GetTask(c.Request.Context(), id)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, newTaskView(task))
}
