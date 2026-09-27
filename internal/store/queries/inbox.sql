-- Records a processed worker update. update_id is the idempotency key: 0 rows
-- affected means this update was already processed (a duplicate delivery).
-- A concurrent insert of the same id waits on the first one's unique-index
-- entry, then sees it once the first commits (DESIGN.md §5).
-- name: InsertInbox :execrows
INSERT INTO inbox (update_id, task_id)
VALUES ($1, $2)
ON CONFLICT (update_id) DO NOTHING;
