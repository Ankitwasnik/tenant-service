# Everything runs in containers: the host needs only Docker and make (DESIGN.md §10).

COMPOSE := docker compose

# Test isolation: a throwaway database and RabbitMQ vhost on the dev stack's
# containers. Must match the `tests` service URLs in compose.yaml.
TEST_DB    := controlplane_test
TEST_VHOST := test

.PHONY: test test-reset test-drop

## test: run the full suite with -race against a fresh test database and vhost
# The suite's exit status is kept, so cleanup runs even when tests fail.
test:
	$(COMPOSE) up -d --wait postgres rabbitmq
	@$(MAKE) --no-print-directory test-reset
	@$(COMPOSE) run --rm tests go test -race ./...; status=$$?; \
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
