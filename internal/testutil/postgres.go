// Package testutil holds helpers shared by integration tests.
package testutil

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// NewDatabase creates an empty database for one test, drops it when the test
// ends, and returns its connection URL. Every test gets a clean schema, and
// tests can run in parallel without seeing each other's rows.
//
// It needs TEST_DATABASE_URL, which `make test` sets (compose.yaml, `tests`
// service). With -short, integration tests are skipped instead.
func NewDatabase(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: needs Postgres (run make test)")
	}
	adminURL := os.Getenv("TEST_DATABASE_URL")
	if adminURL == "" {
		// Fail rather than skip: a missing variable in `make test` must not
		// silently turn the integration suite green.
		t.Fatal("TEST_DATABASE_URL is not set: run the suite with `make test`, or `go test -short` for unit tests only")
	}

	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect to test server: %v", err)
	}

	name := "t_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
		_ = admin.Close(ctx)
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		// FORCE: a test that leaked a connection must not fail the cleanup.
		if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)"); err != nil {
			t.Errorf("drop test database: %v", err)
		}
		_ = admin.Close(ctx)
	})

	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}
