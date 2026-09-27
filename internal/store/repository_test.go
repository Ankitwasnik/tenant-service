package store_test

import (
	"context"
	"encoding/json"
	"math"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Ankitwasnik/tenant-service/internal/domain"
	"github.com/Ankitwasnik/tenant-service/internal/store"
	"github.com/Ankitwasnik/tenant-service/internal/testutil"
)

func TestCreateTenant(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newRepo(t)

	tenant, task, err := s.CreateTenant(ctx, "acme", "Acme Corp")
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}

	if tenant.Slug != "acme" || tenant.Name != "Acme Corp" || tenant.Status != domain.TenantProvisioning || tenant.Version != 1 {
		t.Errorf("tenant = %+v", tenant)
	}
	if task.TenantID != tenant.ID || task.Type != domain.TaskDeploy || task.Status != domain.TaskAccepted || task.Error != "" {
		t.Errorf("task = %+v", task)
	}

	// Exactly one event, carrying what the brief requires.
	rows := outbox(t, pool)
	if len(rows) != 1 {
		t.Fatalf("outbox has %d rows, want 1", len(rows))
	}
	row := rows[0]
	if row.TaskID != task.ID || row.RoutingKey != "task.deploy" {
		t.Errorf("outbox row = %+v", row)
	}
	want := map[string]string{
		"id":         task.ID.String(),
		"type":       "deploy",
		"tenant_id":  tenant.ID.String(),
		"status":     "accepted",
		"created_at": domain.FormatTime(task.CreatedAt),
	}
	if len(row.Payload) != len(want) {
		t.Errorf("payload has fields %v, want exactly %v", row.Payload, want)
	}
	for k, v := range want {
		if row.Payload[k] != v {
			t.Errorf("payload[%q] = %q, want %q", k, row.Payload[k], v)
		}
	}
}

func TestCreateTenantDuplicateSlug(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newRepo(t)

	first := mustCreate(t, s, "acme")

	_, _, err := s.CreateTenant(ctx, "acme", "Another")
	assertCode(t, err, domain.CodeTenantAlreadyExists)
	if n := len(outbox(t, pool)); n != 1 {
		t.Errorf("outbox has %d rows after a rejected create, want 1", n)
	}

	// Slugs stay taken after the tenant is destroyed.
	testutil.SetTenantStatus(t, pool, first.ID, domain.TenantDestroyed)
	_, _, err = s.CreateTenant(ctx, "acme", "Another")
	assertCode(t, err, domain.CodeTenantAlreadyExists)
}

func TestPatchTenant(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newRepo(t)

	created := mustCreate(t, s, "acme")
	testutil.FinishTask(t, pool, created.ID, domain.TenantActive) // version 2

	tenant, task, err := s.PatchTenant(ctx, created.ID, "Acme Renamed", 2)
	if err != nil {
		t.Fatalf("PatchTenant: %v", err)
	}
	if tenant.Name != "Acme Renamed" || tenant.Status != domain.TenantUpdating || tenant.Version != 3 {
		t.Errorf("tenant = %+v, want renamed, updating, version 3", tenant)
	}
	if tenant.UpdatedAt.Before(created.UpdatedAt) {
		t.Errorf("updated_at went backwards: %v < %v", tenant.UpdatedAt, created.UpdatedAt)
	}
	if task.Type != domain.TaskUpdate || task.Status != domain.TaskAccepted || task.TenantID != tenant.ID {
		t.Errorf("task = %+v", task)
	}

	rows := outbox(t, pool)
	if len(rows) != 2 || rows[1].TaskID != task.ID || rows[1].RoutingKey != "task.update" {
		t.Fatalf("outbox = %+v, want a second row for the update task", rows)
	}
}

func TestPatchTenantRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newRepo(t)

	active := mustCreate(t, s, "active")
	testutil.FinishTask(t, pool, active.ID, domain.TenantActive) // version 2
	provisioning := mustCreate(t, s, "provisioning")             // version 1

	tests := []struct {
		name     string
		id       uuid.UUID
		version  int
		wantCode domain.Code
	}{
		{"unknown tenant", uuid.New(), 1, domain.CodeTenantNotFound},
		{"stale version", active.ID, 1, domain.CodeTenantVersionConflict},
		{"future version", active.ID, 3, domain.CodeTenantVersionConflict},
		{"current version, not active", provisioning.ID, 1, domain.CodeTenantUpdateNotAllowed},
		{"stale version, not active", provisioning.ID, 5, domain.CodeTenantVersionConflict},
		// Out of int32 range: no row can match, so no UPDATE is sent at all.
		{"version zero", active.ID, 0, domain.CodeTenantVersionConflict},
		{"version above int32", active.ID, math.MaxInt32 + 1, domain.CodeTenantVersionConflict},
		{"version out of range, unknown tenant", uuid.New(), -1, domain.CodeTenantNotFound},
	}

	before := len(outbox(t, pool))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := s.PatchTenant(ctx, tt.id, "Renamed", tt.version)
			assertCode(t, err, tt.wantCode)
		})
	}

	if after := len(outbox(t, pool)); after != before {
		t.Errorf("outbox grew from %d to %d rows on rejected PATCHes", before, after)
	}
	for _, id := range []uuid.UUID{active.ID, provisioning.ID} {
		got := mustGet(t, s, id)
		if got.Name == "Renamed" {
			t.Errorf("tenant %s renamed by a rejected PATCH", got.Slug)
		}
	}
	if got := mustGet(t, s, active.ID); got.Version != 2 || got.Status != domain.TenantActive {
		t.Errorf("active tenant changed by rejected PATCHes: %+v", got)
	}
}

func TestDeleteTenant(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newRepo(t)

	for _, from := range []domain.TenantStatus{domain.TenantActive, domain.TenantFailed} {
		t.Run("from "+string(from), func(t *testing.T) {
			created := mustCreate(t, s, "from-"+string(from))
			testutil.FinishTask(t, pool, created.ID, from) // version 2

			tenant, task, err := s.DeleteTenant(ctx, created.ID)
			if err != nil {
				t.Fatalf("DeleteTenant: %v", err)
			}
			if tenant.Status != domain.TenantDestroying || tenant.Version != 3 {
				t.Errorf("tenant = %+v, want destroying, version 3", tenant)
			}
			if task.Type != domain.TaskDestroy || task.Status != domain.TaskAccepted {
				t.Errorf("task = %+v", task)
			}
			if last := outbox(t, pool); last[len(last)-1].TaskID != task.ID || last[len(last)-1].RoutingKey != "task.destroy" {
				t.Errorf("last outbox row = %+v, want the destroy task", last[len(last)-1])
			}
		})
	}
}

func TestDeleteTenantRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newRepo(t)

	provisioning := mustCreate(t, s, "provisioning")

	updating := mustCreate(t, s, "updating")
	testutil.FinishTask(t, pool, updating.ID, domain.TenantActive)
	if _, _, err := s.PatchTenant(ctx, updating.ID, "Renamed", 2); err != nil {
		t.Fatalf("PatchTenant: %v", err)
	}

	destroying := mustCreate(t, s, "destroying")
	testutil.FinishTask(t, pool, destroying.ID, domain.TenantActive)
	if _, _, err := s.DeleteTenant(ctx, destroying.ID); err != nil {
		t.Fatalf("DeleteTenant: %v", err)
	}

	destroyed := mustCreate(t, s, "destroyed")
	testutil.SetTenantStatus(t, pool, destroyed.ID, domain.TenantDestroyed)

	tests := []struct {
		name     string
		id       uuid.UUID
		wantCode domain.Code
	}{
		{"unknown tenant", uuid.New(), domain.CodeTenantNotFound},
		{"provisioning", provisioning.ID, domain.CodeTenantUpdateNotAllowed},
		{"updating", updating.ID, domain.CodeTenantUpdateNotAllowed},
		{"destroying", destroying.ID, domain.CodeTenantUpdateNotAllowed},
		{"destroyed", destroyed.ID, domain.CodeTenantUpdateNotAllowed},
	}

	before := len(outbox(t, pool))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := s.DeleteTenant(ctx, tt.id)
			assertCode(t, err, tt.wantCode)
		})
	}
	if after := len(outbox(t, pool)); after != before {
		t.Errorf("outbox grew from %d to %d rows on rejected DELETEs", before, after)
	}
}

