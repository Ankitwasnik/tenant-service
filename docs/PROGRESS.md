# Progress

Tracks the subtasks in [`PLAN.md`](PLAN.md). Update a row when its subtask changes state; the commit column is filled when the subtask's commit lands.

**Status legend:** ⬜ not started · 🔄 in progress · ✅ done · ⏭️ cut (see *Cut line* in `PLAN.md`) · ⛔ blocked

A subtask is ✅ only when its commit is in and `make check` passes on it, plus its *Done when* check from `PLAN.md` where it has one.

| #   | Subtask                                   | Status | Commit | Notes |
| --- | ----------------------------------------- | ------ | ------ | ----- |
| **0** | **Skeleton and quality gates**          |        |        |       |
| 0.1 | Build and test harness                    | 🔄     |        | Implemented and verified; awaiting commit. `make check` only exists from 0.2, so the gate here is `make test`. |
| 0.2 | Lint, format, vuln scan                   | ⬜     |        |       |
| **1** | **Domain**                              |        |        |       |
| 1.1 | Statuses, transitions, errors             | ⬜     |        |       |
| 1.2 | Validation and formatting                 | ⬜     |        |       |
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

## Open issues

Anything found along the way that isn't fixed in the current subtask.

_None yet._
