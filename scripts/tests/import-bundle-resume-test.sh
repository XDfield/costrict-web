#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
TMP="$(mktemp -d -t costrict-import-resume-test.XXXXXX)"
trap 'rm -rf "$TMP"' EXIT INT TERM

mkdir -p "$TMP/bin" "$TMP/state" "$TMP/plugins"
cat >"$TMP/bin/curl" <<'FAKE_CURL'
#!/usr/bin/env bash
url=""
for arg in "$@"; do
    case "$arg" in http://* | https://*) url="$arg" ;; esac
done
case "$url" in
*/repos/mirror-owner/demo/branches/main)
    printf '{"name":"main","commit":{"id":"%s"}}\n200' "$FAKE_REMOTE_SHA"
    ;;
*/repos/mirror-owner/demo/contents/*)
    printf '{"encoding":"base64","content":"%s"}\n200' "$FAKE_MANIFEST_B64"
    ;;
*/repos/mirror-owner/demo)
    printf '{"full_name":"mirror-owner/demo","default_branch":"main","empty":false}\n200'
    ;;
*) printf '{"message":"not found"}\n404' ;;
esac
FAKE_CURL
chmod +x "$TMP/bin/curl"

mkdir "$TMP/work"
git -C "$TMP/work" init -q
git -C "$TMP/work" config user.name test
git -C "$TMP/work" config user.email test@example.invalid
printf '{"name":"demo-plugin","version":"1.0.0"}\n' >"$TMP/work/.plugin.json"
git -C "$TMP/work" add .plugin.json
git -C "$TMP/work" commit -qm initial
git clone -q --bare "$TMP/work" "$TMP/plugins/demo.git"

printf 'demo\tMATCH\t.plugin.json\tdemo-plugin\t1.0.0\t\n' >"$TMP/state/plan.tsv"
export PATH="$TMP/bin:$PATH"
export STATE_DIR="$TMP/state"
export PLUGINS_DIR="$TMP/plugins"
export GITEA_URL="http://gitea.test"
export GITEA_OWNER="mirror-owner"
export FAKE_REMOTE_SHA="$(git -C "$TMP/plugins/demo.git" rev-parse HEAD)"
export FAKE_MANIFEST_B64="$(printf '{"name":"demo-plugin","version":"1.0.0"}\n' | base64 | tr -d '\n')"

"$ROOT/scripts/import-bundle-to-gitea.sh" __verify-resume-candidate demo

export FAKE_REMOTE_SHA="0000000000000000000000000000000000000000"
if "$ROOT/scripts/import-bundle-to-gitea.sh" __verify-resume-candidate demo; then
    echo "stale remote HEAD was accepted" >&2
    exit 1
fi

echo "import resume verification: ok"
