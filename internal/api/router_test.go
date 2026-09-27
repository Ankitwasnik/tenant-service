package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Ankitwasnik/tenant-service/internal/domain"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode) // no gin debug output in test logs
	os.Exit(m.Run())
}

type fakeDB struct{ err error }

func (f fakeDB) Ping(context.Context) error { return f.err }

// newTestRouter is NewRouter plus routes that exist only to exercise the
// middleware and helpers. They are registered after NewRouter's Use calls,
// so the real middleware wraps them.
func newTestRouter(t *testing.T, db Pinger) (*gin.Engine, *bytes.Buffer) {
	t.Helper()
	logs := &bytes.Buffer{}
	r := NewRouter(Deps{Logger: slog.New(slog.NewJSONHandler(logs, nil)), DB: db})

	r.GET("/test/panic", func(*gin.Context) { panic("secret internal detail") })
	r.GET("/test/error", func(c *gin.Context) {
		respondError(c, errors.New("pq: password authentication failed for host db.internal"))
	})
	r.POST("/test/decode", func(c *gin.Context) {
		var body struct {
			Name    string `json:"name"`
			Version int    `json:"version"`
		}
		if err := decodeJSON(c, &body); err != nil {
			respondError(c, err)
			return
		}
		c.JSON(http.StatusOK, body)
	})
	return r, logs
}

func TestUnknownRouteIsJSON404(t *testing.T) {
	r, _ := newTestRouter(t, fakeDB{})
	rec := serve(r, http.MethodGet, "/v1/nope", "")

	assertError(t, rec, http.StatusNotFound, "not_found")
}

func TestWrongMethodIsJSON405(t *testing.T) {
	r, _ := newTestRouter(t, fakeDB{})
	rec := serve(r, http.MethodDelete, "/healthz", "")

	assertError(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
}

func TestPanicIsJSON500WithoutDetails(t *testing.T) {
	r, logs := newTestRouter(t, fakeDB{})
	rec := serve(r, http.MethodGet, "/test/panic", "")

	body := assertError(t, rec, http.StatusInternalServerError, "internal_error")
	for _, leak := range []string{"secret internal detail", "goroutine", ".go:"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("response leaks %q: %s", leak, rec.Body.String())
		}
	}
	if body.Error.Message != "internal server error" {
		t.Errorf("message = %q", body.Error.Message)
	}
	// The details go to the server log instead, with a stack trace.
	if !strings.Contains(logs.String(), "secret internal detail") || !strings.Contains(logs.String(), "goroutine") {
		t.Errorf("panic not logged with its stack: %s", logs.String())
	}
}

func TestNonDomainErrorIsGeneric500(t *testing.T) {
	r, logs := newTestRouter(t, fakeDB{})
	rec := serve(r, http.MethodGet, "/test/error", "")

	assertError(t, rec, http.StatusInternalServerError, "internal_error")
	if strings.Contains(rec.Body.String(), "db.internal") {
		t.Errorf("response leaks the underlying error: %s", rec.Body.String())
	}
	if !strings.Contains(logs.String(), "db.internal") {
		t.Errorf("underlying error not logged: %s", logs.String())
	}
}

func TestRespondErrorStatusMapping(t *testing.T) {
	id := uuid.New()
	tests := []struct {
		err    error
		status int
	}{
		{domain.TenantNotFound(id), http.StatusNotFound},
		{domain.TaskNotFound(id), http.StatusNotFound},
		{domain.TenantAlreadyExists("acme"), http.StatusConflict},
		{domain.TenantUpdateNotAllowed("PATCH", domain.TenantUpdating), http.StatusConflict},
		{domain.TenantVersionConflict(1, 2), http.StatusConflict},
		{(domain.FieldErrors{"name": "must not be empty"}).Err(), http.StatusBadRequest},
		// Wrapped domain errors keep their code.
		{fmt.Errorf("patch: %w", domain.TenantVersionConflict(1, 2)), http.StatusConflict},
		// A domain error with an unmapped code is a bug: 500, not a guess.
		{&domain.Error{Code: "made_up", Message: "?"}, http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.err.Error(), func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			respondError(c, tt.err)

			wantCode := domain.CodeOf(tt.err)
			if tt.status == http.StatusInternalServerError {
				wantCode = "internal_error"
			}
			assertError(t, rec, tt.status, wantCode)
		})
	}
}

