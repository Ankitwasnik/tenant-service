# How the tenant service is implemented

This document walks through how the service works: the moving parts, the path a request takes, the mechanisms that make it correct under concurrency and failure, and what each file does.

## The pieces

Two Go binaries, built into one Docker image, plus PostgreSQL and RabbitMQ:

- **`controlplane`**: one process running three goroutines that share one Postgres connection pool:
  - the **HTTP API**, which clients call to create, read, update and delete tenants, and to read tasks;
  - the **outbox relay**, which publishes committed tasks to RabbitMQ;
  - the **update consumer**, which applies the worker's progress reports.
- **`worker`**: the provisioning simulator. It takes tasks from RabbitMQ, waits a random delay, and reports `in_progress`, then `done` or `failed`.

Postgres is the source of truth. RabbitMQ only carries messages between the control plane and the worker.

## One tenant, end to end

1. **`POST /v1/tenants`**. In one transaction, the API inserts the tenant (`provisioning`, version 1), a `deploy` task (`accepted`), and an **outbox** row holding the task as JSON. It returns `201` with both. The API never talks to RabbitMQ.
2. **The relay** polls the outbox every 200 ms. It claims unpublished rows, publishes them to the `tasks` exchange, waits for RabbitMQ to confirm each one, and marks only the confirmed rows as published, in the same transaction as the claim.
3. **The worker** receives the task from `worker.tasks`. It waits, publishes `in_progress` to the `task-updates` exchange, waits again, publishes `done` (or `failed`), and only then acks the task.
4. **The consumer** receives each update from `controlplane.task-updates`. In one transaction it records the update's id in the **inbox**, moves the task forward, and, when the task is finished, moves the tenant: `deploy done` → `active`, and so on. The version goes up by one.
5. `GET /v1/tenants/{id}` now shows `active`, version 2, and `GET /v1/tasks?tenant_id=…` shows the deploy task `done`.

PATCH (→ `updating` → `active`) and DELETE (→ `destroying` → `destroyed`) follow the same path with an `update` or `destroy` task.

## State machines

**Tenant.** Created in `provisioning`. PATCH is allowed only from `active` (→ `updating`). DELETE is allowed from `active` or `failed` (→ `destroying`). A finished task moves the tenant:

| Task | `done` → | `failed` → | Tenant must be in |
| ---- | -------- | ---------- | ----------------- |
| deploy | `active` | `failed` | `provisioning` |
| update | `active` | `failed` | `updating` |
| destroy | `destroyed` | `failed` | `destroying` |

Because PATCH and DELETE need a tenant with no task in flight, a tenant never has more than one open task.

**Task.** It only ever moves forward: `accepted` < `in_progress` < `done` | `failed`. An update that doesn't move it forward is ignored as stale. That covers a late `in_progress` after `done`, anything sent to a finished task, and a repeat. A `done` that arrives before its `in_progress` is applied, because it's still a forward move.

These rules are plain functions in `internal/domain`, with no I/O. The SQL guards enforce the same rules atomically.

## Concurrency

**No application locks, no read-then-write.** Every API mutation is one conditional `UPDATE`:

```sql
UPDATE tenants
   SET name = $2, status = 'updating', version = version + 1, updated_at = now()
 WHERE id = $1 AND status = 'active' AND version = $3
RETURNING *;
```

When two PATCHes race with the same version, Postgres serializes them on the row lock. The loser re-evaluates the `WHERE` against the committed row, where the version has moved on, and matches nothing. DELETE is the same with `status IN ('active', 'failed')` and no version.

**Picking the error.** When the `UPDATE` matches nothing, the tenant is read once more and the error is chosen from it:
- no row → `tenant_not_found`
- a different version → `tenant_version_conflict`
- otherwise → `tenant_update_not_allowed`

The **version is checked before the status**, so a request that lost a race is always told to re-read and retry (`tenant_version_conflict`). If the read shows the operation is allowed after all (the tenant changed in between), the `UPDATE` is retried, at most 3 times.

**Optimistic locking with the client's version.** PATCH carries the version the client last saw: `{"name": "...", "version": 2}`. That also protects against lost updates between two clients' separate requests, not just transactions that overlap on the server. The worker's updates bump the version too, so a stale version also means "the tenant changed since you looked".

