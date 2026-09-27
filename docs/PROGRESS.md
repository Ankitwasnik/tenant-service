# Progress

Tracks the subtasks in [`PLAN.md`](PLAN.md). Update a row when its subtask changes state; the commit column is filled when the subtask's commit lands.

**Status legend:** ⬜ not started · 🔄 in progress · ✅ done · ⏭️ cut (see *Cut line* in `PLAN.md`) · ⛔ blocked

A subtask is ✅ only when its commit is in and `make check` passes on it, plus its *Done when* check from `PLAN.md` where it has one.

| #   | Subtask                                   | Status | Commit | Notes |
| --- | ----------------------------------------- | ------ | ------ | ----- |
| **0** | **Skeleton and quality gates**          |        |        |       |
| 0.1 | Build and test harness                    | ✅     | `1fbe5a3` | `make test` passes cold; cleanup verified on success and on failure; image builds and runs as non-root. `make check` only exists from 0.2, so the gate here was `make test`. |
| 0.2 | Lint, format, vuln scan                   | ✅     | `825ad59` | `make check` passes. A misformatted file fails `fmt-check`, and lint issues (including gosec) fail `lint`. Pinned: golangci-lint v2.14.0 (built with go1.27.0), govulncheck v1.8.0. |
| **1** | **Domain**                              |        |        |       |
| 1.1 | Statuses, transitions, errors             | ✅     | `3c2965f` | `make check` passes; 70 test cases, 100% statement coverage of `internal/domain`. Adds `github.com/google/uuid` v1.6.0. |
| 1.2 | Validation and formatting                 | ✅     | `5d40e63` | `make check` passes; `internal/domain` still at 100% statement coverage. |
| **2** | **Persistence**                         |        |        |       |
| 2.1 | Schema, migrations, sqlc                  | ✅     | `6c6d690` | `make check` passes, now including `sqlc diff`; a stale query fails it. Integration tests confirmed running, not skipped, and every per-test database is dropped. Pinned: pgx v5.11.0, goose v3.28.0, sqlc 1.31.1. |
| 2.2 | Repository: tenant and task operations    | ✅     | `1daa8f3` | `make check` gates pass; 48 store tests and subtests, 87.4% coverage of `internal/store` (the uncovered lines are database-failure branches). |
| 2.3 | Transient-error classification            | ✅     | `ca5a5ce` | `make check` passes. 34 unit cases, plus a real terminated backend: pgx returns `PgError` 57P01, which is classified as transient. |
| **3** | **HTTP API**                            |        |        |       |
| 3.1 | Server scaffolding and boot               | ✅     | `3b07c62` | `make check` passes. `make up` → `/healthz` 200. Data survives `make down && make up`. SIGTERM → `shutting down` / `stopped`, exit 0 at once. Pinned: gin v1.12.0. |
| 3.2 | Tenant and task endpoints                 | ✅     | `6608b42` | `make check` gates pass; 73 API tests and subtests, 95.3% coverage of `internal/api`. Live: a curl create returns 201 + `Location`, with the tenant in `provisioning` (the `curl-demo` tenant is left in the dev DB; its event should go out once the relay lands in 4.2). |
| 3.3 | Concurrency tests                         | ✅     | `2b0b980` | `make check` passes. 49–50 of 50 requests in flight at once. Stable over 10 consecutive runs (120 races). With the PATCH guard deliberately broken, the PATCH race test fails. |
| **4** | **Outbound messaging**                  |        |        |       |
| 4.1 | Messaging package: topology and envelopes | ✅     | `13719b2` | `make check` passes. Routing, dead-lettering and the delivery limit were verified against real RabbitMQ, one vhost per test; stable over 3 runs. Pinned: amqp091-go v1.15.0. |
| 4.2 | Outbox relay                              | ✅     | `fd93574` | `make check` passes; 12 outbox tests, stable over 3 runs. Live: on start the relay published the `curl-demo` event from 3.2, and a new create put a second message in `worker.tasks`. |
| **5** | **Worker simulator**                    |        |        |       |
| 5.1 | Worker                                    | ✅     | `3399448` | `make check` passes. Live: the worker drained the 2 queued tasks, a create gave `in_progress` + `done`, and `make worker ARGS="--fail-rate=1"` replaced it (one container) so a create ended `failed`. 8 updates now wait in `controlplane.task-updates` for the consumer (6.2). |
| **6** | **Inbound consumer**                    |        |        |       |
| 6.1 | Apply-update transaction                  | ✅     | _not committed yet_ | `make check` passes; 17 update tests and subtests, stable over 5 runs. With the task-row lock removed, the concurrency test fails 20/20 (task regressed to `in_progress`). |
| 6.2 | AMQP consumer loop                        | ⬜     |        |       |
| 6.3 | End-to-end test                           | ⬜     |        |       |
| **7** | **Resilience, tooling, docs**           |        |        |       |
| 7.1 | Broker reconnect helper                   | ⬜     |        |       |
| 7.2 | publish-update script                     | ⬜     |        |       |
| 7.3 | README and a cold-run check               | ⬜     |        |       |

