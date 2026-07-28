# API 服务支持 ConfigMap 挂载为 .env 设计

日期：2026-07-28
状态：已确认

## 背景与目标

API 服务（`server/cmd/api`）的配置由 `server/internal/config/config.go` 的 `Load()` 读取：viper 加载工作目录下的 `.env` 文件（`AddConfigPath(".")`），配合 `AutomaticEnv()`，实际优先级为 **环境变量 > `.env` 文件 > 默认值**。

目标：在 Kubernetes 部署中支持将 ConfigMap 挂载为容器工作目录下的 `.env` 文件，使配置可以通过 ConfigMap 下发；ConfigMap 不存在（未配置挂载）时行为完全不变。优先级维持现状不变（环境变量 > `.env` 文件 > 默认值）。

## 关键结论

- API 镜像 `WORKDIR /app`（`server/Dockerfile:42`），代码读 `./.env` 即 `/app/.env`。
- **Go 代码零改动**：现有 viper 逻辑已满足语义，ConfigMap 挂载为 `/app/.env` 后自动生效。
- 改动仅限 Helm chart：`deploy/charts/api`。

## 变更内容

### 1. `deploy/charts/api/values.yaml` 新增配置块

```yaml
# 可选：将已有的 ConfigMap 挂载为容器工作目录下的 .env 文件。
# 优先级保持：环境变量 > .env 文件 > 默认值（与代码现状一致）。
envFile:
  existingConfigMap: ""   # ConfigMap 名称，为空则不挂载（默认行为不变）
  key: .env               # ConfigMap 中包含 env 内容的 key
```

- 只支持引用已有 ConfigMap（`existingConfigMap` 模式与 chart 内 `database.existingSecret`、`redis.existingSecret` 风格一致），chart 不负责创建 ConfigMap。

### 2. `deploy/charts/api/templates/deployment.yaml` 模板改动

- `volumeMounts` 追加（当 `envFile.existingConfigMap` 非空时）：

  ```yaml
  - name: app-env-file
    mountPath: /app/.env
    subPath: .env
    readOnly: true
  ```

  使用 `subPath` 挂载单个文件，避免覆盖整个 `/app` 目录。

- `volumes` 追加：

  ```yaml
  - name: app-env-file
    configMap:
      name: {{ .Values.envFile.existingConfigMap }}
      items:
        - key: {{ .Values.envFile.key }}
          path: .env
  ```

- 挂载判断条件并入现有 `volumeMounts`/`volumes` 外层 `if`（`deployment.yaml` 中 artifacts/logs/CA 的同一组条件）。

### 3. 文档/注释

- 在 values.yaml 的 `envFile` 块注释中说明：
  - `subPath` 挂载不会随 ConfigMap 更新自动刷新文件内容，更新 ConfigMap 后需重启 Pod 生效。
  - 同名 key 同时存在于环境变量和 `.env` 时，环境变量胜出。
- 附上使用示例：

  ```bash
  kubectl create configmap costrict-api-env --from-file=.env=./prod.env
  helm install api ./deploy/charts/api --set envFile.existingConfigMap=costrict-api-env
  ```

## 测试

- `helm template` 验证：
  - 默认（`envFile.existingConfigMap` 为空）：不渲染 `app-env-file` 的 volume/volumeMount，输出与现状一致。
  - 设置 `envFile.existingConfigMap=costrict-api-env`：渲染出正确的 volumeMount（`mountPath: /app/.env`、`subPath: .env`、`readOnly: true`）和 configMap volume（`items` 映射 `key` → `.env`）。
  - 自定义 `envFile.key` 时 volume 的 `items.key` 正确变化。
- 行为回归：现有「环境变量覆盖 .env 文件值」语义不变（代码无改动，无需新增单测）。

## 范围外（YAGNI）

- 不由 chart 创建 ConfigMap 资源。
- 不引入额外的配置文件路径或 K8s API 读取方式。
- 不处理 ConfigMap 热更新（subPath 限制下需重启 Pod）。
