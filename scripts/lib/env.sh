# shellcheck shell=bash
#
# Shared by the scripts in scripts/: read settings from the repo's .env, the
# same file compose.yaml reads (make creates it from .env.template).
#
# Usage, from a script in scripts/:
#   source "$(dirname "$0")/lib/env.sh"
#   load_env API_HOST_PORT RABBITMQ_USER ...
#
# Written for bash 3.2 (the macOS default).

# load_env reads the named variables from .env and exports them. It parses the
# file rather than sourcing it, so nothing in .env is executed. A variable
# already set in the environment wins, as it does for docker compose. It exits
# if .env is missing or leaves any of the variables unset.
load_env() {
	local env_file line key wanted var
	env_file="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/.env"
	if [ ! -f "$env_file" ]; then
		echo "$(basename "$0"): no $env_file: run make up first (or cp .env.template .env)" >&2
		exit 1
	fi

	while IFS= read -r line || [ -n "$line" ]; do
		case $line in '' | '#'*) continue ;; esac
		key=${line%%=*}
		wanted=false
		for var in "$@"; do [ "$key" = "$var" ] && wanted=true; done
		$wanted || continue
		[ -n "${!key+set}" ] && continue
		export "$key=${line#*=}"
	done <"$env_file"

	for var in "$@"; do
		if [ -z "${!var:-}" ]; then
			echo "$(basename "$0"): $var is not set in $env_file (see .env.template)" >&2
			exit 1
		fi
	done
}
