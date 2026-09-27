package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	requestIDHeader = "X-Request-ID"
	loggerKey       = "logger"
)

// A client-supplied request id is kept only if it looks like one, so it can't
// inject arbitrary text into the logs.
var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// requestLogger gives every request an id (the client's X-Request-ID if it is
// well-formed, else a new UUID), echoes it in the response, stores a logger
// carrying it for handlers, and logs one line per request.
func requestLogger(base *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()

		id := c.GetHeader(requestIDHeader)
		if !validRequestID.MatchString(id) {
			id = uuid.NewString()
		}
		c.Header(requestIDHeader, id)
		logger := base.With("request_id", id)
		c.Set(loggerKey, logger)

		c.Next()

		status := c.Writer.Status()
		// The compose health check polls /healthz every few seconds; only
		// log it when it fails.
		if c.FullPath() == "/healthz" && status == http.StatusOK {
			return
		}
		logger.Info("request",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"route", c.FullPath(),
			"status", status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	}
}

// recovery turns a panic into a 500 internal_error. The panic value and stack
// are logged server-side; the client never sees them (DESIGN.md §7).
func recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// net/http uses this panic to abort a response on purpose; let it through.
			if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(rec)
			}
			loggerFrom(c).Error("panic", "panic", fmt.Sprint(rec), "stack", string(debug.Stack()))
			if c.Writer.Written() {
				c.Abort() // too late for an error body; the response is already underway
				return
			}
			respondError(c, fmt.Errorf("panic: %v", rec))
		}()
		c.Next()
	}
}

// loggerFrom returns the request's logger (with its request id), or the
// default logger if requestLogger didn't run.
func loggerFrom(c *gin.Context) *slog.Logger {
	if l, ok := c.Get(loggerKey); ok {
		if logger, ok := l.(*slog.Logger); ok {
			return logger
		}
	}
	return slog.Default()
}
