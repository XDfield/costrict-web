# 非文本制品存储部署：Local / S3

本文档说明 costrict-web 的非文本 asset/artifact 存储如何在部署时选择
`local` 或 `s3`。存储接口、数据映射和最小 S3 能力边界的设计依据见
[Local / 受限 S3 非文本存储适配（二期）](../proposals/RESTRICTED_S3_OBJECT_STORAGE_DESIGN.md)。

## 1. 模式与约束

每套部署必须选择且只选择一种模式：

| 模式 | 适用环境 | 持久化要求 |
|---|---|---|
| `local` | 有持久化文件系统的环境 | API 和 worker 共享同一个 RWX PVC |
| `s3` | 提供对象存储 endpoint 的环境 | 不创建、不挂载 artifacts PVC |

选择由 API 和 worker Chart 中相同的配置决定：

```yaml
artifactStorage:
  backend: local # 或 s3
```

对应进程环境变量为：

```text
ARTIFACT_STORAGE_BACKEND=local
# 或
ARTIFACT_STORAGE_BACKEND=s3
```

`local` 和 `s3` 不是同一套部署内的读写分流规则。每条非文本对象记录都会保存
对象写入时使用的 `storage_backend` 和完整 `storage_key`；这不仅包括 asset 和
artifact，也包括 `capability_import_jobs` 中 `source_kind=upload` 的 bundle。
运行时 backend 与记录不匹配时，服务会拒绝读取。切换已有环境前必须先迁移
存量对象和数据库映射。

无论选择哪种模式：

- UTF-8 文本 asset 保存在 PostgreSQL。
- 非文本对象只使用精确 `Put` 和 `Get`。
- PostgreSQL 维护全部对象的 `storage_backend + storage_key` 映射。
- `source_kind=url` 的 admin import job 不产生存储对象，backend/key 均为空；
  `source_kind=upload` 则必须同时保存 backend 和 key。
- 应用不调用 List、Head、Delete、Presign、Multipart 或 CRC32 checksum。
- CSC 只访问 costrict-web 的 manifest/download API，不接触底层存储凭据。

Memory 不属于本期映射改造：

- memory 写路径当前是 stub，尚未接入本期 Local/S3 写入链路。
- `memory_versions` 不在本期 `storage_backend + storage_key` 映射和迁移范围内。
- local -> S3 切换前若 `memory_versions` 非空，必须阻断切换并先设计、执行独立
  memory 迁移；禁止忽略这些记录或假定当前 S3 backend 可以读取其历史 key。

## 2. Local 模式

### 2.1 API values

```yaml
artifactStorage:
  backend: local

persistence:
  enabled: true
  accessModes:
    - ReadWriteMany
  size: 10Gi
  existingClaim: ""
  storageClass: ""
  mountPath: /app/data/artifacts
```

API Chart 在 `persistence.enabled=true` 时挂载 artifacts PVC。若
`persistence.existingClaim` 为空，Chart 创建 `<api-release>-artifacts` PVC；
也可以通过 `existingClaim` 使用已有 RWX PVC。

`persistence.enabled=false` 不提供 artifacts 持久卷，只能用于开发、测试或其他
明确接受数据丢失的临时环境。容器重启、Pod 重建或迁移节点后，本地非文本对象
会丢失；生产 local 模式不得使用该配置。

### 2.2 Worker values

```yaml
artifactStorage:
  backend: local
  local:
    existingClaim: api-artifacts
    mountPath: /app/data/artifacts
```

worker 会处理 catalog 中的非文本文件，因此必须与 API 使用同一个 claim，并且
`mountPath` 必须一致。`api-artifacts` 只适用于 API Helm release 名为 `api` 且
API 未指定其他 `persistence.existingClaim` 的默认情况；否则应显式改成实际
claim 名称。

Local 模式不适用于没有 RWX 持久卷、API 与 worker 不能共享目录的环境。

### 2.3 Worker 安装与升级

`artifactStorage.local.existingClaim=api-artifacts` 只是“API Helm release 名为
`api` 且 API 未覆盖 `persistence.existingClaim`”时的默认约定。worker Chart
不会自动发现 API 实际使用的 PVC。

每次安装或升级 local worker 前，必须先确认：

