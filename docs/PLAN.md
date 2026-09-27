# Implementation Plan

This is the incremental build plan for [`DESIGN.md`](DESIGN.md). Section references (§n) point there.

## Rules for every subtask

- **Buildable at every step.** When a subtask is finished, `make check` passes: format check, lint, `sqlc diff` (from 2.1), `govulncheck` and the full `make test`. From 3.1 on, `make up` also boots cleanly.
- **One subtask = one commit.** This gives the clean history the brief asks for. The suggested message is listed with each subtask.
- **Tests land with the code they cover,** never in a later "add tests" step.
- **Nothing is stubbed for later.** Each subtask only adds what it needs. Later subtasks extend it, they don't replace it.
- **Scope can be cut from the end.** If scope has to shrink, cut in the order given in *Cut line* at the end.

---

## Phase 0: Skeleton and quality gates

### 0.1 Build and test harness

- `cmd/controlplane/main.go`: parses env config (`DATABASE_URL`, `AMQP_URL`, `HTTP_ADDR`), logs a JSON "starting" line with `slog`, exits.
- `Dockerfile`: multi-stage. The build stage is `golang:1.27`; the runtime stage is `alpine` with the binaries in `/usr/local/bin`.
- `compose.yaml`: add the `tests` service (`golang:1.27`, `profiles: [test]`, repo mounted, `GOMODCACHE`/`GOCACHE` volumes, `depends_on` postgres and rabbitmq healthy). Its environment points at `controlplane_test` and vhost `test`.
- `Makefile`: `test` target. It brings postgres and rabbitmq up, recreates the `controlplane_test` database and `test` vhost (with permissions), runs `docker compose run --rm tests go test -race ./...`, then drops both (§10). The cleanup runs even when tests fail.
- One trivial test, so the pipeline is proven end to end.

**Done when:** `make test` passes from a cold clone.
**Commit:** `build: add Dockerfile, compose test service and make test`

### 0.2 Lint, format, vuln scan

- `.golangci.yml` (v2): linters including `gosec`, `errcheck`, `staticcheck`, `revive`; formatters `gofumpt` and `goimports`.
- `Makefile`: `fmt`, `lint`, `vuln`, `check` (fmt --diff → lint → govulncheck → test). Each tool runs in a pinned container image, so the host needs none of them.

**Done when:** `make check` passes, and it fails on a deliberately misformatted file.
**Commit:** `build: add golangci-lint, gofumpt, govulncheck and make check`

---

## Phase 1: Domain (pure, no I/O)

### 1.1 Statuses, transitions, errors

`internal/domain`:
- Types `TenantStatus`, `TaskStatus`, `TaskType`, and `Tenant` / `Task` structs.
- Tenant guards: `CanPatch(status)`, `CanDelete(status)`.
- `TerminalOutcome(taskType, taskStatus) (newTenantStatus, expectedTenantStatus)` (§4 table).
- Task rank rule: `ShouldApply(current, incoming TaskStatus) bool`.
- Error values carrying the contract codes: `tenant_not_found`, `tenant_already_exists`, `tenant_update_not_allowed`, `tenant_version_conflict`, `task_not_found`, plus `ValidationError` with per-field details.
- `ConflictError(expectedVersion, current)`: the version-before-status precedence (§5) as a pure function.

**Tests:** table-driven. Every tenant status × {PATCH, DELETE}; every (task status, incoming) pair; every (type, outcome); the precedence cases.
**Commit:** `domain: add tenant and task state machines and error codes`

### 1.2 Validation and formatting

- Slug regex `^[a-z][-a-z0-9]{1,26}[a-z0-9]$`.
- `NormalizeName`: trim, then non-empty and at most 200 characters.
- `FormatTime`: `.UTC()`, then `2006-01-02T15:04:05.000Z`.

**Tests:** slug lengths 2/3/28/29, leading digit, trailing dash, uppercase; whitespace-only name; 200 vs 201 characters; time formatting in a non-UTC zone.
**Commit:** `domain: add slug, name and timestamp rules`

