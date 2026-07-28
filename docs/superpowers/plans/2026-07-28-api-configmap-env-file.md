# API Chart 支持 ConfigMap 挂载为 .env 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为 `deploy/charts/api` Helm chart 增加 `envFile.existingConfigMap` 支持，将已有 ConfigMap 以 subPath 方式挂载为 `/app/.env`，使 API 服务可以通过 ConfigMap 下发配置。

**Architecture:** Go 代码零改动（`server/internal/config/config.go` 已读工作目录 `.env`，镜像 `WORKDIR /app`，优先级环境变量 > .env > 默认值）。改动仅限 Helm chart：values 新增 `envFile` 块，deployment 模板条件渲染 volume/volumeMount，新增渲染测试脚本并接入 CI。

**Tech Stack:** Helm 3 chart 模板、bash 渲染断言测试（沿用 `tests/storage_mode_test.sh` 模式）、GitHub Actions。

**Spec:** `docs/superpowers/specs/2026-07-28-api-configmap-env-file-design.md`

## Global Constraints

- 配置优先级必须保持：环境变量 > `.env` 文件 > 默认值（不改 Go 代码）。
- 默认行为零变化：`envFile.existingConfigMap` 为空时渲染结果与现状逐字节一致。
- 只支持引用已有 ConfigMap，chart 不创建 ConfigMap 资源。
- 挂载必须用 `subPath` 挂单个文件，不得覆盖整个 `/app`。
- 遵循 chart 内现有命名惯例（`existingConfigMap` 对齐 `database.existingSecret` 风格）。
- 不直接提交到 main；在 `feat/api-configmap-env-file` 分支上提交。

---

### Task 1: envFile values + deployment 模板 + 渲染测试 + CI 接入

**Files:**
- Modify: `deploy/charts/api/values.yaml`（在 `env: []` / `envFrom: []` 附近追加 `envFile` 块）
- Modify: `deploy/charts/api/templates/deployment.yaml:162`（volumeMounts 外层 if 条件及内部追加挂载）
- Modify: `deploy/charts/api/templates/deployment.yaml:178`（volumes 外层 if 条件及内部追加 volume）
- Create: `deploy/charts/api/tests/env_file_test.sh`
- Modify: `.github/workflows/lint-charts.yaml:48-50`（api 矩阵下追加测试步骤）

**Interfaces:**
- Consumes: 现有 values `envFile.existingConfigMap`（string，默认 `""`）、`envFile.key`（string，默认 `.env`）。
- Produces: 渲染出的 Deployment 中 volumeMount `name: app-env-file, mountPath: /app/.env, subPath: .env, readOnly: true` 与对应 configMap volume（`items: [{key: <envFile.key>, path: .env}]`）。

- [ ] **Step 1: 编写失败的渲染测试**

创建 `deploy/charts/api/tests/env_file_test.sh`（沿用 storage_mode_test.sh 的断言风格）：

```bash
#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
CHART_DIR="$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)"
TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT

assert_contains() {
  local file="$1"
  local text="$2"
  grep -Fq -- "$text" "$file" || {
    echo "expected rendered manifest to contain: $text" >&2
    exit 1
  }
}

assert_not_contains() {
  local file="$1"
  local text="$2"
  if grep -Fq -- "$text" "$file"; then
    echo "expected rendered manifest not to contain: $text" >&2
    exit 1
  fi
}

# Default: no envFile configured, nothing extra is rendered.
helm template default "$CHART_DIR" > "$TEST_DIR/default.yaml"
assert_not_contains "$TEST_DIR/default.yaml" "name: app-env-file"
assert_not_contains "$TEST_DIR/default.yaml" "mountPath: /app/.env"

# ConfigMap mounted as /app/.env via subPath.
helm template with-env-file "$CHART_DIR" \
  --set envFile.existingConfigMap=costrict-api-env > "$TEST_DIR/with-env-file.yaml"
assert_contains "$TEST_DIR/with-env-file.yaml" "name: app-env-file"
assert_contains "$TEST_DIR/with-env-file.yaml" "mountPath: /app/.env"
assert_contains "$TEST_DIR/with-env-file.yaml" "subPath: .env"
assert_contains "$TEST_DIR/with-env-file.yaml" "readOnly: true"
assert_contains "$TEST_DIR/with-env-file.yaml" "name: costrict-api-env"
assert_contains "$TEST_DIR/with-env-file.yaml" "key: .env"
assert_contains "$TEST_DIR/with-env-file.yaml" "path: .env"

# Custom ConfigMap key is honoured.
helm template custom-key "$CHART_DIR" \
  --set envFile.existingConfigMap=costrict-api-env \
  --set envFile.key=custom.env > "$TEST_DIR/custom-key.yaml"
assert_contains "$TEST_DIR/custom-key.yaml" "key: custom.env"
assert_contains "$TEST_DIR/custom-key.yaml" "path: .env"

echo "api env file render tests passed"
```

