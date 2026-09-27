-- Written in the same tx as the tenant and task (DESIGN.md §6). The relay's
-- queries are added with the relay (PLAN.md 4.2).
-- name: InsertOutbox :exec
INSERT INTO outbox (task_id, routing_key, payload)
VALUES ($1, $2, $3);
