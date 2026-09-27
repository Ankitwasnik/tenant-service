# Everything runs in containers: the host needs only Docker and make (DESIGN.md §10).

COMPOSE := docker compose

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

.PHONY: up down clean logs check fmt fmt-check lint vuln generate sqlc-check test test-reset test-drop

## up: build and start the whole stack, and wait until every service is healthy
up:
	$(COMPOSE) up --build -d --wait

## down: stop and remove the containers; volumes (and so tenants) are kept
down:
	$(COMPOSE) down

## clean: like down, and also remove the volumes, for a fresh start
clean:
	$(COMPOSE) down --volumes

## logs: follow the logs of every service
logs:
	$(COMPOSE) logs -f

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

## lint: static analysis, including gosec
lint:
	$(LINT) golangci-lint run

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
# -count=1: never reuse cached results, since the suite depends on the database and
# broker, not just the code. The exit status is kept, so cleanup runs on failure too.
test:
	$(COMPOSE) up -d --wait postgres rabbitmq
	@$(MAKE) --no-print-directory test-reset
	@$(COMPOSE) run --rm tests go test -race -count=1 ./...; status=$$?; \
	$(MAKE) --no-print-directory test-drop; \
	exit $$status

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