// Every write to the tenant row bumps the version, including the worker's
// terminal outcomes (simulated here by finishTask).
func TestVersionBumpsOnEveryWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newRepo(t)

	tenant := mustCreate(t, s, "acme")
	assertVersion(t, s, tenant.ID, 1)

	testutil.FinishTask(t, pool, tenant.ID, domain.TenantActive)
	assertVersion(t, s, tenant.ID, 2)

	if _, _, err := s.PatchTenant(ctx, tenant.ID, "Renamed", 2); err != nil {
		t.Fatalf("PatchTenant: %v", err)
	}
	assertVersion(t, s, tenant.ID, 3)

	testutil.FinishTask(t, pool, tenant.ID, domain.TenantActive)
	assertVersion(t, s, tenant.ID, 4)

	if _, _, err := s.DeleteTenant(ctx, tenant.ID); err != nil {
		t.Fatalf("DeleteTenant: %v", err)
	}
	assertVersion(t, s, tenant.ID, 5)
}

func TestGetNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newRepo(t)

	_, err := s.GetTenant(ctx, uuid.New())
	assertCode(t, err, domain.CodeTenantNotFound)
	_, err = s.GetTask(ctx, uuid.New())
	assertCode(t, err, domain.CodeTaskNotFound)
}

func TestGetTask(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newRepo(t)

	_, created, err := s.CreateTenant(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	got, err := s.GetTask(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.ID != created.ID || got.Type != domain.TaskDeploy || !got.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("GetTask = %+v, want %+v", got, created)
	}
}

func TestListTenantsPagination(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newRepo(t)

	var ids []uuid.UUID // oldest first
	for _, slug := range []string{"t-one", "t-two", "t-three", "t-four", "t-five"} {
		ids = append(ids, mustCreate(t, s, slug).ID)
	}

	t.Run("pages newest first until the end", func(t *testing.T) {
		var got []uuid.UUID
		var cursor *uuid.UUID
		pages := 0
		for {
			page, next, err := s.ListTenants(ctx, store.Page{Cursor: cursor, Limit: 2})
			if err != nil {
				t.Fatalf("ListTenants: %v", err)
			}
			pages++
			for _, tenant := range page {
				got = append(got, tenant.ID)
			}
			if next == nil {
				break
			}
			if *next != page[len(page)-1].ID {
				t.Fatalf("next cursor %s is not the page's last id", next)
			}
			cursor = next
		}

		if pages != 3 {
			t.Errorf("got %d pages of 2 for 5 rows, want 3", pages)
		}
		want := []uuid.UUID{ids[4], ids[3], ids[2], ids[1], ids[0]}
		if len(got) != len(want) {
			t.Fatalf("got %d tenants, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("order at %d: got %s, want %s (newest first)", i, got[i], want[i])
			}
		}
	})

	t.Run("limit equal to row count has no next page", func(t *testing.T) {
		page, next, err := s.ListTenants(ctx, store.Page{Limit: 5})
		if err != nil {
			t.Fatalf("ListTenants: %v", err)
		}
		if len(page) != 5 || next != nil {
			t.Errorf("got %d rows, next=%v; want 5 rows and no next page", len(page), next)
		}
	})

	t.Run("limit above row count", func(t *testing.T) {
		page, next, err := s.ListTenants(ctx, store.Page{Limit: 50})
		if err != nil || len(page) != 5 || next != nil {
			t.Errorf("got %d rows, next=%v, err=%v", len(page), next, err)
		}
	})

	t.Run("cursor past the oldest row gives an empty page", func(t *testing.T) {
		page, next, err := s.ListTenants(ctx, store.Page{Cursor: &ids[0], Limit: 2})
		if err != nil || len(page) != 0 || next != nil {
			t.Errorf("got %d rows, next=%v, err=%v", len(page), next, err)
		}
	})

	t.Run("destroyed tenants are listed", func(t *testing.T) {
		page, _, err := s.ListTenants(ctx, store.Page{Limit: 50})
		if err != nil {
			t.Fatalf("ListTenants: %v", err)
		}
		if len(page) != len(ids) {
			t.Errorf("got %d tenants, want all %d", len(page), len(ids))
		}
	})

	t.Run("limit out of range", func(t *testing.T) {
		for _, limit := range []int{0, -1, 1001} {
			if _, _, err := s.ListTenants(ctx, store.Page{Limit: limit}); err == nil {
				t.Errorf("limit %d accepted", limit)
			}
		}
	})
}

func TestListTasks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newRepo(t)

	a := mustCreate(t, s, "tenant-a")
	testutil.FinishTask(t, pool, a.ID, domain.TenantActive)
	_, update, err := s.PatchTenant(ctx, a.ID, "Renamed", 2)
	if err != nil {
		t.Fatalf("PatchTenant: %v", err)
	}
	b := mustCreate(t, s, "tenant-b")

	t.Run("filtered by tenant, newest first", func(t *testing.T) {
		tasks, next, err := s.ListTasks(ctx, &a.ID, store.Page{Limit: 50})
		if err != nil {
			t.Fatalf("ListTasks: %v", err)
		}
		if len(tasks) != 2 || next != nil {
			t.Fatalf("got %d tasks (next=%v), want 2", len(tasks), next)
		}
		if tasks[0].ID != update.ID || tasks[0].Type != domain.TaskUpdate || tasks[1].Type != domain.TaskDeploy {
			t.Errorf("tasks = %+v, want update then deploy", tasks)
		}
		if tasks[1].Status != domain.TaskDone {
			t.Errorf("deploy task status = %q, want done", tasks[1].Status)
		}
		for _, task := range tasks {
			if task.TenantID != a.ID {
				t.Errorf("task %s belongs to %s, not tenant a", task.ID, task.TenantID)
			}
		}
	})

	t.Run("unfiltered", func(t *testing.T) {
		tasks, _, err := s.ListTasks(ctx, nil, store.Page{Limit: 50})
		if err != nil || len(tasks) != 3 {
			t.Fatalf("got %d tasks, err=%v; want 3", len(tasks), err)
		}
		if tasks[0].TenantID != b.ID {
			t.Errorf("newest task belongs to %s, want tenant b", tasks[0].TenantID)
		}
	})

	t.Run("paginated with filter", func(t *testing.T) {
		first, next, err := s.ListTasks(ctx, &a.ID, store.Page{Limit: 1})
		if err != nil || len(first) != 1 || next == nil {
			t.Fatalf("first page: %d tasks, next=%v, err=%v", len(first), next, err)
		}
		second, next, err := s.ListTasks(ctx, &a.ID, store.Page{Cursor: next, Limit: 1})
		if err != nil || len(second) != 1 || next != nil {
			t.Fatalf("second page: %d tasks, next=%v, err=%v", len(second), next, err)
		}
		if first[0].ID == second[0].ID {
			t.Error("the same task on both pages")
		}
	})

	t.Run("unknown tenant gives an empty page", func(t *testing.T) {
		unknown := uuid.New()
		tasks, next, err := s.ListTasks(ctx, &unknown, store.Page{Limit: 50})
		if err != nil || len(tasks) != 0 || next != nil {
			t.Errorf("got %d tasks, next=%v, err=%v; want an empty page", len(tasks), next, err)
		}
	})
}