## Deviations from the plan

Record here anything built differently from `DESIGN.md` / `PLAN.md`, and why. If a deviation changes the design, update `DESIGN.md` too.

- **0.1:** added two small things the plan didn't list. `.dockerignore` keeps `.git`, `.env` and docs out of the build context. The runtime image runs as a non-root `app` user (uid 10001). Neither changes the design.
- **0.2:**
  - **More linters than the plan listed.** Besides `gosec`, `errcheck`, `staticcheck` and `revive`: the rest of golangci's `standard` set (`govet`, `ineffassign`, `unused`), plus `errorlint`, `bodyclose`, `misspell` and `unconvert`. `errorlint` matters later: it enforces `errors.Is`/`errors.As` for pgx and domain errors.
  - **`make test` now uses `-count=1`.** Go was serving the test result from its cache. From 2.1 on, the suite depends on the database and broker, so results must never be reused.
  - **Separate `fmt-check` target** (`golangci-lint fmt --diff`), which `check` runs first.
  - **Tool service:** the linter runs as a `lint` compose service (profile `tools`), and `govulncheck` runs in the `tests` service. Neither starts postgres or rabbitmq (`--no-deps`).
- **1.1:**
  - **The plan's single `ConflictError(expectedVersion, current)` is split in two.** `PatchConflict` checks the version, then the status. `DeleteConflict` checks only the status, since DELETE takes no version. Both return `nil` when the operation would be allowed against the current row, which only happens if the row changed between the guarded UPDATE and the read; the store (2.2) then retries the UPDATE. This covers a gap in DESIGN.md §5: a DELETE on a `provisioning` tenant whose deploy fails in between would otherwise report `tenant_update_not_allowed` for a tenant that is now deletable.
  - **Validation errors** are built with `FieldErrors` (`Add(field, msg)`, then `Err()`), so 1.2 and the API can collect every field problem before failing.
  - **Small additions (1.1):** `Valid()` on each enum type (for the store's DB → domain mapping and for message validation in 4.1), `TaskStatus.Terminal()`, and `CodeOf(err)` for mapping errors to codes through `%w` wrapping.
- **1.2:**
  - **`ValidateCreate(slug, name)` added** beyond the plan's list. It checks both fields together and returns one `validation_error` naming every invalid field, so a client doesn't have to fix them one at a time. The create handler (3.2) calls it; PATCH uses `NormalizeName` directly.
  - **Slug errors are split into two messages:** a length message ("must be 3-28 characters") and a format message. The regex alone already enforces the length, but a separate message is clearer for a slug that's too short or too long.
  - **Name length is counted in characters (runes), not bytes**, so 200 accented characters are allowed. DESIGN.md §7 says "200 characters", so this matches it.
- **2.1:**
  - **Each integration test gets its own database.** `internal/testutil.NewDatabase` creates an empty database on the test server and drops it afterwards, so tests can run in parallel with a clean schema. `controlplane_test` (from `make test`) is only the admin connection. DESIGN.md §10 is updated.
  - **Integration tests fail rather than skip** when `TEST_DATABASE_URL` is missing, so a misconfigured `make test` can't pass silently. `go test -short ./...` on the host skips them and runs only the unit tests.
  - **No `inbox.sql` yet.** Its only queries belong to the consumer (6.1). sqlc needs no empty file, and the plan's rule is to add queries with the code that uses them.
  - **Extra schema tests** beyond "migrations apply, and re-running is a no-op": UUIDv7 ids, new-tenant defaults, millisecond timestamps, and the unique-slug, one-open-task and non-empty-name constraints, which the repository (2.2) relies on.
  - **sqlc runs as the host user** (`--user $(id -u):$(id -g)`), so on Linux the generated files aren't owned by root. The staleness check is its own target, `make sqlc-check`, and runs in `make check` after the format check.
- **2.2:**
  - **`messaging.TaskEvent` added now instead of in 4.1.** The store has to write the event JSON into the outbox, so the envelope type and `TaskRoutingKey` live where 4.1 expects them, rather than ad-hoc JSON that moves later. 4.1 adds the update envelope, topology and `UpdateID`.
  - **List methods return the next-page cursor.** They fetch `limit + 1` rows to know whether another page exists, with no count query. The API (3.2) only encodes the result. The store rejects limits outside 1-1000; the API applies its own default of 50 and maximum of 200.
  - **PATCH and DELETE share one `mutate` helper**, which does the guarded UPDATE, the diagnosis on a miss, and the bounded retry (3 attempts) from the 1.1 design change. The retry is tested with a guard that misses on purpose: the real race window is too narrow to hit reliably.
  - **A PATCH version outside int32** (such as 0 or 2³¹) matches no row by definition. The UPDATE is skipped and the normal diagnosis answers: `tenant_version_conflict`, or `tenant_not_found` for an unknown id.
  - **Test stand-in for the worker:** `finishTask` in the tests closes the open task and moves the tenant on with plain SQL, until the consumer (6.1) exists.
- **2.3:**
  - **`40002` is permanent**, although it's in class 40. It means a deferred constraint failed at commit, and retrying gives the same result. DESIGN.md §6 names class 40 as a whole, so this is a refinement.
  - **Cancelled and timed-out contexts count as transient.** The operation didn't fail on its own merits, and it means a shutdown requeues the in-flight message instead of dead-lettering it (DESIGN.md §6).
  - **The server's error code is checked first.** A failed connection can wrap a server error, so "starting up" (`57P03`) is retried but a wrong password (`28P01`) is not.
  - **An integration test beyond the plan** terminates its own backend (`pg_terminate_backend`) to check what a Postgres restart looks like to a query: `57P01`, transient.
- **3.1:**
  - **quic-go upgraded from v0.59.0 to v0.59.1.** gin v1.12.0 pulls in quic-go for optional HTTP/3, and `govulncheck` failed `make check` on GO-2026-5676 (QPACK memory exhaustion), reachable from `gin.New`. v0.59.1 is the smallest version with the fix; the newer v0.6x releases aren't what gin v1.12.0 was built against.
  - **A new `service_unavailable` code (503)**, used only by `/healthz` when the database doesn't answer, so that every non-2xx response keeps the error envelope. DESIGN.md §7 is updated.
  - **Request ids:** a client's `X-Request-ID` is kept if it matches `^[A-Za-z0-9._-]{1,64}$` (so it can't inject text into the logs); otherwise a UUID is generated. Either way it's echoed in the response and attached to every log line of the request.
  - **Successful `/healthz` calls aren't logged.** The compose health check polls every 5 s, which would bury the real request logs.
  - **Decode errors name the field:** an unknown field, a wrong JSON type, malformed or trailing JSON, and a body over 64 KiB are all `validation_error`s, with `details` naming the field (or `body`). A too-large body is a 400 rather than a 413, which keeps the code list short.
  - **The compose health check uses `wget -q -O /dev/null`**, which fails on a non-2xx response. DESIGN.md §10 is updated to match.
