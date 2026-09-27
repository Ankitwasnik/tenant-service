package store

import (
	"context"
	"errors"
	"io"
	"net"

	"github.com/jackc/pgx/v5/pgconn"
)

// IsTransient reports whether a database error is worth retrying: the same
// operation may succeed once the database is reachable or less loaded
// (DESIGN.md §6). The update consumer retries transient errors in-process and
// dead-letters everything else, so the default is permanent: a bug such as a
// CHECK violation fails loudly instead of being retried forever.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}

	// A server error is judged by its SQLSTATE alone. This comes first because
	// a failed connection can wrap one: "the database is starting up" (57P03)
	// is transient, a wrong password (28P01) is not.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return transientSQLState(pgErr.Code)
	}

	// Cancelled or timed out: the operation didn't fail on its own merits.
	// Treating this as transient means a shutdown requeues the message
	// instead of dead-lettering it.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	// The database couldn't be reached, or the connection broke mid-query.
	var connectErr *pgconn.ConnectError
	var netErr net.Error
	switch {
	case errors.As(err, &connectErr),
		errors.As(err, &netErr),
		errors.Is(err, io.EOF),
		errors.Is(err, io.ErrUnexpectedEOF),
		pgconn.Timeout(err),
		pgconn.SafeToRetry(err):
		return true
	}
	return false
}

// transientSQLState implements the SQLSTATE rule from DESIGN.md §6.
func transientSQLState(code string) bool {
	switch code {
	case "40002":
		// transaction_integrity_constraint_violation: a deferred constraint
		// failed at commit. It sits in class 40, but retrying gives the same result.
		return false
	case "57P01", "57P02", "57P03":
		// admin_shutdown, crash_shutdown, cannot_connect_now (starting up).
		return true
	}
	// A SQLSTATE's class is its first two characters.
	switch code[:min(2, len(code))] {
	case "08", // connection exception
		"40", // transaction rollback: serialization failure, deadlock
		"53": // insufficient resources: too many connections, out of memory, disk full
		return true
	}
	return false
}
