package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Ankitwasnik/tenant-service/internal/domain"
	"github.com/Ankitwasnik/tenant-service/internal/store"
	"github.com/Ankitwasnik/tenant-service/internal/testutil"
)

// Endpoint tests: real router, real store, real Postgres (one database per test).

var timestampFormat = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`)

func TestCreateTenant(t *testing.T) {
	t.Parallel()
	r, _ := newAPI(t)

	rec := serve(r, http.MethodPost, "/v1/tenants", `{"slug":"acme","name":"  Acme Corp  "}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	body := decode[mutationView](t, rec)

	if got := rec.Header().Get("Location"); got != "/v1/tenants/"+body.Tenant.ID.String() {
		t.Errorf("Location = %q", got)
	}
	tn := body.Tenant
	if tn.Slug != "acme" || tn.Name != "Acme Corp" || tn.Status != domain.TenantProvisioning || tn.Version != 1 {
		t.Errorf("tenant = %+v (name must be trimmed)", tn)
	}
	task := body.Task
	if task.Type != domain.TaskDeploy || task.Status != domain.TaskAccepted || task.TenantID != tn.ID {
		t.Errorf("task = %+v", task)
	}
	for _, ts := range []string{tn.CreatedAt, tn.UpdatedAt, task.CreatedAt, task.UpdatedAt} {
		if !timestampFormat.MatchString(ts) {
			t.Errorf("timestamp %q is not ISO 8601 with milliseconds and Z", ts)
		}
	}
	// No error field on a task that hasn't failed.
	if strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("body has an error field: %s", rec.Body.String())
	}
}

func TestCreateTenantRejected(t *testing.T) {
	t.Parallel()
	r, _ := newAPI(t)
	mustCreate(t, r, "acme")

	tests := []struct {
		name        string
		body        string
		status      int
		code        domain.Code
		wantDetails map[string]string
	}{
		{
			name: "invalid slug and name together", body: `{"slug":"A","name":"  "}`,
			status: http.StatusBadRequest, code: domain.CodeValidation,
			wantDetails: map[string]string{"slug": "must be 3-28 characters", "name": "must not be empty"},
		},
		{
			name: "missing fields", body: `{}`,
			status: http.StatusBadRequest, code: domain.CodeValidation,
			wantDetails: map[string]string{"slug": "must be 3-28 characters", "name": "must not be empty"},
		},
		{
			name: "unknown field", body: `{"slug":"new-one","name":"New","status":"active"}`,
			status: http.StatusBadRequest, code: domain.CodeValidation,
			wantDetails: map[string]string{"status": "is not a known field"},
		},
		{
			name: "duplicate slug", body: `{"slug":"acme","name":"Other"}`,
			status: http.StatusConflict, code: domain.CodeTenantAlreadyExists,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(r, http.MethodPost, "/v1/tenants", tt.body)
			body := assertError(t, rec, tt.status, tt.code)
			for field, msg := range tt.wantDetails {
				if body.Error.Details[field] != msg {
					t.Errorf("details[%q] = %v, want %q", field, body.Error.Details[field], msg)
				}
			}
		})
	}
}