- **3.2:**
  - **PATCH validation happens in the API** before the store is called:
    - A missing `name` or `version` is "is required".
    - A `version` below 1 is a 400 "must be a positive integer". Such a version could never match; the store's version-conflict path now only sees values above int32.
    - Sending `slug` is "cannot be changed" rather than the generic "is not a known field", so the client learns why.
    - Every problem is reported in one response.
  - **All bad list parameters are reported at once:** `limit`, `cursor` and `tenant_id` each get their own entry in `details`.
  - **A malformed path id's message quotes the id** (`"nope" is not a known id`), with the same `*_not_found` code as an unknown one.
  - **JSON views are separate from the domain types** (`views.go`), so the wire format can't change by accident when a domain struct does. An empty page is `"items": []`, never `null`. `error` is omitted from a task unless it failed.
  - **`FinishTask` / `SetTenantStatus` moved to `internal/testutil`**, so the store and API tests share one stand-in for the worker.
- **3.3:**
  - **A fourth race beyond the plan:** 20 PATCHes and 20 DELETEs at the same version, on the same tenant. Exactly one request wins, whichever kind. Losing PATCHes get `tenant_version_conflict` and losing DELETEs `tenant_update_not_allowed`, the version goes up by exactly 1, and there is one open task.
  - **Each race runs 3 rounds** on fresh tenants, and asserts the database afterwards as well as the HTTP counts: one tenant, task and outbox row for creates; the winner's name stored; version +1; exactly one open task, of the winner's type.
  - **Proof of overlap:** a wrapper counts requests inside the handler at once, and the test fails if the peak is below 2. In practice it's 49–50 of 50.
  - **The pool is sized for contention:** `pool_max_conns=25`, so most racing requests hold a database connection at once and the race happens in Postgres, not in the Go pool's queue. The race tests aren't `t.Parallel`, which keeps the total connections well under Postgres's limit of 100.
  - **Mutation check:** with the PATCH guard's `status` and `version` conditions removed from the generated SQL, the PATCH race gives 1 × 202 and 49 × 500 and the test fails. It also showed the safety net working: the `tasks_one_open_per_tenant` index rejected the 49 extra transactions. The generated file was restored from git afterwards.
