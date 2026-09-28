# Everything runs in containers: the host needs only Docker and make (DESIGN.md §10).

COMPOSE := docker compose

# compose.yaml has no defaults: every setting comes from .env (git-ignored).
# On a fresh clone there is no .env yet, so create it from the template;
# an existing .env is never touched.
ifeq ($(wildcard .env),)
$(shell cp .env.template .env)
$(info Created .env from .env.template)
endif

# Test isolation: a throwaway database and RabbitMQ vhost on the dev stack's
# containers. Must match the `tests` service URLs in compose.yaml.
TEST_DB    := controlplane_test
TEST_VHOST := test

GOVULNCHECK_VERSION := v1.8.0

# Tool containers never need postgres or rabbitmq.
LINT := $(COMPOSE) run --rm --no-deps lint
GO   := $(COMPOSE) run --rm --no-deps tests
# sqlc runs as the host user, so generated files aren't owned by root on Linux.
SQLC := $(COMPOSE) run --rm --no-deps --user "$(shell id -u):$(shell id -g)" sqlc

.PHONY: up down clean logs worker race race-demo check fmt fmt-check lint vuln generate sqlc-check test test-reset test-drop

## up: build and start the whole stack, and wait until every service is healthy
up:
	$(COMPOSE) up --build -d --wait

## down: stop and remove the containers; volumes (and so tenants) are kept
down:
	$(COMPOSE) down

## clean: like down, and also remove the volumes (data and the Go/lint caches), for a fresh start
# The profiles are needed: down skips services whose profile isn't active,
# and with them the cache volumes that only the tests and lint services use.
clean:
	$(COMPOSE) --profile test --profile tools down --volumes

## logs: follow the logs of every service
logs:
	$(COMPOSE) logs -f

## worker: replace the running worker with one using ARGS, e.g. make worker ARGS="--fail-rate=1"
# Recreates the one worker service rather than starting a second: two workers
# would share worker.tasks, and only some tasks would see the new ARGS.
worker:
	WORKER_ARGS="$(ARGS)" $(COMPOSE) up -d --no-deps --force-recreate worker

## check: every quality gate, in order; stops at the first failure
check:
	@$(MAKE) --no-print-directory fmt-check
	@$(MAKE) --no-print-directory sqlc-check
	@$(MAKE) --no-print-directory lint
	@$(MAKE) --no-print-directory vuln
	@$(MAKE) --no-print-directory test

## fmt: format all Go code in place (gofumpt + goimports)
fmt:
	$(LINT) golangci-lint fmt

## fmt-check: fail if any Go file is not formatted, and show the diff
fmt-check:
	$(LINT) golangci-lint fmt --diff

## lint: static analysis: golangci-lint (including gosec) for Go, shellcheck for scripts/
lint:
	$(LINT) golangci-lint run
	$(COMPOSE) run --rm --no-deps shellcheck -x -P SCRIPTDIR scripts/*.sh scripts/lib/*.sh

## generate: regenerate the sqlc code in internal/store/sqlcgen (committed)
generate:
	$(SQLC) generate

## sqlc-check: fail if the committed sqlc code doesn't match the SQL
sqlc-check:
	$(SQLC) diff

## vuln: scan dependencies and the standard library for known vulnerabilities
vuln:
	$(GO) go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

## test: run the full suite with -race against a fresh test database and vhost
# -timeout 5m: a hang fails in minutes, not after go test's default 10.
# -count=1: never reuse cached results, since the suite depends on the database and
# broker, not just the code. The exit status is kept, so cleanup runs on failure too.
test:
	$(COMPOSE) up -d --wait postgres rabbitmq
	@$(MAKE) --no-print-directory test-reset
	@$(COMPOSE) run --rm tests go test -race -count=1 -timeout 5m ./...; status=$$?; \
	$(MAKE) --no-print-directory test-drop; \
	exit $$status

## race: run just the concurrency tests, verbosely, with each race's outcome counts
# The in-repo demonstration that the race conditions are handled: concurrent
# HTTP creates/PATCHes/DELETEs, concurrent delivery of worker updates, several
# relays on one outbox, many tenants at once. Same isolation as make test.
RACE_TESTS := TestRace|TestApplyUpdateConcurrent|TestConcurrentRelays|TestLifecycleManyTenants
race:
	$(COMPOSE) up -d --wait postgres rabbitmq
	@$(MAKE) --no-print-directory test-reset
	@$(COMPOSE) run --rm tests go test -race -count=1 -timeout 5m -v -run '$(RACE_TESTS)' \
		./internal/api/ ./internal/store/ ./internal/outbox/ ./internal/e2e/; status=$$?; \
	$(MAKE) --no-print-directory test-drop; \
	exit $$status

## race-demo: fire concurrent requests at the running stack (make up) and tally the results
race-demo:
	scripts/race-demo.sh $(ARGS)

# The containers' own POSTGRES_USER / RABBITMQ_DEFAULT_USER are used, so the
# credentials live only in compose.yaml.
test-reset: test-drop
	$(COMPOSE) exec -T postgres sh -c 'createdb -U "$$POSTGRES_USER" $(TEST_DB)'
	$(COMPOSE) exec -T rabbitmq sh -c 'rabbitmqctl -q add_vhost $(TEST_VHOST) && \
		rabbitmqctl -q set_permissions -p $(TEST_VHOST) "$$RABBITMQ_DEFAULT_USER" ".*" ".*" ".*"'

test-drop:
	$(COMPOSE) exec -T postgres sh -c 'dropdb -U "$$POSTGRES_USER" --if-exists --force $(TEST_DB)'
	$(COMPOSE) exec -T rabbitmq sh -c 'if rabbitmqctl -q list_vhosts name | grep -qx $(TEST_VHOST); then \
		rabbitmqctl -q delete_vhost $(TEST_VHOST); fi'