func TestGetTenant(t *testing.T) {
	t.Parallel()
	r, _ := newAPI(t)
	created := mustCreate(t, r, "acme")

	rec := serve(r, http.MethodGet, "/v1/tenants/"+created.Tenant.ID.String(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := decode[tenantView](t, rec); got != created.Tenant {
		t.Errorf("GET = %+v, want %+v", got, created.Tenant)
	}

	assertError(t, serve(r, http.MethodGet, "/v1/tenants/"+uuid.NewString(), ""), http.StatusNotFound, domain.CodeTenantNotFound)
	// A malformed id can't name a tenant: the same 404, not a 400.
	assertError(t, serve(r, http.MethodGet, "/v1/tenants/not-a-uuid", ""), http.StatusNotFound, domain.CodeTenantNotFound)
}

func TestPatchTenant(t *testing.T) {
	t.Parallel()
	r, pool := newAPI(t)
	created := mustCreate(t, r, "acme")
	testutil.FinishTask(t, pool, created.Tenant.ID, domain.TenantActive) // version 2

	rec := serve(r, http.MethodPatch, "/v1/tenants/"+created.Tenant.ID.String(), `{"name":" Acme Renamed ","version":2}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	body := decode[mutationView](t, rec)
	if body.Tenant.Name != "Acme Renamed" || body.Tenant.Status != domain.TenantUpdating || body.Tenant.Version != 3 {
		t.Errorf("tenant = %+v", body.Tenant)
	}
	if body.Task.Type != domain.TaskUpdate || body.Task.Status != domain.TaskAccepted {
		t.Errorf("task = %+v", body.Task)
	}
}

func TestPatchTenantRejected(t *testing.T) {
	t.Parallel()
	r, pool := newAPI(t)
	active := mustCreate(t, r, "active").Tenant
	testutil.FinishTask(t, pool, active.ID, domain.TenantActive) // version 2
	provisioning := mustCreate(t, r, "provisioning").Tenant      // version 1

	tests := []struct {
		name        string
		id          string
		body        string
		status      int
		code        domain.Code
		wantDetails map[string]any
	}{
		{
			"stale version", active.ID.String(), `{"name":"X","version":1}`,
			http.StatusConflict, domain.CodeTenantVersionConflict,
			map[string]any{"current_version": float64(2)},
		},
		{
			"not active", provisioning.ID.String(), `{"name":"X","version":1}`,
			http.StatusConflict, domain.CodeTenantUpdateNotAllowed,
			map[string]any{"status": "provisioning"},
		},
		{
			"unknown tenant", uuid.NewString(), `{"name":"X","version":1}`,
			http.StatusNotFound, domain.CodeTenantNotFound, nil,
		},
		{
			"malformed id", "nope", `{"name":"X","version":1}`,
			http.StatusNotFound, domain.CodeTenantNotFound, nil,
		},
		{
			"missing version", active.ID.String(), `{"name":"X"}`,
			http.StatusBadRequest, domain.CodeValidation,
			map[string]any{"version": "is required"},
		},
		{
			"missing name", active.ID.String(), `{"version":2}`,
			http.StatusBadRequest, domain.CodeValidation,
			map[string]any{"name": "is required"},
		},
		{
			"empty body", active.ID.String(), `{}`,
			http.StatusBadRequest, domain.CodeValidation,
			map[string]any{"name": "is required", "version": "is required"},
		},
		{
			"blank name", active.ID.String(), `{"name":"   ","version":2}`,
			http.StatusBadRequest, domain.CodeValidation,
			map[string]any{"name": "must not be empty"},
		},
		{
			"version zero", active.ID.String(), `{"name":"X","version":0}`,
			http.StatusBadRequest, domain.CodeValidation,
			map[string]any{"version": "must be a positive integer"},
		},
		{
			"version not a number", active.ID.String(), `{"name":"X","version":"2"}`,
			http.StatusBadRequest, domain.CodeValidation,
			map[string]any{"version": "must be a number"},
		},
		{
			"slug is immutable", active.ID.String(), `{"name":"X","version":2,"slug":"other"}`,
			http.StatusBadRequest, domain.CodeValidation,
			map[string]any{"slug": "cannot be changed"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(r, http.MethodPatch, "/v1/tenants/"+tt.id, tt.body)
			body := assertError(t, rec, tt.status, tt.code)
			for field, want := range tt.wantDetails {
				if body.Error.Details[field] != want {
					t.Errorf("details[%q] = %v, want %v", field, body.Error.Details[field], want)
				}
			}
		})
	}

	// None of it changed the tenant.
	got := decode[tenantView](t, serve(r, http.MethodGet, "/v1/tenants/"+active.ID.String(), ""))
	if got.Name != active.Name || got.Version != 2 || got.Status != domain.TenantActive {
		t.Errorf("rejected PATCHes changed the tenant: %+v", got)
	}
}

func TestDeleteTenant(t *testing.T) {
	t.Parallel()
	r, pool := newAPI(t)

	active := mustCreate(t, r, "active").Tenant
	testutil.FinishTask(t, pool, active.ID, domain.TenantActive)

	rec := serve(r, http.MethodDelete, "/v1/tenants/"+active.ID.String(), "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	body := decode[mutationView](t, rec)
	if body.Tenant.Status != domain.TenantDestroying || body.Task.Type != domain.TaskDestroy {
		t.Errorf("body = %+v", body)
	}

	// A second DELETE: the tenant is destroying now, so it is not allowed.
	assertError(t, serve(r, http.MethodDelete, "/v1/tenants/"+active.ID.String(), ""), http.StatusConflict, domain.CodeTenantUpdateNotAllowed)
	assertError(t, serve(r, http.MethodDelete, "/v1/tenants/"+uuid.NewString(), ""), http.StatusNotFound, domain.CodeTenantNotFound)
	assertError(t, serve(r, http.MethodDelete, "/v1/tenants/nope", ""), http.StatusNotFound, domain.CodeTenantNotFound)
}

func TestListTenantsPagination(t *testing.T) {
	t.Parallel()
	r, _ := newAPI(t)
	var slugs []string // newest first, as the API returns them
	for _, s := range []string{"t-one", "t-two", "t-three"} {
		mustCreate(t, r, s)
		slugs = append([]string{s}, slugs...)
	}

	first := decode[pageView[tenantView]](t, serve(r, http.MethodGet, "/v1/tenants?limit=2", ""))
	if len(first.Items) != 2 || first.NextCursor == nil {
		t.Fatalf("first page: %d items, next_cursor %v; want 2 and a cursor", len(first.Items), first.NextCursor)
	}
	second := decode[pageView[tenantView]](t, serve(r, http.MethodGet, "/v1/tenants?limit=2&cursor="+first.NextCursor.String(), ""))
	if len(second.Items) != 1 || second.NextCursor != nil {
		t.Fatalf("second page: %d items, next_cursor %v; want 1 and null", len(second.Items), second.NextCursor)
	}

	got := []string{first.Items[0].Slug, first.Items[1].Slug, second.Items[0].Slug}
	for i := range slugs {
		if got[i] != slugs[i] {
			t.Fatalf("order = %v, want newest first %v", got, slugs)
		}
	}

	// The last page says so explicitly: "next_cursor": null.
	raw := serve(r, http.MethodGet, "/v1/tenants", "").Body.String()
	if !strings.Contains(raw, `"next_cursor":null`) {
		t.Errorf("default page body lacks next_cursor null: %s", raw)
	}
}

func TestListParamsRejected(t *testing.T) {
	t.Parallel()
	r, _ := newAPI(t)

	tests := []struct {
		path  string
		field string
	}{
		{"/v1/tenants?limit=0", "limit"},
		{"/v1/tenants?limit=201", "limit"},
		{"/v1/tenants?limit=ten", "limit"},
		{"/v1/tenants?cursor=nope", "cursor"},
		{"/v1/tasks?tenant_id=nope", "tenant_id"},
		{"/v1/tasks?limit=-1", "limit"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			body := assertError(t, serve(r, http.MethodGet, tt.path, ""), http.StatusBadRequest, domain.CodeValidation)
			if _, ok := body.Error.Details[tt.field]; !ok {
				t.Errorf("details %v don't name %q", body.Error.Details, tt.field)
			}
		})
	}

	t.Run("every bad parameter reported at once", func(t *testing.T) {
		body := assertError(t, serve(r, http.MethodGet, "/v1/tasks?limit=0&cursor=x&tenant_id=y", ""), http.StatusBadRequest, domain.CodeValidation)
		if len(body.Error.Details) != 3 {
			t.Errorf("details = %v, want limit, cursor and tenant_id", body.Error.Details)
		}
	})

	t.Run("limit 200 is allowed", func(t *testing.T) {
		if rec := serve(r, http.MethodGet, "/v1/tenants?limit=200", ""); rec.Code != http.StatusOK {
			t.Errorf("status = %d, body %s", rec.Code, rec.Body.String())
		}
	})
}

func TestTasks(t *testing.T) {
	t.Parallel()
	r, pool := newAPI(t)
	a := mustCreate(t, r, "tenant-a")
	testutil.FinishTask(t, pool, a.Tenant.ID, domain.TenantActive)
	patched := decode[mutationView](t, serve(r, http.MethodPatch, "/v1/tenants/"+a.Tenant.ID.String(), `{"name":"Renamed","version":2}`))
	mustCreate(t, r, "tenant-b")

	t.Run("list for one tenant", func(t *testing.T) {
		page := decode[pageView[taskView]](t, serve(r, http.MethodGet, "/v1/tasks?tenant_id="+a.Tenant.ID.String(), ""))
		if len(page.Items) != 2 || page.Items[0].ID != patched.Task.ID || page.Items[1].ID != a.Task.ID {
			t.Fatalf("items = %+v, want the update then the deploy", page.Items)
		}
		if page.Items[1].Status != domain.TaskDone {
			t.Errorf("deploy status = %q, want done", page.Items[1].Status)
		}
	})

	t.Run("list all", func(t *testing.T) {
		page := decode[pageView[taskView]](t, serve(r, http.MethodGet, "/v1/tasks", ""))
		if len(page.Items) != 3 {
			t.Fatalf("got %d tasks, want 3", len(page.Items))
		}
	})

	t.Run("unknown tenant is an empty page", func(t *testing.T) {
		rec := serve(r, http.MethodGet, "/v1/tasks?tenant_id="+uuid.NewString(), "")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"items":[]`) {
			t.Fatalf("got %d %s, want 200 with items []", rec.Code, rec.Body.String())
		}
	})

	t.Run("get one", func(t *testing.T) {
		rec := serve(r, http.MethodGet, "/v1/tasks/"+a.Task.ID.String(), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		got := decode[taskView](t, rec)
		if got.ID != a.Task.ID || got.Type != domain.TaskDeploy || got.TenantID != a.Tenant.ID {
			t.Errorf("task = %+v", got)
		}
	})

	t.Run("not found", func(t *testing.T) {
		assertError(t, serve(r, http.MethodGet, "/v1/tasks/"+uuid.NewString(), ""), http.StatusNotFound, domain.CodeTaskNotFound)
		assertError(t, serve(r, http.MethodGet, "/v1/tasks/nope", ""), http.StatusNotFound, domain.CodeTaskNotFound)
	})

	t.Run("read-only", func(t *testing.T) {
		assertError(t, serve(r, http.MethodDelete, "/v1/tasks/"+a.Task.ID.String(), ""), http.StatusMethodNotAllowed, codeMethodNotAllowed)
		assertError(t, serve(r, http.MethodPost, "/v1/tasks", `{}`), http.StatusMethodNotAllowed, codeMethodNotAllowed)
	})
}

func TestTenantMethodNotAllowed(t *testing.T) {
	t.Parallel()
	r, _ := newAPI(t)
	// PUT isn't part of the API: partial updates are PATCH (DESIGN.md §7).
	assertError(t, serve(r, http.MethodPut, "/v1/tenants/"+uuid.NewString(), `{}`), http.StatusMethodNotAllowed, codeMethodNotAllowed)
}

// ---- helpers ----

func newAPI(t *testing.T) (*gin.Engine, *pgxpool.Pool) {
	t.Helper()
	pool, err := store.Open(context.Background(), testutil.NewDatabase(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(pool.Close)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return NewRouter(Deps{Logger: logger, DB: pool, Store: store.New(pool)}), pool
}

func mustCreate(t *testing.T, r http.Handler, slug string) mutationView {
	t.Helper()
	rec := serve(r, http.MethodPost, "/v1/tenants", `{"slug":"`+slug+`","name":"Tenant `+slug+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create %q: %d %s", slug, rec.Code, rec.Body.String())
	}
	return decode[mutationView](t, rec)
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %T: %v: %s", v, err, rec.Body.String())
	}
	return v
}
