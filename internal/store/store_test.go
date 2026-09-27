package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Ankitwasnik/tenant-service/internal/store"
	"github.com/Ankitwasnik/tenant-service/internal/store/sqlcgen"
	"github.com/Ankitwasnik/tenant-service/internal/testutil"
)

func TestOpenMigratesEmptyDatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := openStore(t)

	for _, table := range []string{"tenants", "tasks", "outbox", "inbox", "goose_db_version"} {
		var exists bool
		if err := pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists); err != nil {
			t.Fatalf("check table %s: %v", table, err)
		}
		if !exists {
			t.Errorf("table %s missing after Open", table)
		}
	}

	var version int64
	if err := pool.QueryRow(ctx, "SELECT max(version_id) FROM goose_db_version WHERE is_applied").Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != 1 {
		t.Errorf("schema version = %d, want 1", version)
	}
}

func TestMigrateTwiceIsNoop(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := openStore(t) // Open already applied everything once

	applied, err := store.Migrate(ctx, pool)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if applied != 0 {
		t.Fatalf("second Migrate applied %d migrations, want 0", applied)
	}

	var rows int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM goose_db_version WHERE version_id > 0").Scan(&rows); err != nil {
		t.Fatalf("count applied migrations: %v", err)
	}
	if rows != 1 {
		t.Fatalf("goose_db_version has %d applied rows, want 1", rows)
	}
}

func TestOpenUnreachableDatabase(t *testing.T) {
	t.Parallel()
	_, err := store.Open(context.Background(), "postgres://nobody:secret@127.0.0.1:1/none?connect_timeout=2")
	if err == nil {
		t.Fatal("Open succeeded against an unreachable server")
	}
}

// The schema-level rules the repository (PLAN.md 2.2) relies on.
func TestSchemaDefaultsAndConstraints(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := sqlcgen.New(openStore(t))

	tenant, err := q.InsertTenant(ctx, sqlcgen.InsertTenantParams{Slug: "acme", Name: "Acme"})
	if err != nil {
		t.Fatalf("insert tenant: %v", err)
	}

	t.Run("ids are UUIDv7", func(t *testing.T) {
		if tenant.ID.Version() != 7 {
			t.Errorf("tenant id version = %d, want 7", tenant.ID.Version())
		}
	})

	t.Run("new tenant defaults", func(t *testing.T) {
		if tenant.Status != "provisioning" || tenant.Version != 1 {
			t.Errorf("status=%q version=%d, want provisioning/1", tenant.Status, tenant.Version)
		}
	})

	t.Run("timestamps have millisecond precision", func(t *testing.T) {
		if ns := tenant.CreatedAt.Nanosecond(); ns%1_000_000 != 0 {
			t.Errorf("created_at has sub-millisecond digits: %d ns", ns)
		}
		if !tenant.CreatedAt.Equal(tenant.UpdatedAt) {
			t.Errorf("created_at %v != updated_at %v on insert", tenant.CreatedAt, tenant.UpdatedAt)
		}
	})

	t.Run("slug is unique", func(t *testing.T) {
		_, err := q.InsertTenant(ctx, sqlcgen.InsertTenantParams{Slug: "acme", Name: "Other"})
		assertUniqueViolation(t, err, "tenants_slug_key")
	})

	t.Run("at most one open task per tenant", func(t *testing.T) {
		if _, err := q.InsertTask(ctx, sqlcgen.InsertTaskParams{TenantID: tenant.ID, Type: "deploy"}); err != nil {
			t.Fatalf("insert first task: %v", err)
		}
		_, err := q.InsertTask(ctx, sqlcgen.InsertTaskParams{TenantID: tenant.ID, Type: "update"})
		assertUniqueViolation(t, err, "tasks_one_open_per_tenant")
	})

	t.Run("empty name rejected", func(t *testing.T) {
		_, err := q.InsertTenant(ctx, sqlcgen.InsertTenantParams{Slug: "blank", Name: ""})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("err = %v, want check violation (23514)", err)
		}
	})
}

func openStore(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := store.Open(context.Background(), testutil.NewDatabase(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func assertUniqueViolation(t *testing.T, err error, constraint string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName != constraint {
		t.Fatalf("err = %v, want unique violation on %s", err, constraint)
	}
}
