-- name: InsertTask :one
INSERT INTO tasks (tenant_id, type, status)
VALUES ($1, $2, 'accepted')
RETURNING *;

-- name: GetTask :one
SELECT * FROM tasks
WHERE id = $1;

-- Keyset pagination, newest first, optionally for one tenant (DESIGN.md §7).
-- name: ListTasks :many
SELECT * FROM tasks
WHERE (sqlc.narg(tenant_id)::uuid IS NULL OR tenant_id = sqlc.narg(tenant_id)::uuid)
  AND (sqlc.narg(cursor)::uuid IS NULL OR id < sqlc.narg(cursor)::uuid)
ORDER BY id DESC
LIMIT sqlc.arg(page_limit);

-- Serializes updates to one task (DESIGN.md §5). NO KEY UPDATE, not UPDATE:
-- the key never changes, so foreign-key checks against the row aren't blocked.
-- name: LockTask :one
SELECT * FROM tasks
WHERE id = $1
FOR NO KEY UPDATE;

-- name: UpdateTaskStatus :one
UPDATE tasks
   SET status = $2, error = $3, updated_at = now()
 WHERE id = $1
RETURNING *;