func TestErrorBodyShape(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	respondError(c, domain.TenantVersionConflict(3, 4))

	want := `{"error":{"code":"tenant_version_conflict","message":"expected version 3, current is 4","details":{"current_version":4}}}`
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Fatalf("body =\n%s\nwant\n%s", got, want)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestHealthz(t *testing.T) {
	t.Run("database up", func(t *testing.T) {
		r, _ := newTestRouter(t, fakeDB{})
		rec := serve(r, http.MethodGet, "/healthz", "")
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"status":"ok"}` {
			t.Fatalf("got %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("database down", func(t *testing.T) {
		r, _ := newTestRouter(t, fakeDB{err: errors.New("connection refused")})
		rec := serve(r, http.MethodGet, "/healthz", "")
		assertError(t, rec, http.StatusServiceUnavailable, "service_unavailable")
	})
}

func TestDecodeJSON(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantField string // "" means success
		wantMsg   string
	}{
		{"valid", `{"name":"Acme","version":2}`, "", ""},
		{"valid with surrounding whitespace", " \n{\"name\":\"Acme\"}\n ", "", ""},
		{"unknown field", `{"name":"Acme","slug":"acme"}`, "slug", "is not a known field"},
		{"wrong type", `{"name":"Acme","version":"2"}`, "version", "must be a number"},
		{"wrong type, string field", `{"name":7}`, "name", "must be a string"},
		{"empty body", ``, "body", "must be a JSON object"},
		{"array instead of object", `[1,2]`, "body", "must be a JSON object"},
		{"malformed", `{"name":`, "body", "is not valid JSON"},
		{"syntax error", `{name:"Acme"}`, "body", "is not valid JSON"},
		{"trailing data", `{"name":"Acme"}{"name":"Evil"}`, "body", "must contain a single JSON object"},
		{"too large", `{"name":"` + strings.Repeat("a", maxBodyBytes) + `"}`, "body", fmt.Sprintf("must be at most %d bytes", maxBodyBytes)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := newTestRouter(t, fakeDB{})
			rec := serve(r, http.MethodPost, "/test/decode", tt.body)

			if tt.wantField == "" {
				if rec.Code != http.StatusOK {
					t.Fatalf("got %d %s, want 200", rec.Code, rec.Body.String())
				}
				return
			}
			body := assertError(t, rec, http.StatusBadRequest, "validation_error")
			if got := body.Error.Details[tt.wantField]; got != tt.wantMsg {
				t.Fatalf("details[%q] = %v, want %q (details %v)", tt.wantField, got, tt.wantMsg, body.Error.Details)
			}
		})
	}
}

func TestRequestID(t *testing.T) {
	r, logs := newTestRouter(t, fakeDB{})

	t.Run("generated when absent", func(t *testing.T) {
		rec := serve(r, http.MethodGet, "/v1/nope", "")
		if _, err := uuid.Parse(rec.Header().Get(requestIDHeader)); err != nil {
			t.Fatalf("X-Request-ID = %q, want a UUID", rec.Header().Get(requestIDHeader))
		}
	})

	t.Run("client id kept and logged", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/nope", nil)
		req.Header.Set(requestIDHeader, "client-abc.123")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if got := rec.Header().Get(requestIDHeader); got != "client-abc.123" {
			t.Fatalf("X-Request-ID = %q", got)
		}
		if !strings.Contains(logs.String(), `"request_id":"client-abc.123"`) {
			t.Fatalf("request id not in logs: %s", logs.String())
		}
	})

	t.Run("malformed client id replaced", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/nope", nil)
		req.Header.Set(requestIDHeader, "bad id\nwith newline")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if _, err := uuid.Parse(rec.Header().Get(requestIDHeader)); err != nil {
			t.Fatalf("X-Request-ID = %q, want a fresh UUID", rec.Header().Get(requestIDHeader))
		}
	})
}

func TestHealthzSuccessNotLogged(t *testing.T) {
	r, logs := newTestRouter(t, fakeDB{})
	serve(r, http.MethodGet, "/healthz", "")
	if logs.Len() != 0 {
		t.Fatalf("successful health check was logged: %s", logs.String())
	}
}

func serve(r http.Handler, method, path, body string) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func assertError(t *testing.T, rec *httptest.ResponseRecorder, status int, code domain.Code) errorBody {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, status, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want JSON (body %s)", ct, rec.Body.String())
	}
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not the error JSON: %v: %s", err, rec.Body.String())
	}
	if body.Error.Code != code {
		t.Fatalf("code = %q, want %q", body.Error.Code, code)
	}
	if body.Error.Message == "" {
		t.Fatal("error message is empty")
	}
	return body
}
