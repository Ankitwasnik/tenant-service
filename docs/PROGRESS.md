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
| 2.3 | Transient-error classification            | ✅     | _not committed yet_ | `make check` passes. 34 unit cases, plus a real terminated backend: pgx returns `PgError` 57P01, which is classified as transient. |
| **3** | **HTTP API**                            |        |        |       |
| 3.1 | Server scaffolding and boot               | ⬜     |        |       |
| 3.2 | Tenant and task endpoints                 | ⬜     |        |       |
| 3.3 | Concurrency tests                         | ⬜     |        |       |
| **4** | **Outbound messaging**                  |        |        |       |
| 4.1 | Messaging package: topology and envelopes | ⬜     |        |       |
| 4.2 | Outbox relay                              | ⬜     |        |       |
| **5** | **Worker simulator**                    |        |        |       |
| 5.1 | Worker                                    | ⬜     |        |       |
| **6** | **Inbound consumer**                    |        |        |       |
| 6.1 | Apply-update transaction                  | ⬜     |        |       |
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

## Open issues

Anything found along the way that isn't fixed in the current subtask.

- **golangci-lint reports at most one issue per line** (its default). A gosec finding can hide behind an errcheck finding on the same line until the first one is fixed. That's harmless, because the gate still fails, but it can look like gosec missed something.
