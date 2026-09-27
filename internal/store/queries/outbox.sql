-- Written in the same tx as the tenant and task (DESIGN.md §6).
-- name: InsertOutbox :exec
INSERT INTO outbox (task_id, routing_key, payload)
VALUES ($1, $2, $3);

-- The relay's claim: the oldest unpublished events, locked for this tx.
-- SKIP LOCKED lets several relays (control-plane replicas) run at once: each
-- takes rows no other relay holds, so no event is claimed twice concurrently.
-- name: ClaimUnpublished :many
SELECT id, task_id, routing_key, payload
FROM outbox
WHERE published_at IS NULL
ORDER BY id
LIMIT sqlc.arg(batch_size)
FOR UPDATE SKIP LOCKED;

-- clock_timestamp(), not now(): the tx started before the publish, and
-- now() would record that earlier time (DESIGN.md §3).
-- name: MarkPublished :exec
UPDATE outbox
SET published_at = clock_timestamp()
WHERE id = ANY(sqlc.arg(ids)::bigint[]);