**Unique constraints.** The slug has a unique index, and tenants are never deleted (a destroyed tenant stays as a row), so the slug stays unique forever. Of 50 concurrent creates with one slug, exactly one succeeds; the rest hit the index and get `tenant_already_exists`. A partial unique index, `tasks (tenant_id) WHERE status IN ('accepted', 'in_progress')`, is a safety net under the status rules: two open tasks for one tenant can't be written even if a bug slipped past them.

**Concurrent updates for one task.** The consumer handles updates on 4 goroutines, so a task's `in_progress` and `done` can be processed at the same moment. The consumer locks the task row (`SELECT … FOR NO KEY UPDATE`), so they take turns, and the second sees the first's result. Without that lock, a late `in_progress` could overwrite a committed `done`.

## Messaging

**Publish only after commit: the transactional outbox.** The event is a row written in the same transaction as the change. A rolled-back change has no row, so nothing is published for it. The relay can only see committed rows, so nothing is published before the commit. The relay claims rows with `FOR UPDATE SKIP LOCKED`, so several control-plane instances could relay without claiming the same row. The relay's cost: up to 200 ms of latency, and **at-least-once** delivery. If the relay crashes after publishing but before marking the row, the event goes out again.

**Duplicates made harmless.** Every update carries an `update_id`, the idempotency key. The consumer inserts it into the `inbox` table with `ON CONFLICT DO NOTHING`, in the same transaction as the change. If the id is already there, the update is a duplicate and nothing else happens. The worker derives `update_id` deterministically, as UUIDv5 of the task id and status. So a task that reaches the worker twice (the relay is at-least-once) produces the same ids the second time, and the inbox drops the repeats. If a re-run reports the *other* outcome, it has a different id, but the forward-only rule rejects it as stale.

**Confirmed publishing.** The relay and the worker publish through one shared publisher (`messaging.ConfirmPublisher`):
- Every message is persistent.
- The whole batch is sent, then all its confirms are awaited, with a 5 s timeout.
- `mandatory` is set, so an unroutable message is returned by the broker instead of silently dropped. It counts as not published.

**Topology** (declared by both processes on every connect, from one definition):

| Exchange | Routing key | Queue | Dead-letter queue |
| -------- | ----------- | ----- | ----------------- |
| `tasks` (topic) | `task.<type>` | `worker.tasks` | `worker.tasks.dlq` |
| `task-updates` (topic) | `task.update.<status>` | `controlplane.task-updates` | `controlplane.task-updates.dlq` |

The queues are durable **quorum** queues. They dead-letter through one `dlx` exchange, and have `x-delivery-limit: 10`.

## Failure handling

**What the consumer does with each message:**

| Outcome | Action |
| ------- | ------ |
| applied, duplicate or stale | ack |
| malformed (bad JSON, missing ids, status `accepted`), unknown task, broken invariant, any other permanent error, a panic | dead-letter; carry on with the next message |
| transient database error | retry in-process with backoff, holding the message |
| shutdown during a retry | requeue |

**Transient or permanent** is decided by the Postgres error code, in one function (`store.IsTransient`):
- **transient:** lost connections (class `08`), deadlocks and serialization failures (`40`), exhausted resources (`53`), the server shutting down or starting (`57P01`–`57P03`), network errors, and cancelled contexts
- **permanent:** everything else, so a bug fails loudly instead of being retried forever

**The delivery limit** stops a message that keeps crashing its consumer. In RabbitMQ 4.3, a delivery whose channel closes with the message unacked counts toward the limit, and after the first delivery plus 10 redeliveries the message is dead-lettered. A `nack` with requeue does *not* count. That's why transient errors are retried in-process rather than by requeueing: a requeue loop would never be stopped by the limit. It's also why a shutdown requeue costs a message nothing.

**The broker going away.** `messaging.Run` keeps a connection alive. It dials, declares the topology, and runs the work on that connection. When the connection closes, it cancels that work, backs off (500 ms doubling to 15 s, with jitter), and repeats. Unacked messages are redelivered by RabbitMQ, and unpublished events wait in the outbox. The HTTP API doesn't depend on the broker, so it keeps accepting writes during an outage, and their events go out after the reconnect.

**The worker's side.**
- A malformed task goes to its dead-letter queue.
- A task interrupted by shutdown is requeued.
- If an update fails to publish, the worker stops, leaving the task unacked, so RabbitMQ redelivers it and counts the redelivery.

**Shutdown.** SIGTERM cancels one context shared by everything. The HTTP server drains its in-flight requests, and the relay, consumer and worker stop at their next step.

