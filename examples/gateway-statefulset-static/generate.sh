#!/usr/bin/env bash
# 用 Helm 重新生成 nginx-router-configmap.yaml（static 发现模式，自动生成 Pod FQDN）。
# 用法：
#   ./generate.sh <RELEASE_NAME> <NAMESPACE> <REPLICAS> [CLUSTER_DNS]
#
# 示例：
#   ./generate.sh costrict-web-gateway costrict 2
#
# REPLICAS 是 Gateway StatefulSet 的副本数；生成的 ConfigMap 会按副本数自动
# 生成 Pod FQDN（<release>-0/1/...）。CLUSTER_DNS 留空时，nginx-router 会从
# Pod 的 /etc/resolv.conf 自动探测 DNS；仅在 hostNetwork、自定义 dnsConfig
# 等非常规场景需要显式指定。

set -euo pipefail

RELEASE_NAME="${1:-costrict-web-gateway}"
NAMESPACE="${2:-default}"
REPLICAS="${3:-}"
CLUSTER_DNS="${4:-}"

if [ -z "$REPLICAS" ]; then
    echo "ERROR: REPLICAS is required" >&2
    echo "Usage: $0 <RELEASE_NAME> <NAMESPACE> <REPLICAS> [CLUSTER_DNS]" >&2
    exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="${SCRIPT_DIR}/../../deploy/charts/gateway"
OUT_FILE="${SCRIPT_DIR}/nginx-router-configmap.yaml"
TMP_PY=$(mktemp)
trap 'rm -f "$TMP_PY"' EXIT

cat > "$TMP_PY" <<'PY'
import sys, yaml

release_name, namespace, replicas, cluster_dns = sys.argv[1:5]

docs = list(yaml.safe_load_all(sys.stdin))
for doc in docs:
    if doc and doc.get('kind') == 'ConfigMap' and doc.get('metadata', {}).get('name') == f'{release_name}-nginx-router-config':
        header = f'''# AUTO-GENERATED from deploy/charts/gateway (static discovery mode).
# You can regenerate this file with:
#   ./generate.sh {release_name} {namespace} \\
'''
        if cluster_dns:
            header += f'#     "{cluster_dns}"\n'
        else:
            header += '#     (omit CLUSTER_DNS to auto-detect from /etc/resolv.conf)\n'
        header += '''#
# The ConfigMap auto-generates StatefulSet Pod FQDNs from the replica count
# below ("local gateway_replicas"); keep it in sync when you scale the
# StatefulSet, then restart nginx-router.
'''
        lua_header = header.replace('# ', '-- ').replace('#\n', '--\n')
        conf = doc['data']['nginx.conf']
        lua = doc['data']['router.lua']
        dns_utils = doc['data']['dns_utils.lua']
        if cluster_dns:
            conf = conf.replace(cluster_dns, '{{CLUSTER_DNS}}')
        else:
            conf = conf.replace('local resolver_override = ""', 'local resolver_override = ""  -- set {{CLUSTER_DNS}} to override auto-detection')
        conf = conf.replace(f'local pod_name_prefix = "{release_name}"', 'local pod_name_prefix = "{{RELEASE_NAME}}"')
        conf = conf.replace(f'local headless_base = "{release_name}-headless.{namespace}.svc"', 'local headless_base = "{{RELEASE_NAME}}-headless.{{NAMESPACE}}.svc"')
        conf = conf.replace(f'local gateway_replicas = {replicas}', 'local gateway_replicas = {{REPLICAS}}')
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
  --set "replicaCount=${REPLICAS}"
)
if [ -n "${CLUSTER_DNS}" ]; then
  HELM_ARGS+=(--set "nginxRouter.resolver=${CLUSTER_DNS}")
fi

# Generate and post-process in one shot. We keep only the nginx-router ConfigMap,
# then replace rendered values with obvious placeholders.
helm template "${RELEASE_NAME}" "${CHART_DIR}" "${HELM_ARGS[@]}" \
  | python3 "$TMP_PY" "${RELEASE_NAME}" "${NAMESPACE}" "${REPLICAS}" "${CLUSTER_DNS}" > "${OUT_FILE}"

echo "Generated: ${OUT_FILE}"
echo "Please review and replace remaining placeholders before applying."