---

## Phase 2: Persistence

### 2.1 Schema, migrations, sqlc

- `internal/store/migrations/00001_init.sql`: the §3 schema. goose `Up`/`Down`, embedded with `embed.FS`.
- `internal/store/queries/{tenants,tasks,outbox,inbox}.sql`: only the queries needed by 2.2. Relay and consumer queries are added in their own subtasks.
- `sqlc.yaml` with the §3 overrides; `make generate` (sqlc in a container); `sqlc diff` added to `make check`.
- `store.Open(ctx, url)`: `pgxpool` plus goose `Up`.

**Tests:** migrations apply to an empty database, and re-running them is a no-op.
**Commit:** `store: add schema, goose migrations and sqlc setup`

### 2.2 Repository: tenant and task operations

`internal/store`, mapping `sqlcgen` rows ↔ domain types:
- `CreateTenant`: one tx doing insert tenant → insert task (`deploy`) → build event JSON → insert outbox. `23505` on `tenants_slug_key` → `tenant_already_exists`.
- `PatchTenant`: guarded UPDATE (§5), then the diagnosing `SELECT` on 0 rows, then the task and outbox inserts.
- `DeleteTenant`: status guard only.
- `GetTenant`, `ListTenants`, `GetTask`, `ListTasks(tenantID?)`, all keyset on `id DESC`.

**Tests (real Postgres):** each operation's happy path and each error; exactly one outbox row per successful mutation and none on a rejected one; a version bump on each write; pagination boundaries.
**Commit:** `store: add tenant and task repository with outbox writes`

### 2.3 Transient-error classification

- `store.IsTransient(err)`: the SQLSTATE rule from §6, plus network and pgx connection errors.

**Tests:** table test of error codes (`08006`, `40P01`, `57P01` → transient; `23514`, `22P02`, a plain error → permanent).
**Commit:** `store: classify transient database errors`

---

## Phase 3: HTTP API

### 3.1 Server scaffolding and boot

- `internal/api`:
  - `gin.New()`, the slog request logger with a request id, a JSON recovery handler, and `HandleMethodNotAllowed`.
  - JSON `NoRoute`/`NoMethod` handlers.
  - `respondError` (domain error → status and body, §7).
  - A decode helper with `DisallowUnknownFields` and a size limit.
  - `GET /healthz` (pings the database).
- `cmd/controlplane`:
  - An `errgroup` with a signal-cancelled context.
  - An explicit `http.Server` with timeouts and `Shutdown(ctx)`.
  - Migrations run on start.
- `compose.yaml`: the `controlplane` service. It uses `restart: unless-stopped` and a `wget` health check, and depends on postgres being healthy. The API is published on `127.0.0.1:${API_HOST_PORT:-8080}`.
- `Makefile`: `up`, `down`, `clean`, `logs`.

**Tests:** an unknown route returns 404 JSON; a wrong method returns 405 JSON; a panicking handler returns 500 `internal_error` with no stack trace; an unknown body field returns 400.
**Done when:** `make up && curl localhost:8080/healthz` returns 200, and `make down && make up` keeps the data.
**Commit:** `api: add gin server, error mapping and controlplane boot`

### 3.2 Tenant and task endpoints

- The seven routes from §7, with their status codes, the `Location` header, `{tenant, task}` bodies, and `?limit=` / `?cursor=` / `?tenant_id=` handling. A malformed path UUID returns `*_not_found`.

**Tests (httptest plus real Postgres):** every route's happy path; every error code with its status; validation details; pagination (`next_cursor` present, then null).
**Done when:** creating a tenant with curl returns 201 with `provisioning`. It stays there, because there's no relay yet.
**Commit:** `api: add tenant CRUD and task read endpoints`

### 3.3 Concurrency tests

