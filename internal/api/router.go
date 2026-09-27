// Package api is the HTTP layer: the gin router, middleware, handlers and the
// mapping from domain errors to HTTP responses (DESIGN.md §7).
package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Ankitwasnik/tenant-service/internal/domain"
)

// Pinger checks that the database is reachable. *pgxpool.Pool implements it.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Deps are the router's dependencies.
type Deps struct {
	Logger *slog.Logger
	DB     Pinger
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
