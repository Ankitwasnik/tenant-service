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
| 1.2 | Validation and formatting                 | ✅     | _not committed yet_ | `make check` passes; `internal/domain` still at 100% statement coverage. |
| **2** | **Persistence**                         |        |        |       |
| 2.1 | Schema, migrations, sqlc                  | ⬜     |        |       |
| 2.2 | Repository: tenant and task operations    | ⬜     |        |       |
| 2.3 | Transient-error classification            | ⬜     |        |       |
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

## Open issues

Anything found along the way that isn't fixed in the current subtask.

- **golangci-lint reports at most one issue per line** (its default). A gosec finding can hide behind an errcheck finding on the same line until the first one is fixed. That's harmless, because the gate still fails, but it can look like gosec missed something.
