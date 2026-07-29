#!/bin/sh

set -eu

script_dir="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
server_dir="$(CDPATH= cd -- "$script_dir/../.." && pwd)"
entrypoint="$server_dir/docker-entrypoint.sh"
test_dir="$(mktemp -d)"

cleanup() {
	rm -f "$test_dir/.env" "$test_dir/invalid.env"
	rmdir "$test_dir"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

assert_equal() {
	actual="$1"
	expected="$2"
	message="$3"
	[ "$actual" = "$expected" ] || fail "$message: got '$actual', want '$expected'"
}

printf '%s\n' \
	'DOTENV_VALUE=from-file' \
	'PRECEDENCE_VALUE=from-file' \
	'EMPTY_VALUE=' >"$test_dir/.env"

loaded="$(
	COSTRICT_ENV_FILE="$test_dir/.env" \
		PRECEDENCE_VALUE=from-container \
		sh "$entrypoint" sh -c \
		'printf "%s|%s|%s" "$DOTENV_VALUE" "$PRECEDENCE_VALUE" "${EMPTY_VALUE+x}"'
)"
assert_equal "$loaded" "from-file|from-file|x" \
	".env values should be exported and override container values"

missing="$(
	COSTRICT_ENV_FILE="$test_dir/missing.env" \
		UNCHANGED_VALUE=from-container \
		sh "$entrypoint" sh -c 'printf "%s" "$UNCHANGED_VALUE"'
)"
assert_equal "$missing" "from-container" \
	"a missing .env should leave the container environment unchanged"

printf '%s\n' 'INVALID ENV LINE' >"$test_dir/invalid.env"
set +e
COSTRICT_ENV_FILE="$test_dir/invalid.env" \
	sh "$entrypoint" sh -c 'exit 0' >/dev/null 2>&1
status=$?
set -e
[ "$status" -ne 0 ] || fail "an invalid .env should prevent the command from starting"

set +e
COSTRICT_ENV_FILE="$test_dir/.env" sh "$entrypoint" sh -c 'exit 23'
status=$?
set -e
assert_equal "$status" "23" "entrypoint should preserve the child exit status"

for dockerfile in Dockerfile Dockerfile.worker Dockerfile.channel-worker; do
	grep -F 'COPY --chmod=755 docker-entrypoint.sh /usr/local/bin/costrict-entrypoint' \
		"$server_dir/$dockerfile" >/dev/null ||
		fail "$dockerfile does not copy the shared entrypoint"
	grep -F 'ENTRYPOINT ["/usr/local/bin/costrict-entrypoint"]' \
		"$server_dir/$dockerfile" >/dev/null ||
		fail "$dockerfile does not use the shared entrypoint"
done

echo "docker entrypoint tests passed"
