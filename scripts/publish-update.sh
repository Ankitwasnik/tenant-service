#!/usr/bin/env bash
#
# publish-update.sh: publish a task update straight to the broker, as if the
# worker had sent it. Reproduces the negative scenarios by hand: duplicates,
# out-of-order (stale) updates and poison messages (DESIGN.md §8).
#
# It uses RabbitMQ's management HTTP API (published on 127.0.0.1 by
# `make up`), so it needs only bash and curl.
#
# Usage:
#   scripts/publish-update.sh --task-id <id|latest> --status <status> [options]
#   scripts/publish-update.sh --raw '<body>' [options]
#
# Options:
#   --task-id <id|latest>  The task the update is for. "latest" looks up the
#                          newest task through the API.
#   --status <status>      in_progress, done or failed. Anything else is sent
#                          as-is, and the consumer dead-letters it.
#   --update-id <uuid>     The idempotency key. Default: a new random one,
#                          printed, so it can be replayed.
#   --error <message>      The error message (only valid with failed).
#   --raw <body>           Publish <body> exactly as given (say, not JSON), with
#                          routing key task.update.done. Ignores the options above.
#   --count <n>            Publish the same message n times (default 1).
#   -h, --help             Show this help.
#
# Settings come from the repo's .env, the same file compose.yaml reads (make
# creates it from .env.template): RABBITMQ_USER, RABBITMQ_PASSWORD,
# RABBITMQ_MGMT_HOST_PORT and API_HOST_PORT. A variable already set in the
# environment wins, as it does for docker compose. RABBITMQ_API_URL and
# API_URL, if set, replace the http://127.0.0.1:<port> URLs built from them.
#
# See what the consumer did with it:
#   docker compose logs controlplane | grep '"update processed"\|dead-letter'
#
# Written for bash 3.2 (the macOS default), so no bash 4 features.

set -euo pipefail

usage() { sed -n '3,/^# Written for/p' "$0" | sed 's/^# \{0,1\}//'; }
die() { echo "publish-update: $*" >&2; exit 1; }

# shellcheck source=lib/env.sh
source "$(dirname "$0")/lib/env.sh"
load_env RABBITMQ_USER RABBITMQ_PASSWORD RABBITMQ_MGMT_HOST_PORT API_HOST_PORT

RABBITMQ_API_URL=${RABBITMQ_API_URL:-http://127.0.0.1:$RABBITMQ_MGMT_HOST_PORT}
API_URL=${API_URL:-http://127.0.0.1:$API_HOST_PORT}

task_id="" status="" update_id="" error_msg="" raw="" have_raw=false count=1

while [ $# -gt 0 ]; do
	case $1 in
	--task-id) task_id=${2:?--task-id needs a value}; shift 2 ;;
	--status) status=${2:?--status needs a value}; shift 2 ;;
	--update-id) update_id=${2:?--update-id needs a value}; shift 2 ;;
	--error) error_msg=${2:?--error needs a value}; shift 2 ;;
	--raw) raw=${2?--raw needs a value}; have_raw=true; shift 2 ;;
	--count) count=${2:?--count needs a value}; shift 2 ;;
	-h | --help) usage; exit 0 ;;
	*) die "unknown option $1 (see --help)" ;;
	esac
done

case $count in '' | *[!0-9]* | 0) die "--count must be a positive integer" ;; esac
command -v curl >/dev/null || die "curl is required"

# json_escape prints $1 as the inside of a JSON string.
json_escape() {
	local s=$1
	s=${s//\\/\\\\}
	s=${s//\"/\\\"}
	s=${s//$'\n'/\\n}
	s=${s//$'\r'/\\r}
	s=${s//$'\t'/\\t}
	printf '%s' "$s"
}

new_uuid() {
	if command -v uuidgen >/dev/null; then
		uuidgen | tr '[:upper:]' '[:lower:]'
	elif [ -r /proc/sys/kernel/random/uuid ]; then
		cat /proc/sys/kernel/random/uuid
	else
		die "can't generate a UUID (no uuidgen); pass --update-id"
	fi
}

# latest_task_id asks the API for the newest task. The task JSON starts with
# its id ({"items":[{"id":"..."), so a sed match is enough; no jq needed.
latest_task_id() {
	local body id
	body=$(curl -fsS "$API_URL/v1/tasks?limit=1") || die "can't reach the API at $API_URL"
	id=$(printf '%s' "$body" | sed -nE 's/^\{"items":\[\{"id":"([^"]+)".*/\1/p')
	[ -n "$id" ] || die "no tasks yet: create a tenant first"
	printf '%s' "$id"
}

if $have_raw; then
	routing_key=task.update.done
	message_id=${update_id:-raw-$(new_uuid)}
	payload=$raw
else
	[ -n "$task_id" ] || die "--task-id is required (or use --raw)"
	[ -n "$status" ] || die "--status is required (or use --raw)"
	[ "$task_id" = latest ] && task_id=$(latest_task_id)
	[ -n "$update_id" ] || update_id=$(new_uuid)
	routing_key=task.update.$status
	message_id=$update_id

	payload="{\"update_id\":\"$(json_escape "$update_id")\",\"task_id\":\"$(json_escape "$task_id")\",\"status\":\"$(json_escape "$status")\""
	[ -n "$error_msg" ] && payload="$payload,\"error\":\"$(json_escape "$error_msg")\""
	payload="$payload}"
fi

# The management API's publish endpoint: persistent, JSON, with the same
# message_id and type the worker uses.
request="{\"routing_key\":\"$(json_escape "$routing_key")\",\"payload_encoding\":\"string\",\"payload\":\"$(json_escape "$payload")\",\"properties\":{\"delivery_mode\":2,\"content_type\":\"application/json\",\"type\":\"task_update.v1\",\"message_id\":\"$(json_escape "$message_id")\"}}"

echo "update_id:   $message_id"
echo "routing key: $routing_key"
echo "payload:     $payload"

i=1
while [ "$i" -le "$count" ]; do
	response=$(curl -fsS -u "$RABBITMQ_USER:$RABBITMQ_PASSWORD" \
		-H 'Content-Type: application/json' \
		-X POST "$RABBITMQ_API_URL/api/exchanges/%2F/task-updates/publish" \
		-d "$request") || die "publish failed (is the stack up? make up)"
	case $response in
	*'"routed":true'*) echo "published ($i/$count)" ;;
	*) die "not routed: $response" ;;
	esac
	i=$((i + 1))
done