The in-repo race demonstration (§11). These run real concurrent HTTP requests against an `httptest.Server`:
- 50 creates with the same slug → exactly 1 × 201 and 49 × 409 `tenant_already_exists`.
- 50 PATCHes with the same version → exactly 1 × 202, 49 × 409 `tenant_version_conflict`, one open task, version +1.
- 20 DELETEs → exactly 1 × 202 and 19 × 409 `tenant_update_not_allowed`.

All requests start behind a shared barrier, so they really do race. Run with `-race`.
**Commit:** `test: add concurrent create, patch and delete race tests`

---

## Phase 4: Outbound messaging

### 4.1 Messaging package: topology and envelopes

- `internal/messaging`:
  - `Dial(url)` (plain for now; reconnect comes in 7.1).
  - `DeclareTopology(ch)`: the exchanges, quorum queues with `x-delivery-limit`, `dlx` and both DLQs (§6).
  - Envelope types `TaskEvent` and `TaskUpdate`, with `TaskUpdate.Validate()`.
- `UpdateID(taskID, status)`: the UUIDv5 id from §6.

**Tests:** declaring twice is idempotent (on the `test` vhost); the validation table covers bad UUIDs, `accepted`, and `error` on a non-failed update; `UpdateID` is deterministic and differs per status.
**Commit:** `messaging: add topology, envelopes and deterministic update ids`

### 4.2 Outbox relay

- `internal/outbox`:
  - A `Publisher` interface; the AMQP implementation uses confirms, `mandatory=true` and `NotifyReturn`.
  - The relay loop (§6): claim with `SKIP LOCKED`, publish the batch, wait for confirms with a timeout, mark the confirmed rows, commit, and back off outside the tx on failure.
- Relay queries added to `outbox.sql`. The relay is wired into the controlplane `errgroup`.
- `compose.yaml`: the controlplane also depends on rabbitmq being healthy.

**Unit tests (fake publisher):** broker down → rows stay unpublished and are retried; a partial batch marks only the confirmed rows; a return or timeout counts as not confirmed.
**Integration tests:** a committed create → exactly one message on `worker.tasks` carrying id/type/tenant_id/status; a rejected PATCH → no message.
**Done when:** after `make up` and a create, the RabbitMQ UI shows one message in `worker.tasks`.
**Commit:** `outbox: add relay publishing committed events with confirms`

---

## Phase 5: Worker simulator

### 5.1 Worker

- `internal/worker` and `cmd/worker`:
  - Flags and `WORKER_*` env vars (`--min-delay-ms`, `--max-delay-ms`, `--fail-rate`), with validation (min ≤ max, 0 ≤ rate ≤ 1).
  - Consume with prefetch 4 and 4 goroutines.
  - Per task: delay → publish `in_progress` (confirmed) → delay → publish `done`/`failed` (confirmed) → ack.
  - A malformed task → nack without requeue (to `worker.tasks.dlq`).
- `compose.yaml`: the `worker` service with `command: worker ${WORKER_ARGS:-}` and `restart: unless-stopped`.
- `Makefile`: `worker` target (`--force-recreate`, §10).

**Tests:** flag validation; `--fail-rate=0` and `1` give deterministic outcomes; a handler test with a fake publisher checks that the ack comes after both publishes.
**Done when:** after `make up` and a create, two messages land in `controlplane.task-updates`, and `make worker ARGS="--fail-rate=1"` replaces the worker.
**Commit:** `worker: add provisioning simulator with configurable delay and failure`

---

## Phase 6: Inbound consumer

### 6.1 Apply-update transaction

- `store.ApplyUpdate(ctx, update) (Result, error)`, in one tx (§6 pseudocode):
  - `INSERT inbox ... ON CONFLICT DO NOTHING` → `SELECT task FOR NO KEY UPDATE` → rank check → UPDATE the task.
  - If the outcome is terminal: the guarded tenant UPDATE.
- Result is `Applied | Duplicate | Stale`. Errors are `ErrTaskNotFound` and `ErrInvariant`, both permanent.

