-- Initial schema (DESIGN.md §3). Postgres 18: uuidv7() is built in.
-- Ids and timestamps come from the database; timestamptz(3) rounds to milliseconds.

-- +goose Up
CREATE TABLE tenants (
    id          uuid PRIMARY KEY DEFAULT uuidv7(),
    slug        text NOT NULL UNIQUE,          -- never deleted => unique vs destroyed tenants too
    name        text NOT NULL CHECK (name <> ''),
    status      text NOT NULL CHECK (status IN
                  ('provisioning','active','updating','destroying','destroyed','failed')),
    version     integer NOT NULL DEFAULT 1,
    created_at  timestamptz(3) NOT NULL DEFAULT now(),
    updated_at  timestamptz(3) NOT NULL DEFAULT now()
);

CREATE TABLE tasks (
    id          uuid PRIMARY KEY DEFAULT uuidv7(), -- also the event id
    tenant_id   uuid NOT NULL REFERENCES tenants(id),
    type        text NOT NULL CHECK (type IN ('deploy','update','destroy')),
    status      text NOT NULL CHECK (status IN ('accepted','in_progress','done','failed')),
    error       text,                           -- set when the worker reports failed
    created_at  timestamptz(3) NOT NULL DEFAULT now(),
    updated_at  timestamptz(3) NOT NULL DEFAULT now()
);
-- Safety net for "at most one open task per tenant". The status guards make it
-- unreachable; if a bug ever breaks them, the tx fails instead of corrupting state.
CREATE UNIQUE INDEX tasks_one_open_per_tenant ON tasks (tenant_id)
    WHERE status IN ('accepted','in_progress');
CREATE INDEX tasks_tenant_id ON tasks (tenant_id, id DESC);  -- GET /v1/tasks?tenant_id=
-- Unfiltered lists paginate on the primary key (id DESC); no extra index needed.

CREATE TABLE outbox (
    id           bigserial PRIMARY KEY,
    task_id      uuid NOT NULL REFERENCES tasks(id),
    routing_key  text NOT NULL,                 -- task.deploy | task.update | task.destroy
    payload      jsonb NOT NULL,                -- frozen at commit time
    created_at   timestamptz(3) NOT NULL DEFAULT now(),
    published_at timestamptz(3)
);
CREATE INDEX outbox_unpublished ON outbox (id) WHERE published_at IS NULL;

CREATE TABLE inbox (                            -- processed worker updates
    update_id    uuid PRIMARY KEY,              -- idempotency key
    task_id      uuid NOT NULL,                 -- no FK: an unknown task rolls the whole tx back anyway
    received_at  timestamptz(3) NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE inbox;
DROP TABLE outbox;
DROP TABLE tasks;
DROP TABLE tenants;
