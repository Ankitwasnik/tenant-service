#!/usr/bin/env bash
#
# race-demo.sh: fire concurrent requests at the running stack (`make up`) and
# tally the results, to show the race conditions are handled:
#
#   1. N creates with the same slug        → exactly 1 × 201, the rest 409 tenant_already_exists
#   2. N PATCHes with the same version     → exactly 1 × 202, the rest 409 tenant_version_conflict
#   3. N DELETEs of the same tenant        → exactly 1 × 202, the rest 409 tenant_update_not_allowed
#
# Each batch goes out through curl --parallel, one connection per request, so
# they really are concurrent. Between batches it waits for the worker to finish
# the tenant's task, as a real client would. The automated version, with
# database checks, is `make race`.
#
# Usage: scripts/race-demo.sh [--count N]     (default 50)
#
# Needs bash (3.2 or later) and curl 7.66 or later (for --parallel).

set -euo pipefail

die() { echo "race-demo: $*" >&2; exit 1; }

count=50
while [ $# -gt 0 ]; do
	case $1 in
	--count) count=${2:?--count needs a value}; shift 2 ;;
	-h | --help) sed -n '3,/^# Needs/p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
	*) die "unknown option $1 (see --help)" ;;
	esac
done
case $count in '' | *[!0-9]* | 0 | 1) die "--count must be an integer of at least 2" ;; esac

# shellcheck source=lib/env.sh
source "$(dirname "$0")/lib/env.sh"
load_env API_HOST_PORT
API=${API_URL:-http://127.0.0.1:$API_HOST_PORT}

curl -fsS "$API/healthz" >/dev/null 2>&1 || die "the API at $API isn't up: run make up"
curl --help all 2>/dev/null | grep -q -- '--parallel-immediate' || die "curl is too old for --parallel (needs 7.66+)"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
failed=0

# race METHOD PATH BODY: send $count identical requests at once, and print one
# line per request: "<status> <error code, or - on success>".
race() {
	local method=$1 path=$2 body=$3 i
	local args=(--parallel --parallel-immediate --parallel-max "$count" -s -X "$method"
		-H 'Content-Type: application/json' -w '%{http_code} %{filename_effective}\n')
	[ -n "$body" ] && args+=(-d "$body")
	for ((i = 1; i <= count; i++)); do
		args+=(-o "$work/$i" "$API$path")
	done
	curl "${args[@]}" | while read -r status file; do
		printf '%s %s\n' "$status" "$(sed -nE 's/.*"code":"([a-z_]+)".*/\1/p' "$file" | grep . || echo -)"
	done
}

# report WANT_STATUS WANT_CODE: tally the race's results (stdin), and
# check there was exactly one WANT_STATUS and every other request got WANT_CODE.
report() {
	local want_status=$1 want_code=$2 results wins losers
	results=$(cat)
	echo "$results" | sort | uniq -c | sort -rn | sed 's/^/    /'
	wins=$(echo "$results" | grep -c "^$want_status " || true)
	losers=$(echo "$results" | grep -c "^409 $want_code$" || true)
	if [ "$wins" -eq 1 ] && [ "$losers" -eq $((count - 1)) ]; then
		echo "  PASS: exactly one of $count won; the other $((count - 1)) got $want_code"
	else
		echo "  FAIL: $wins won, $losers got $want_code (want 1 and $((count - 1)))"
		failed=1
	fi
}

field() { sed -nE "s/.*\"$1\":\"?([^\",}]+)\"?.*/\1/p" | head -1; }

# wait_status ID WANT: poll the tenant until the worker has moved it to WANT.
wait_status() {
	local id=$1 want=$2 status="" i
	for ((i = 0; i < 60; i++)); do
		status=$(curl -fsS "$API/v1/tenants/$id" | field status)
		[ "$status" = "$want" ] && return 0
		sleep 1
	done
	die "tenant $id is $status after 60s, want $want (is the worker running? make worker)"
}

slug="race-$(date +%s)"
echo "== 1. $count concurrent creates of slug $slug"
race POST /v1/tenants "{\"slug\":\"$slug\",\"name\":\"Race\"}" | report 201 tenant_already_exists

id=$(curl -fsS "$API/v1/tenants?limit=200" | tr '{' '\n' | grep "\"slug\":\"$slug\"" | field id)
[ -n "$id" ] || die "the created tenant wasn't found"
echo "  waiting for the worker to make it active..."
wait_status "$id" active
version=$(curl -fsS "$API/v1/tenants/$id" | field version)

echo
echo "== 2. $count concurrent PATCHes of tenant $id, all with version $version"
race PATCH "/v1/tenants/$id" "{\"name\":\"Race winner\",\"version\":$version}" | report 202 tenant_version_conflict
open=$(curl -fsS "$API/v1/tasks?tenant_id=$id" | grep -o '"status":"accepted"\|"status":"in_progress"' | wc -l | tr -d ' ')
echo "  open tasks for the tenant: $open (at most 1 allowed)"
[ "$open" -le 1 ] || { echo "  FAIL: more than one open task"; failed=1; }
echo "  waiting for the update to finish..."
wait_status "$id" active
echo "  version now $(curl -fsS "$API/v1/tenants/$id" | field version) (was $version: +1 for the one PATCH, +1 for its completion)"

echo
echo "== 3. $count concurrent DELETEs of tenant $id"
race DELETE "/v1/tenants/$id" "" | report 202 tenant_update_not_allowed

echo
if [ "$failed" -eq 0 ]; then
	echo "All races handled: every batch had exactly one winner."
else
	echo "Some races were not handled as expected (see FAIL above)."
	exit 1
fi
