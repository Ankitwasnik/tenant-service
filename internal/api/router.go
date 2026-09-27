// Package api is the HTTP layer: the gin router, middleware, handlers and the
// mapping from domain errors to HTTP responses (DESIGN.md §7).
package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Ankitwasnik/tenant-service/internal/domain"
	"github.com/Ankitwasnik/tenant-service/internal/store"
)

// Pinger checks that the database is reachable. *pgxpool.Pool implements it.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Store is what the handlers need from persistence. *store.Store implements it.
type Store interface {
	CreateTenant(ctx context.Context, slug, name string) (domain.Tenant, domain.Task, error)
	PatchTenant(ctx context.Context, id uuid.UUID, name string, expectedVersion int) (domain.Tenant, domain.Task, error)
	DeleteTenant(ctx context.Context, id uuid.UUID) (domain.Tenant, domain.Task, error)
	GetTenant(ctx context.Context, id uuid.UUID) (domain.Tenant, error)
	ListTenants(ctx context.Context, p store.Page) ([]domain.Tenant, *uuid.UUID, error)
	GetTask(ctx context.Context, id uuid.UUID) (domain.Task, error)
	ListTasks(ctx context.Context, tenantID *uuid.UUID, p store.Page) ([]domain.Task, *uuid.UUID, error)
}

// Deps are the router's dependencies.
type Deps struct {
	Logger *slog.Logger
	DB     Pinger
	Store  Store
}

type handlers struct {
	store Store
}

const healthTimeout = 2 * time.Second

// NewRouter builds the gin engine with gin's defaults replaced, so that every
// response, errors included, is structured JSON (DESIGN.md §7).
func NewRouter(d Deps) *gin.Engine {
	r := gin.New() // not gin.Default(): our own logger and recovery replace gin's
	r.HandleMethodNotAllowed = true
	// No proxy in front of the service, so client IPs are never taken from headers.
	if err := r.SetTrustedProxies(nil); err != nil {
		panic(err) // only fails for malformed proxy CIDRs; nil has none
	}

	r.Use(requestLogger(d.Logger), recovery())

	r.NoRoute(func(c *gin.Context) {
		respondError(c, &domain.Error{Code: codeNotFound, Message: "no route for " + c.Request.Method + " " + c.Request.URL.Path})
	})
	r.NoMethod(func(c *gin.Context) {
		respondError(c, &domain.Error{Code: codeMethodNotAllowed, Message: c.Request.Method + " is not allowed on " + c.Request.URL.Path})
	})

	r.GET("/healthz", health(d.DB))

	h := &handlers{store: d.Store}
	v1 := r.Group("/v1")
	v1.POST("/tenants", h.createTenant)
	v1.GET("/tenants", h.listTenants)
	v1.GET("/tenants/:id", h.getTenant)
	v1.PATCH("/tenants/:id", h.patchTenant)
	v1.DELETE("/tenants/:id", h.deleteTenant)
	v1.GET("/tasks", h.listTasks)
	v1.GET("/tasks/:id", h.getTask)
	return r
}

// health reports 200 when the database answers a ping, 503 otherwise. It is
// what the compose health check (and so `make up --wait`) waits on.
func health(db Pinger) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), healthTimeout)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			loggerFrom(c).Warn("health check: database unreachable", "error", err)
			respondError(c, &domain.Error{Code: codeServiceUnavailable, Message: "database unreachable"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}