- **4.1:**
  - **Finding: in RabbitMQ 4.3, a `nack` with requeue doesn't count toward `x-delivery-limit`.** A message requeued 30 times was never dead-lettered. A crash (the channel closes with the message unacked) does count, and after 1 + 10 deliveries the message goes to the DLQ with reason `delivery_limit`. DESIGN.md §6 had this backwards ("a shutdown requeue does count") and is corrected. Two tests now pin the behavior: `TestDeliveryLimitStopsCrashLoop` and `TestRequeueByNackIsNotCounted`. Consequence: the consumer (6.2) must never retry by requeueing, which the design already avoids.
  - **Each RabbitMQ test gets its own vhost.** `testutil.NewVhost` creates it through the management API and deletes it afterwards, the analogue of `NewDatabase`. Without this, tests in different packages (4.2, 6.2) would consume each other's messages from the same queue names. The compose `tests` service gains `TEST_RABBITMQ_API_URL`. `testutil.Management` also reads a queue's type and arguments, which AMQP itself doesn't expose.
  - **More than "declaring twice is idempotent":** the tests also check that the queues are durable quorum queues with the right arguments, that a conflicting declaration gets `PRECONDITION_FAILED`, that each exchange routes only to its own queue, and that a rejected message reaches its DLQ.
  - **Additions:** `NewTaskUpdate` builds an update with its deterministic id and drops `error` unless the status is `failed`. `DecodeTaskUpdate` wraps every failure in `ErrInvalidMessage`, so the consumer can dead-letter it. It also pins the message types (`task.v1`, `task_update.v1`) and `UpdateRoutingKey`.
  - **Unknown JSON fields in an update are ignored**, not rejected, so a newer worker can add fields without its messages being dead-lettered by an older control plane.
  - **The UUIDv5 namespace is pinned in a test.** Changing it would give a re-run of an old task new update ids, and the inbox would stop deduplicating them.
