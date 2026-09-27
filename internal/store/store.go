// Package store is the Postgres persistence layer: the connection pool, the
// schema migrations, and (from PLAN.md 2.2) the repository that maps
// sqlc-generated rows to domain types.
package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// The migrations are embedded, so the binary carries its own schema and no
// separate migration step or container is needed (DESIGN.md §1).
//
//go:embed migrations/*.sql
var embedded embed.FS

// Open connects to Postgres, checks the connection, and applies any pending
// migrations. The caller owns the returned pool and must Close it.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	if _, err := Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// Migrate applies every pending migration and returns how many it applied.
// Applied migrations are skipped, so running it again is a no-op.
//
// goose's session locker is not enabled: there is one control-plane instance
// (DESIGN.md §12).
func Migrate(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	migrations, err := fs.Sub(embedded, "migrations")
	if err != nil {
		return 0, fmt.Errorf("load migrations: %w", err)
	}

	// goose works on database/sql; this wraps the pool without opening new
	// connections. Closing db does not close the pool.
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()

	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations)
	if err != nil {
		return 0, fmt.Errorf("create migration provider: %w", err)
	}
	results, err := provider.Up(ctx)
	if err != nil {
		return 0, fmt.Errorf("apply migrations: %w", err)
	}
	return len(results), nil
}
