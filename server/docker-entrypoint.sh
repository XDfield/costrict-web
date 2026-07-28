#!/bin/sh

set -eu

env_file="${COSTRICT_ENV_FILE:-/app/.env}"

if [ -f "$env_file" ]; then
	echo "Loading environment from $env_file" >&2
	set -a
	. "$env_file"
	set +a
fi

if [ "$#" -eq 0 ]; then
	echo "No command provided to container entrypoint" >&2
	exit 64
fi

exec "$@"
