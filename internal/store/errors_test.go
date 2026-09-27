package store_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Ankitwasnik/tenant-service/internal/domain"
	"github.com/Ankitwasnik/tenant-service/internal/store"
	"github.com/Ankitwasnik/tenant-service/internal/testutil"
)

func TestIsTransientSQLState(t *testing.T) {
	tests := []struct {
		code      string
		name      string
		transient bool
	}{
		// Class 08: connection exception.
		{"08000", "connection_exception", true},
		{"08001", "sqlclient_unable_to_establish_sqlconnection", true},
		{"08006", "connection_failure", true},
		// Class 40: transaction rollback.
		{"40001", "serialization_failure", true},
		{"40P01", "deadlock_detected", true},
		{"40003", "statement_completion_unknown", true},
		{"40002", "transaction_integrity_constraint_violation", false}, // the class-40 exception
		// Class 53: insufficient resources.
		{"53000", "insufficient_resources", true},
		{"53300", "too_many_connections", true},
		// 57P0x: the server is going away or not up yet.
		{"57P01", "admin_shutdown", true},
		{"57P02", "crash_shutdown", true},
		{"57P03", "cannot_connect_now", true},
		// Everything else is permanent.
		{"57014", "query_canceled", false},
		{"23505", "unique_violation", false},
		{"23514", "check_violation", false},
		{"23503", "foreign_key_violation", false},
		{"22P02", "invalid_text_representation", false},
		{"42P01", "undefined_table", false},
		{"42703", "undefined_column", false},
		{"28P01", "invalid_password", false},
		{"", "empty code", false},
		{"0", "malformed code", false},
	}

	for _, tt := range tests {
		t.Run(tt.code+" "+tt.name, func(t *testing.T) {
			// Wrapped, as the store returns it.
			err := fmt.Errorf("list tasks: %w", &pgconn.PgError{Code: tt.code})
			if got := store.IsTransient(err); got != tt.transient {
				t.Fatalf("IsTransient(%s) = %v, want %v", tt.code, got, tt.transient)
			}
		})
	}
}

func TestIsTransientOtherErrors(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		transient bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("boom"), false},
		{"no rows", pgx.ErrNoRows, false},
		{"domain error", domain.TenantNotFound(uuid.New()), false},
		{"context canceled", fmt.Errorf("query: %w", context.Canceled), true},
		{"deadline exceeded", fmt.Errorf("query: %w", context.DeadlineExceeded), true},
		{"connection reset", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, true},
		{"connection dropped mid-query", fmt.Errorf("read: %w", io.ErrUnexpectedEOF), true},
		{"EOF", io.EOF, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := store.IsTransient(tt.err); got != tt.transient {
				t.Fatalf("IsTransient(%v) = %v, want %v", tt.err, got, tt.transient)
			}
		})
	}
}

// What a query actually returns when the server drops the connection, as it
// does during a Postgres restart: terminate this test's own backend from a
// second connection, then use the first one.
func TestIsTransientTerminatedConnection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	url := testutil.NewDatabase(t)

	victim, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer victim.Close(ctx)
	killer, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer killer.Close(ctx)

	var terminated bool
	if err := killer.QueryRow(ctx, "SELECT pg_terminate_backend($1)", victim.PgConn().PID()).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("terminate backend: ok=%v err=%v", terminated, err)
	}

	_, err = victim.Exec(ctx, "SELECT 1")
	if err == nil {
		t.Fatal("query on a terminated connection succeeded")
	}
	if !store.IsTransient(err) {
		t.Fatalf("IsTransient(%T: %v) = false, want true", err, err)
	}
	t.Logf("terminated connection returns %T: %v", err, err)
}

// A real connection failure, as pgx reports it: a *pgconn.ConnectError whose
// cause is unexported, so it can't be built by hand.
func TestIsTransientConnectError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := pgconn.Connect(ctx, "postgres://nobody@127.0.0.1:1/none?connect_timeout=2")
	var connectErr *pgconn.ConnectError
	if !errors.As(err, &connectErr) {
		t.Fatalf("expected a *pgconn.ConnectError, got %T: %v", err, err)
	}
	if !store.IsTransient(err) {
		t.Fatalf("IsTransient(connect error) = false: %v", err)
	}
}