1. API 实际 RWX claim。API 配置了 `persistence.existingClaim` 时使用该值；
   否则使用 `<api-release>-artifacts`。
2. API 的 `persistence.mountPath`。
3. claim 已存在于 worker 所在 namespace，并支持 API 与 worker 同时挂载。

升级时显式传入实际值，不依赖 worker 默认值：

```bash
helm upgrade --install worker deploy/charts/worker \
  --namespace costrict \
  --set artifactStorage.backend=local \
  --set artifactStorage.local.existingClaim=<api-actual-rwx-claim> \
  --set artifactStorage.local.mountPath=<api-actual-mount-path>
```

claim 名错误或 namespace 中不存在该 claim 时，worker Pod 会因 PVC 无法绑定而
保持 `Pending`。mountPath 与 API 不一致时，即使 Pod 能启动，API 和 worker 也
不会通过相同文件路径访问对象。

## 3. S3 模式

### 3.1 凭据和可选 CA

先在 API、worker 所在 namespace 创建凭据 Secret：

```bash
kubectl -n costrict create secret generic costrict-s3 \
  --from-literal=access-key='<access-key>' \
  --from-literal=secret-key='<secret-key>'
```

临时凭据还需在同一个 Secret 中增加 session token，并配置
`sessionTokenSecretKey`。

如果 endpoint 使用内部 CA，再创建 CA Secret：

```bash
kubectl -n costrict create secret generic costrict-s3-ca \
  --from-file=ca.crt=/path/to/ca.crt
```

不要把 access key、secret key、session token 或 Authorization header 写入
values、日志和文档。

### 3.2 API 和 worker values

API 与 worker 必须使用相同的 endpoint、bucket、region、path-style、凭据和
CA 配置：

```yaml
artifactStorage:
  backend: s3
  s3:
    endpoint: https://object-storage.example.internal
    bucket: costrict-artifacts
    region: internal
    forcePathStyle: true
    existingSecret: costrict-s3
    accessKeySecretKey: access-key
    secretKeySecretKey: secret-key
    sessionTokenSecretKey: ""
    ca:
      existingSecret: costrict-s3-ca
      key: ca.crt
      mountPath: /etc/costrict/object-storage
```

使用公网可信 CA 时，将 `ca.existingSecret` 留空。实际 endpoint 是否需要
`forcePathStyle` 由对象存储服务决定。

S3 模式下：

- API Chart 不渲染 artifacts PVC 或 StorageClass。
- API 和 worker 不挂载 artifacts volume。
- 可选 CA Secret 仍会作为只读 volume 挂载，这不是 artifacts 数据卷。
- Helm 自动注入 `AWS_REQUEST_CHECKSUM_CALCULATION=when_required` 和
  `AWS_RESPONSE_CHECKSUM_VALIDATION=when_required`。
- endpoint、bucket、region 或凭据 Secret 缺失时，Helm 渲染失败。
- DB 记录的 `storage_key` 非空但 `storage_backend` 为空时拒绝读取；空 backend
  不会被解释为当前 S3 backend。

应用账号的目标权限只有：

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "s3:GetObject",
        "s3:PutObject"
      ],
      "Resource": "arn:aws:s3:::costrict-artifacts/*"
    }
  ]
}
```

不要授予应用 ListBucket、DeleteObject 或 multipart 权限。bucket 创建、账号和
策略配置由对象存储管理员在部署前完成。

## 4. 部署前验证

### 4.1 Helm 渲染

分别检查 API 和 worker values：

```bash
helm lint deploy/charts/api -f /path/to/api-values.yaml
helm lint deploy/charts/worker -f /path/to/worker-values.yaml