**Tests (real Postgres):**
- A duplicate changes nothing, not even the version.
- `in_progress` after `done` is stale.
- `accepted → done` is applied.
- Any update to a terminal task is stale.
- An unknown task gives `ErrTaskNotFound`.
- A tenant not in the expected status rolls back (task unchanged).
- The same `update_id` from 10 goroutines is applied exactly once.
- `in_progress` and `done` at the same time give a consistent final state.

**Commit:** `store: apply worker updates idempotently and order-tolerantly`

### 6.2 AMQP consumer loop

- `internal/consumer`:
  - Prefetch 16 and 4 handler goroutines.
  - Decode and validate; poison → nack without requeue.
  - Recover panics as permanent failures.
  - A transient error → retry in process with capped, jittered backoff, holding the message; on shutdown, nack with requeue.
  - Structured logs carry `update_id`, `task_id` and the result.
- The consumer is wired into the controlplane `errgroup`.

**Unit tests (fake store):** a transient error is retried and acked only after success; a permanent error is nacked straight away; a panic is recovered.
**Integration tests:** a malformed message, an `accepted` update and an unknown task each go to `controlplane.task-updates.dlq`, and a valid message sent afterwards is still applied.
**Done when:** after `make up`, a created tenant reaches `active` and its task reaches `done`, both visible through the API.
**Commit:** `consumer: add task-update consumer with DLQ and transient retry`

### 6.3 End-to-end test

- A test that runs the API, relay, consumer and worker in-process against the `test` vhost:
  - create → the tenant becomes `active` and the task `done`;
  - with fail-rate 1 → `failed`, then DELETE → `destroyed`.
- It uses short worker delays, and polls the API with a deadline instead of sleeping.

**Commit:** `test: add end-to-end lifecycle test`

---

## Phase 7: Resilience, tooling, docs

### 7.1 Broker reconnect helper

- `messaging.Run(ctx, url, fn)`: dial → declare topology → `fn(ch)` → on `NotifyClose`, back off with jitter and repeat until `ctx` is done (§6). It replaces the plain `Dial` in the relay, consumer and worker.

**Tests (fake dialer):** a closed connection triggers a redial with growing backoff; a cancelled context stops the loop; the topology is re-declared on every connect.
**Done when:** `docker compose restart rabbitmq` during `make up` leaves all processes running, and a create made afterwards still completes.
**Commit:** `messaging: reconnect with backoff after broker connection loss`

### 7.2 publish-update script

- `scripts/publish-update.sh`: `curl` to the management publish endpoint on `127.0.0.1:15672`. Options: `--task-id`, `--status`, `--update-id` (else random and printed), `--error`, `--raw`.

**Done when:** the duplicate, stale and poison recipes from §8 behave as documented against `make up`.
**Commit:** `scripts: add publish-update.sh for injecting worker updates`

### 7.3 README and a cold-run check

- `README.md`: architecture diagram (from §2), design decisions and trade-offs (condensed from `DESIGN.md` §5, §6, §12), setup (`make up`, `make test`, `make check`), API curl examples, negative scenarios (`make worker ARGS=...` and the script recipes), and the error-code table.
- Verification: a fresh `git clone` into a temp dir, then `make up`, a create/poll walkthrough, `make test`, `make check`, `make clean`.

**Commit:** `docs: add README with setup, usage and design notes`

---

## Cut line

The brief's hard requirements are met at the end of **6.3** plus the script (7.2) and a README (7.3). If scope has to shrink, cut in this order:

1. **7.1 reconnect helper.** Fallback: a lost broker connection makes the process exit, and `restart: unless-stopped` brings it back (§13). Document it in the README.
2. **6.3 end-to-end test.** The 6.1/6.2 integration tests plus a documented manual walkthrough cover the lifecycle.
3. **A shorter README:** setup, curl examples and negative scenarios first; trade-offs as bullets linking to `DESIGN.md`.

7.2 and the README are never cut: the brief requires reproducible negative scenarios and documented setup.
