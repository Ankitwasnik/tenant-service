-- name: InsertTenant :one
INSERT INTO tenants (slug, name, status)
VALUES ($1, $2, 'provisioning')
RETURNING *;

-- name: GetTenant :one
SELECT * FROM tenants
WHERE id = $1;

-- Keyset pagination, newest first (DESIGN.md §7). A NULL cursor starts at the top.
-- name: ListTenants :many
SELECT * FROM tenants
WHERE sqlc.narg(cursor)::uuid IS NULL OR id < sqlc.narg(cursor)::uuid
ORDER BY id DESC
LIMIT sqlc.arg(page_limit);

-- The PATCH guard (DESIGN.md §5): one conditional UPDATE, no read-then-write.
-- No row returned (pgx.ErrNoRows) means the guard didn't match.
-- name: UpdateTenantIfActive :one
UPDATE tenants
   SET name = $2, status = 'updating', version = version + 1, updated_at = now()
 WHERE id = $1 AND status = 'active' AND version = sqlc.arg(expected_version)
RETURNING *;

-- The DELETE guard: status only, no version (DESIGN.md §5).
-- name: MarkTenantDestroying :one
UPDATE tenants
   SET status = 'destroying', version = version + 1, updated_at = now()
 WHERE id = $1 AND status IN ('active', 'failed')
RETURNING *;