- [ ] **Step 2: 运行测试确认失败**

Run: `bash deploy/charts/api/tests/env_file_test.sh`
Expected: FAIL —— `with-env-file` 用例报 `expected rendered manifest to contain: name: app-env-file`（values 与模板尚未实现）。

- [ ] **Step 3: values.yaml 追加 envFile 块**

在 `deploy/charts/api/values.yaml` 的 `envFrom: []` 之后追加：

```yaml
# Optional: mount an existing ConfigMap as the .env file in the container
# working directory (/app/.env). The API server loads it via viper; precedence
# stays: environment variables > .env file > defaults.
# Note: subPath mounts do not receive ConfigMap updates automatically;
# restart the pod after updating the ConfigMap.
# Example:
#   kubectl create configmap costrict-api-env --from-file=.env=./prod.env
#   helm install api ./deploy/charts/api --set envFile.existingConfigMap=costrict-api-env
envFile:
  # Name of an existing ConfigMap containing the env content. Empty disables
  # the mount (default behaviour unchanged).
  existingConfigMap: ""
  # Key within the ConfigMap that holds the .env content.
  key: .env
```

- [ ] **Step 4: deployment.yaml 模板追加挂载**

修改 `deploy/charts/api/templates/deployment.yaml`。

(a) volumeMounts 外层条件（现 `:162`）改为：

```
          {{- if or (and (eq $artifactBackend "local") .Values.persistence.enabled) .Values.logs.enabled (and (eq $artifactBackend "s3") .Values.artifactStorage.s3.ca.existingSecret) .Values.envFile.existingConfigMap }}
```

并在该 `volumeMounts:` 块内、`artifact-storage-ca` 挂载之后追加：

```
            {{- if .Values.envFile.existingConfigMap }}
            - name: app-env-file
              mountPath: /app/.env
              subPath: .env
              readOnly: true
            {{- end }}
```

(b) volumes 外层条件（现 `:178`）改为：

```
      {{- if or (and (eq $artifactBackend "local") .Values.persistence.enabled) .Values.logs.enabled (and (eq $artifactBackend "s3") .Values.artifactStorage.s3.ca.existingSecret) .Values.envFile.existingConfigMap }}
```

并在该 `volumes:` 块内、`artifact-storage-ca` volume 之后追加：

```
        {{- if .Values.envFile.existingConfigMap }}
        - name: app-env-file
          configMap:
            name: {{ .Values.envFile.existingConfigMap }}
            items:
              - key: {{ .Values.envFile.key }}
                path: .env
        {{- end }}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `bash deploy/charts/api/tests/env_file_test.sh`
Expected: PASS —— 输出 `api env file render tests passed`

同时回归既有测试，确认默认渲染零变化：

Run: `bash deploy/charts/api/tests/storage_mode_test.sh`
Expected: PASS —— 输出 `api storage mode render tests passed`

- [ ] **Step 6: CI 接入新测试脚本**

修改 `.github/workflows/lint-charts.yaml`，在 `Validate API storage modes` 步骤（`:48-50`）之后追加：

```yaml
      - name: Validate API env file mount
        if: matrix.chart == 'api'
        run: bash deploy/charts/api/tests/env_file_test.sh
```

- [ ] **Step 7: 提交**

```bash
git add deploy/charts/api/values.yaml deploy/charts/api/templates/deployment.yaml deploy/charts/api/tests/env_file_test.sh .github/workflows/lint-charts.yaml
git commit -m "feat(charts/api): support mounting a ConfigMap as /app/.env

Co-Authored-By: Claude <noreply@anthropic.com>"
```