helm template api deploy/charts/api -f /path/to/api-values.yaml >/tmp/api.yaml
helm template worker deploy/charts/worker -f /path/to/worker-values.yaml >/tmp/worker.yaml
```

S3 模式的渲染结果中不应出现 artifacts PVC、artifacts claim 或 artifacts 数据卷。

### 4.2 真实 MinIO 自动化 E2E

仓库提供使用真实 MinIO server 的自动化 E2E：

```bash
bash server/test/storage-e2e/run.sh
```

前置条件为 Docker 和 Go。runner 会：

1. 启动固定版本的 MinIO。
2. 创建 bucket 和仅有 `GetObject`/`PutObject` 权限的应用账号。
3. 验证该账号执行 ListBucket 得到 `AccessDenied`。
4. 通过生产 S3 backend 写入真实对象。
5. 从服务端生成 DB manifest，并通过下载 handler 读回对象。
6. 校验字节、Content-Length 和 SHA-256。
7. 拒绝 CRC32、chunked、HEAD、DELETE、List 和 multipart 请求。

该测试已验证“真实 S3 协议的服务端 Put/Get + manifest/download”，handler
使用 SQLite 测试 DB；它没有启动真实 PostgreSQL/Kubernetes 或 CSC，也没有
执行真实的 distribution -> CSC subscription/install。

截至 2026-07-23，本地 runner 已通过，尚无远端 GitHub Actions run 记录；是否
CI 通过以对应 run 为准。

### 4.3 目标环境 endpoint smoke

MinIO E2E 不能替代目标环境 endpoint 验收。上线前使用实际 endpoint、bucket、
region、path-style、CA 和应用凭据完成：

1. 精确 PutObject 后按同一 key GetObject。
2. 确认 SDK 固定 query `x-id=PutObject` / `x-id=GetObject` 可被接受。
3. 确认请求不需要 CRC32，且应用不依赖 List、Head、Delete 或 multipart。
4. 使用实际 PostgreSQL 验证 manifest 和非文本文件下载。
5. 创建一个 admin import upload job，确认 DB 同时写入
   `storage_backend=s3` 和非空 `storage_key`，并由 runner 成功读回 bundle。
6. 确认 S3 模式会拒绝 backend 为空但 key 非空的历史对象记录。
7. 执行一次真实 costrict-web distribution -> CSC subscription/install，校验
   完整文件树、size、SHA-256 和原子安装。
8. 重启 API/worker Pod 后再次下载同一对象。

截至 2026-07-23，目标环境 endpoint smoke 和上述真实 CSC 全链路尚未执行。

## 5. 模式切换与迁移

新环境可以直接选择 `local` 或 `s3`。已有环境不能只修改
`artifactStorage.backend`：

1. 进入维护窗口，停止新的 asset/artifact/admin import upload 写入，并确保
   admin import runner 不再领取新任务，避免枚举和切换期间产生新的 local 对象。
2. 检查 `memory_versions`；只要存在记录就阻断本次切换，先完成独立 memory
   迁移方案和验证。
3. 从 PostgreSQL 枚举所有对象映射，包括 `capability_assets`、
   `capability_artifacts` 和 `capability_import_jobs` 的 upload bundle；不扫描
   S3 或本地目录。
4. 将 `storage_key` 非空但 `storage_backend` 为空的历史记录列为阻断项，不允许
   依赖 S3 模式隐式回退。
5. 按 DB 中的 key 从 LocalBackend Get。
6. 向 S3Backend Put。
7. 对源流和目标流计算 SHA-256 并比对；记录已有 size/摘要时还要与 DB 值校验。
8. 仅在对象写入并校验成功后，原子更新 DB 的 backend/key 映射。
9. 检查 admin import upload job。对于 `pending`、`running`、`previewed` 等
   未完成任务，必须迁移 bundle 和映射，或者先在 local 模式完成/取消任务，
   确保它们不再被调度。
10. 确认不存在未迁移、backend 为空或仍会读取 local bundle 的任务，再切换 API
   和 worker。
11. 切换后恢复写入，并抽样验证 asset、artifact 和 admin import bundle 的 Get。

当前迁移工具尚未实现。存在 local 存量对象、backend 为空的历史记录，或未完成
且未迁移/取消的 upload job 时，不得直接切换生产环境；`memory_versions` 非空
同样是阻断条件。

## 6. 回滚

- 只修改 Helm 配置且尚未写入新 backend 时，可以回滚原 values 并重启
  API/worker。
- 一旦 DB 中出现 `storage_backend=s3` 记录，回滚版本必须仍具备 S3 Get 能力，
  否则这些对象不可读。
- 已完成 local -> s3 映射切换后，如需回到 local，必须执行反向对象迁移和 DB
  映射更新，不能只改 `artifactStorage.backend`。
- 当前没有应用侧 Delete。回滚不会清理已写入的 S3 对象，底层对象保留策略由
  bucket 生命周期或运维流程负责。