// ---- helpers ----

func newRepo(t *testing.T) (*store.Store, *pgxpool.Pool) {
	t.Helper()
	pool := openStore(t)
	return store.New(pool), pool
}

func mustCreate(t *testing.T, s *store.Store, slug string) domain.Tenant {
	t.Helper()
	tenant, _, err := s.CreateTenant(context.Background(), slug, "Tenant "+slug)
	if err != nil {
		t.Fatalf("CreateTenant(%q): %v", slug, err)
	}
	return tenant
}

func mustGet(t *testing.T, s *store.Store, id uuid.UUID) domain.Tenant {
	t.Helper()
	tenant, err := s.GetTenant(context.Background(), id)
	if err != nil {
		t.Fatalf("GetTenant: %v", err)
	}
	return tenant
}

func assertVersion(t *testing.T, s *store.Store, id uuid.UUID, want int) {
	t.Helper()
	if got := mustGet(t, s, id).Version; got != want {
		t.Fatalf("version = %d, want %d", got, want)
	}
}

type outboxRow struct {
	TaskID     uuid.UUID
	RoutingKey string
	Payload    map[string]string
}

// outbox returns every outbox row, oldest first.
func outbox(t *testing.T, pool *pgxpool.Pool) []outboxRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT task_id, routing_key, payload FROM outbox ORDER BY id`)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer rows.Close()

	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		var payload []byte
		if err := rows.Scan(&r.TaskID, &r.RoutingKey, &payload); err != nil {
			t.Fatalf("scan outbox: %v", err)
		}
		if err := json.Unmarshal(payload, &r.Payload); err != nil {
			t.Fatalf("decode outbox payload %s: %v", payload, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	return out
}

func assertCode(t *testing.T, err error, want domain.Code) {
	t.Helper()
	if got := domain.CodeOf(err); got != want {
		t.Fatalf("error = %v (code %q), want code %q", err, got, want)
	}
}