## The HTTP API

| Method and path | Success | Errors |
| --------------- | ------- | ------ |
| `POST /v1/tenants` | `201` + `Location`, `{tenant, task}` | `400`, `409 tenant_already_exists` |
| `GET /v1/tenants` | `200` page | `400` |
| `GET /v1/tenants/{id}` | `200` | `404 tenant_not_found` |
| `PATCH /v1/tenants/{id}` | `202` `{tenant, task}` | `400`, `404`, `409 tenant_version_conflict`, `409 tenant_update_not_allowed` |
| `DELETE /v1/tenants/{id}` | `202` `{tenant, task}` | `404`, `409 tenant_update_not_allowed` |
| `GET /v1/tasks[?tenant_id=]` | `200` page | `400` |
| `GET /v1/tasks/{id}` | `200` | `404 task_not_found` |
| `GET /healthz` | `200` | `503 service_unavailable` |

- **Status codes:** POST returns `201` because the tenant exists straight away. PATCH and DELETE return `202` because the work finishes asynchronously; they return the task so the client can follow it.
- **Errors:** every error has one shape, `{"error": {"code", "message", "details"}}`, written by a single function. A non-domain error becomes `500 internal_error`, and is logged but never returned.
- **Validation:** unknown fields, wrong types and oversized bodies are `400 validation_error`, naming each bad field. The slug must match `^[a-z][-a-z0-9]{1,26}[a-z0-9]$`. The name is trimmed, non-empty and at most 200 characters.
- **Lists:** keyset pagination on the id, newest first: `?limit=` (default 50, max 200) and `?cursor=`, returning `next_cursor`. A malformed path id gets the resource's `404`.
- **Ids and timestamps** come from Postgres: `uuidv7()`, and `timestamptz(3)` rounded to milliseconds. They're returned as `2026-09-26T10:00:00.000Z`.

## Data model

| Table | Holds |
| ----- | ----- |
| `tenants` | id, slug (unique), name, status, version, timestamps |
| `tasks` | id (also the event id), tenant_id, type, status, error, timestamps; partial unique index for one open task per tenant |
| `outbox` | committed events waiting to be published: task_id, routing key, payload, `published_at` |
| `inbox` | the `update_id` of every worker update processed, which is how duplicates are recognised |

## What each file does

### Entry points

| File | What it does |
| ---- | ------------ |
| `cmd/controlplane/main.go` | Opens the database and runs its migrations. Starts the HTTP server, and a broker session (relay + consumer) under the reconnect helper, all in one errgroup. Graceful shutdown on SIGTERM. |
| `cmd/controlplane/config.go` | Reads `DATABASE_URL`, `AMQP_URL` and `HTTP_ADDR` from the environment. |
| `cmd/worker/main.go` | Runs the worker under the reconnect helper until SIGTERM. |
| `cmd/worker/config.go` | Parses `--min-delay-ms`, `--max-delay-ms` and `--fail-rate`, each with a `WORKER_*` environment variable, and validates them. |

### `internal/domain`: the model and its rules (no I/O)

| File | What it does |
| ---- | ------------ |
| `status.go` | Tenant statuses, task statuses and task types, with validity checks and the task's forward-only rank. |
| `tenant.go` | The `Tenant` type; which statuses allow PATCH and DELETE; `PatchConflict` / `DeleteConflict`, which choose the error after a guard misses (version before status). |
| `task.go` | The `Task` type; `ShouldApply` (the forward-only rule); `TerminalOutcome` (what a finished task does to its tenant). |
| `errors.go` | The error type carrying the contract codes, their constructors, and `FieldErrors` for validation errors. |
| `validate.go` | Slug and name rules, and `ValidateCreate`, which reports every bad field at once. |
| `time.go` | Formats timestamps as UTC ISO 8601 with milliseconds. |

### `internal/store`: PostgreSQL

