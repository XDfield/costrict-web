#!/usr/bin/env bash
# 用 Helm 重新生成 nginx-router-configmap.yaml（static 发现模式）。
# 用法：
#   ./generate.sh <RELEASE_NAME> <NAMESPACE> <STATIC_FQDNS_CSV> [CLUSTER_DNS]
#
# 示例：
#   ./generate.sh costrict-web-gateway costrict \
#     "costrict-web-gateway-0.costrict-web-gateway-headless.costrict.svc.cluster.local,costrict-web-gateway-1.costrict-web-gateway-headless.costrict.svc.cluster.local"
#
# CLUSTER_DNS 留空时，nginx-router 会从 Pod 的 /etc/resolv.conf 自动探测 DNS；
# 仅在 hostNetwork、自定义 dnsConfig 等非常规场景需要显式指定。

set -euo pipefail

RELEASE_NAME="${1:-costrict-web-gateway}"
NAMESPACE="${2:-default}"
STATIC_FQDNS_CSV="${3:-}"
CLUSTER_DNS="${4:-}"

if [ -z "$STATIC_FQDNS_CSV" ]; then
    echo "ERROR: STATIC_FQDNS_CSV is required" >&2
    echo "Usage: $0 <RELEASE_NAME> <NAMESPACE> <STATIC_FQDNS_CSV> [CLUSTER_DNS]" >&2
    exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="${SCRIPT_DIR}/../../deploy/charts/gateway"
OUT_FILE="${SCRIPT_DIR}/nginx-router-configmap.yaml"
TMP_PY=$(mktemp)
trap 'rm -f "$TMP_PY"' EXIT

cat > "$TMP_PY" <<'PY'
import sys, yaml, re

release_name, namespace, fqdns_csv, cluster_dns = sys.argv[1:5]

docs = list(yaml.safe_load_all(sys.stdin))
for doc in docs:
    if doc and doc.get('kind') == 'ConfigMap' and doc.get('metadata', {}).get('name') == f'{release_name}-nginx-router-config':
        header = f'''# AUTO-GENERATED from deploy/charts/gateway (static discovery mode).
# You can regenerate this file with:
#   ./generate.sh {release_name} {namespace} \\
#     "{fqdns_csv}" \\
'''
        if cluster_dns:
            header += f'#     "{cluster_dns}"\n'
        else:
            header += '#     (omit CLUSTER_DNS to auto-detect from /etc/resolv.conf)\n'
        header += '''#
# CRITICAL: replace the FQDNs in the static_fqdns Lua table below with the
# actual StatefulSet Pod FQDNs in your cluster. Example pattern:
#   {release}-0.{release}-headless.{namespace}.svc.{cluster-domain}
'''
        lua_header = f'''-- AUTO-GENERATED from deploy/charts/gateway (static discovery mode).
-- You can regenerate this file with:
--   ./generate.sh {release_name} {namespace} \\
--     "{fqdns_csv}" \\
'''
        if cluster_dns:
            lua_header += f'--     "{cluster_dns}"\n'
        else:
            lua_header += '--     (omit CLUSTER_DNS to auto-detect from /etc/resolv.conf)\n'
        lua_header += '''--
-- CRITICAL: replace the FQDNs in the static_fqdns Lua table below with the
-- actual StatefulSet Pod FQDNs in your cluster. Example pattern:
--   {release}-0.{release}-headless.{namespace}.svc.{cluster-domain}
'''
        conf = doc['data']['nginx.conf']
        lua = doc['data']['router.lua']
        dns_utils = doc['data']['dns_utils.lua']
        if cluster_dns:
            conf = conf.replace(cluster_dns, '{{CLUSTER_DNS}}')
        else:
            # If auto-detect, leave resolver_override as "" and just add a placeholder comment.
            conf = conf.replace('local resolver_override = ""', 'local resolver_override = ""  -- set {{CLUSTER_DNS}} to override auto-detection')
        # Replace the auto-generated static_fqdns block with explicit placeholders.
        # The regex matches the whole Lua table, including multi-line string literals.
        conf = re.sub(
            r'local static_fqdns = \{[^}]+\}',
            '''local static_fqdns = {
                -- TODO: replace with your actual StatefulSet Pod FQDNs
                "{{RELEASE_NAME}}-0.{{RELEASE_NAME}}-headless.{{NAMESPACE}}.svc.{{CLUSTER_DOMAIN}}",
                "{{RELEASE_NAME}}-1.{{RELEASE_NAME}}-headless.{{NAMESPACE}}.svc.{{CLUSTER_DOMAIN}}",
            }''',
            conf,
            flags=re.DOTALL
        )
        doc['data']['nginx.conf'] = header + conf
        doc['data']['router.lua'] = lua_header + lua
        doc['data']['dns_utils.lua'] = lua_header + dns_utils
        doc['metadata']['name'] = '{{RELEASE_NAME}}-nginx-router-config'
        doc['metadata']['namespace'] = '{{NAMESPACE}}'
        print(yaml.dump(doc, sort_keys=False, allow_unicode=True))
        sys.exit(0)
print('ERROR: nginx-router ConfigMap not found in helm output', file=sys.stderr)
sys.exit(1)
PY

# Build helm args
HELM_ARGS=(
  --namespace "${NAMESPACE}"
  --set statefulSet.enabled=true
  --set nginxRouter.enabled=true
  --set nginxRouter.discovery.mode=static
  --set "nginxRouter.discovery.staticFQDNs={${STATIC_FQDNS_CSV}}"
)
if [ -n "${CLUSTER_DNS}" ]; then
  HELM_ARGS+=(--set "nginxRouter.resolver=${CLUSTER_DNS}")
fi

# Generate and post-process in one shot. We keep only the nginx-router ConfigMap,
# then replace rendered values with obvious placeholders.
helm template "${RELEASE_NAME}" "${CHART_DIR}" "${HELM_ARGS[@]}" \
  | python3 "$TMP_PY" "${RELEASE_NAME}" "${NAMESPACE}" "${STATIC_FQDNS_CSV}" "${CLUSTER_DNS}" > "${OUT_FILE}"

echo "Generated: ${OUT_FILE}"
echo "Please review and replace remaining placeholders before applying."