- **4.2:**
  - **Bug found and fixed:** `drainReturns` spun forever once the AMQP channel closed. amqp091 closes the returns channel, and a receive on a closed Go channel succeeds at once, every time. In production a broker outage would have pinned a CPU and stopped the relay from backing off. `TestAMQPPublisherClosedChannel` caught it as a hang; the fix checks the receive's `ok` value.
  - **`make test` now passes `-timeout 5m`.** That hang ran into go test's default 10-minute timeout; a future one fails in minutes.
  - **The claim-publish-mark transaction lives in the store** (`store.RelayOutbox`, which takes a `PublishFunc`), so all SQL stays in the repository. `internal/outbox` holds the loop, backoff and AMQP publisher. The store marks only ids from the claimed batch, even if a publisher reports others.
  - **The loop's cadence:** after a full batch the relay goes again at once, since more may be waiting. Otherwise it waits the 200 ms poll interval. After a failure it waits a jittered exponential backoff (100 ms doubling to 10 s, spread over [d/2, d] so replicas don't retry in lockstep).
  - **Deferred confirms:** the publisher uses amqp091's `PublishWithDeferredConfirmWithContext`. The whole batch goes out first, then all its confirms are awaited under one 5 s timeout. Returned messages are matched to the batch by `message_id` (the task id). The returns buffer (1024) is well above the batch size, because amqp091 delivers returns synchronously and a full buffer would stall the connection.
  - **A lost broker connection stops the process** until the reconnect helper (7.1): `main` watches `NotifyClose` and returns an error, the errgroup shuts everything down, and compose restarts the container. This is the documented fallback in PLAN.md's cut line.
  - **Tests beyond the plan:** three relays on one outbox publish each of 60 events exactly once (SKIP LOCKED); `Run` backs off through 3 failures, then publishes and stops cleanly on cancel; ids outside the batch are ignored; closed-channel and 1 ns-timeout cases against real RabbitMQ.
- **5.1:**
  - **The confirmed-publish logic moved to `messaging.ConfirmPublisher`**, shared by the relay and the worker, so the confirm and return handling (and 4.2's closed-channel fix) exists once. `outbox.AMQPPublisher` is now a thin adapter that maps outbox rows to messages. All the outbox tests pass unchanged.
  - **Settling rules beyond "ack last"**, now in DESIGN.md §8:
    - A malformed task is nacked to the DLQ.
    - A task interrupted by shutdown is requeued, which is free per 4.1's finding.
    - A failed update publish leaves the task unacked and stops the worker. The redelivery is counted, so it's bounded by the delivery limit.
  - **`messaging.DecodeTaskEvent` added:** the worker rejects an event with a missing id or tenant, an unknown type, or a status other than `accepted`.
  - **The flag and env precedence:** a `WORKER_*` env var sets the default and the flag overrides it. Unknown flags (such as the cut `--seed`) and stray arguments are errors.
  - **The fail-rate rule is `random() < rate`** with `random` in [0, 1), so 0 never fails and 1 always does. The tests inject the random value to pin both ends and the threshold.
  - **Each worker goroutine has its own publisher channel**, since a `ConfirmPublisher` isn't safe for concurrent use.
- **6.1:**
  - **Tests beyond the plan:**
    - The full lifecycle, where `in_progress` doesn't touch the tenant or its version.
    - All five other (task type, outcome) pairs: deploy failed, update done or failed, destroy done or failed.
    - A failed update with no error message.
    - A re-run's opposite outcome (`failed` after `done`, a different update id) is stale, so a redelivered task can't flip its result.
    - Both permanent errors are checked not to be classified as transient, so the consumer (6.2) dead-letters them.
  - **Mutation check:** with `FOR NO KEY UPDATE` removed from `LockTask`, `in_progress` and `done` racing leaves the task back at `in_progress` after the tenant was already made active. That's the regression the lock prevents, and the test failed 20 of 20 runs.
  - **My mistake during that check, caught by the gates:** I restored the generated file with `git checkout`. The committed version predates this subtask, so that also removed the new `LockTask` and `UpdateTaskStatus` code, and `make sqlc-check` failed on it. Regenerating fixed it. For generated files, restore by running `make generate`, not `git checkout`.
  - **Both permanent errors roll back the inbox row too**, so a message replayed from the DLQ after a fix is processed afresh rather than skipped as a duplicate.
  - **`error` is stored only for a `failed` update that carries one;** otherwise it stays NULL.

## Open issues

Anything found along the way that isn't fixed in the current subtask.

- **golangci-lint reports at most one issue per line** (its default). A gosec finding can hide behind an errcheck finding on the same line until the first one is fixed. That's harmless, because the gate still fails, but it can look like gosec missed something.
- **gin links `go.mongodb.org/mongo-driver` into the binary** through its BSON binding and render packages, which have no build tag to leave them out. The cost is binary size only (about 26 MB unstripped); nothing calls BSON, and `govulncheck` only reports code that is reachable.