| File | What it does |
| ---- | ------------ |
| `store.go` | Opens the connection pool and applies the embedded migrations (goose). |
| `repository.go` | The `Store` type, the transaction helper, mapping database rows to domain types, the task + outbox insert shared by every mutation, and pagination helpers. |
| `tenants.go` | Create, PATCH and DELETE (the guarded update, the error diagnosis, the bounded retry), plus get and list. |
| `tasks.go` | Get and list tasks, optionally for one tenant. |
| `outbox.go` | `RelayOutbox`: claim unpublished rows, publish, mark the confirmed ones, all in one transaction. |
| `updates.go` | `ApplyUpdate`: inbox insert, task row lock, forward-only check, task update, guarded tenant update. |
| `errors.go` | `IsTransient`: whether a database error is worth retrying, by Postgres error code. |
| `migrations/00001_init.sql` | The schema: the four tables and their indexes. |
| `queries/*.sql` | The hand-written SQL for each table. |
| `sqlcgen/*.go` | Go code generated from those queries by sqlc (`make generate`), committed; not edited by hand. |

### `internal/api`: HTTP

| File | What it does |
| ---- | ------------ |
| `router.go` | Builds the gin router: its defaults replaced (JSON 404/405), middleware, `/healthz`, and the `/v1` routes; defines the `Store` interface the handlers need. |
| `tenants.go` | The tenant handlers: create, list, get, PATCH, DELETE. |
| `tasks.go` | The task handlers: list and get. |
| `errors.go` | `respondError`: the one place that maps every error to its status code and JSON body. |
| `middleware.go` | A request id and one log line per request; panic recovery that returns `500` without leaking details. |
| `decode.go` | Reads a JSON body strictly (unknown fields, wrong types, size limit), with errors naming the field. |
| `params.go` | Parses `limit`, `cursor`, `tenant_id` and the path id. |
| `views.go` | The JSON shapes of tenants, tasks, `{tenant, task}` and pages. |

### `internal/messaging`: RabbitMQ, shared by both binaries

| File | What it does |
| ---- | ------------ |
| `topology.go` | Exchange and queue names, and `DeclareTopology` (quorum queues, dead-letter queues, delivery limit). |
| `event.go` | The task message (the task as committed), its routing key, and decoding with validation for the worker. |
| `update.go` | The update message, `UpdateID` (deterministic UUIDv5), and decoding with validation for the consumer. |
| `publisher.go` | `ConfirmPublisher`: batch publish with confirms and `mandatory`, matching returns to messages. |
| `reconnect.go` | `Run`: keeps a broker connection alive, re-declaring the topology and restarting the work on every reconnect. |

### Background loops

| File | What it does |
| ---- | ------------ |
| `internal/outbox/relay.go` | The relay loop: poll, publish, back off on failure. |
| `internal/outbox/amqp.go` | Adapts outbox rows to `ConfirmPublisher` messages (the task id is the message id). |
| `internal/consumer/consumer.go` | The update consumer: 4 goroutines, and how each message is settled (ack, dead-letter, retry, requeue). |
| `internal/worker/worker.go` | The simulator: consume with 4 goroutines, delay, publish `in_progress` and then the outcome, ack last. |
| `internal/backoff/backoff.go` | Jittered exponential backoff and a sleep that stops on shutdown, used by the relay, consumer and reconnect helper. |

### Running it

| File | What it does |
| ---- | ------------ |
| `compose.yaml` | Postgres, RabbitMQ, the control plane and the worker; plus tool containers for tests, lint and sqlc. |
| `.env.template` | Every setting (credentials, host ports, worker flags) with its local-dev value; `make` copies it to `.env` on first run. |
| `Makefile` | The commands: `up`, `down`, `clean`, `logs`, `worker`, `test`, `check`, `race`, `race-demo`, `fmt`, `generate`. |
| `Dockerfile` | Builds both binaries into one small Alpine image, running as a non-root user. |
| `sqlc.yaml` | Tells sqlc where the schema and queries are and how to map types. |
| `.golangci.yml` | Linter and formatter configuration (including the gosec security checks). |
| `scripts/publish-update.sh` | Publishes a hand-made update to the broker: duplicates, out-of-order updates, poison messages. |
| `scripts/race-demo.sh` | Fires concurrent creates, PATCHes and DELETEs at the running stack and checks each batch had exactly one winner. |
| `scripts/lib/env.sh` | Reads settings from `.env` for both scripts. |

## Showing it works

```bash
make up                                   # boot everything
make worker ARGS="--fail-rate=1"          # every task fails; `make worker` restores the defaults
scripts/publish-update.sh --task-id latest --status in_progress   # a stale update: ignored
make race-demo                            # 50 concurrent requests per batch: exactly one winner each
make race                                 # the concurrency tests, with each race's outcome counts
make check                                # format, lint, security and vulnerability scans, then the tests
```
