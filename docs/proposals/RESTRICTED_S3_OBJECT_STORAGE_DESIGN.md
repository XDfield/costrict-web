# Local / 受限 S3 非文本存储适配（二期）

**Status**: Implemented; pending target endpoint smoke test and data migration tooling

**Date**: 2026-07-23

**Related PRs**:

- [costrict-web #185](https://github.com/XDfield/costrict-web/pull/185): 完整 Skill 文件树入库与分发
- [csc #454](https://github.com/zgsm-sangfor/csc/pull/454): 完整 Skill 文件树下载与原子安装

---

## 1. 背景

两个关联 PR 已完成完整 Skill 文件树链路：

- `SKILL.md` 和 UTF-8 文本 asset 存入 PostgreSQL。
- 非文本 asset 通过服务端存储抽象持久化。
- CSC 获取 asset manifest，逐文件下载并校验 SHA-256，最后原子替换本地 Skill 目录。

当前 costrict-web 仅实现 `LocalBackend`。它把对象写入 `ARTIFACT_STORAGE_PATH`，生产部署依赖持久化文件系统和 RWX PVC/NFS。

目标部署环境不提供文件系统持久化，只提供一个受限 S3 endpoint。已确认的能力边界是：

| 能力 | 目标环境 |
|---|---|
| 按完整 key 执行 PutObject | 支持 |
| 按完整 key 执行 GetObject | 支持 |
| `ListObject` / `ListBucket` | 不可用 |
| CRC32 checksum | 不可用 |
| 文件系统持久化 / RWX PVC | 不可用 |

本期将该 endpoint 作为一个仅具备精确 Put/Get 能力的 S3 后端，不扩展或探测其他对象存储能力。

## 2. 设计结论

本期采用最小非文本存储模型：

1. 存储模式只支持 `local` 和 `s3`。
2. 核心存储接口只保留 `Put` 和 `Get`。
3. Backend 接收完整、不可变的对象 key，不理解 item、目录或相对路径。
4. PostgreSQL 维护本期 artifact/Skill 存储域的全部对象映射；每条纳入本期的
   非文本对象记录都同时保存 `storage_backend + storage_key`，包括 asset、
   artifact 和 admin import upload job。
5. 应用不调用 List、Head、Delete、Presign 或 Multipart API。
6. 文本 asset 继续存 PostgreSQL，不经过 Local/S3。
7. API 代理 S3 内容给 CSC，CSC 不直连 S3。
8. 内容完整性继续使用应用层 SHA-256，不依赖 CRC32 或 ETag。

```text
                       PostgreSQL
          business record -> backend + storage_key
                         /              \
                        /                \
               text_content          非文本对象
                    |                     |
                    |                 Put / Get
                    |                     |
                    +---- costrict-web ---+
                               |
                               v
                              CSC
```

## 3. 目标

- 受限 S3 环境可以持久化和下载图片等非文本 Skill asset。
- local 和 s3 使用相同的最小读写契约。
- S3 endpoint 不需要支持对象列举。
- S3 客户端实现不发送 CRC32 checksum。
- API、migrate 和执行同步任务的 worker 使用同一个存储配置与初始化入口。
- S3 模式部署不创建 artifacts PVC 或 StorageClass。
- #185 与 #454 的 manifest、SHA-256 校验和原子安装行为保持不变。

## 4. 非目标

- 不实现完整 S3 API。
- 不实现对象 List、Head、Delete、Copy、Presign 或 Multipart。
- 不通过 S3 构造目录树或统计文件。
- 不让 CSC、浏览器或其他客户端获得 S3 凭据。
- 不把 S3 endpoint 暴露为文件系统。
- 不在本期实现物理对象 GC。
- 不把 ETag 当作 MD5 或 SHA-256。
- 不改变文本 asset 存 PostgreSQL 的策略。

## 5. 数据模型与职责

### 5.1 PostgreSQL 维护本期存储域的全部对象映射

本期纳入 artifact/Skill 存储域的非文本对象不能只保存 `storage_key`。这些
持久化对象引用都必须同时记录：

```text
storage_backend
storage_key
```

本期涉及的映射包括：

| DB 记录 | 不产生对象映射的情况 | 对象映射规则 |
|---|---|---|
| `capability_assets` | UTF-8 文本使用 `text_content`，backend/key 均为空 | 非文本 asset 必须保存 `local` 或 `s3` 以及完整 key |
| `capability_artifacts` | 无 | 每个 artifact 必须保存 backend 和完整 key |
| `capability_import_jobs` | `source_kind=url`，backend/key 均为空 | `source_kind=upload` 必须保存上传 bundle 的 backend 和完整 key |

数据库映射必须满足以下不变量：

- `storage_key` 非空时，`storage_backend` 必须是 `local` 或 `s3`，不能留空。
- `storage_backend` 非空时，`storage_key` 也必须非空。
- 写入对象和 DB 记录时使用当前 `ConfiguredBackend.Kind`，不能根据默认值猜测。
- 读取对象前必须验证记录中的 backend 与当前部署模式一致。
- S3 模式不把空 backend 的历史记录解释为 S3 或 local，而是拒绝读取并暴露迁移错误。

manifest、artifact 下载和 admin import runner 都只读取 PostgreSQL 中的精确映射。
Backend 不提供列举能力。

### 5.2 Backend 只保存内容

Backend 的职责：

- 接收完整 key 并写入字节流。
- 接收完整 key 并返回字节流。

Backend 不负责：

- 生成业务 key。
- 解释 `item_id` 或 `rel_path`。
- 建立目录结构。
- 列举对象。
- 判断哪些对象仍被业务引用。
- 维护对象和数据库记录的一致性。

### 5.3 Key 由服务层生成

Catalog 非文本 asset 沿用内容寻址 key：

```text
catalog/{itemId}/assets/{sha256}/{relativePath}
```

例如：

```text
catalog/7d2.../assets/8a4.../assets/icon.png
```

要求：

- key 使用 `/`，不使用操作系统路径分隔符。
- `relativePath` 必须先通过路径穿越校验和 Unicode 规范化。
- key 必须包含 item/version/hash 中至少一项不可变信息。
- 同一内容重复写入相同 key必须是幂等操作。
- 不覆盖仍被其他 DB 记录引用的旧内容。

对象存储中的 `/` 只是 key 的普通字符，不代表真实目录。

## 6. 核心接口

将 `server/internal/storage/storage.go` 的核心接口收敛为：

```go
type Backend interface {
    Put(ctx context.Context, key string, reader io.Reader, size int64) error
    Get(ctx context.Context, key string) (io.ReadCloser, int64, error)
}
```

存储类型不作为 I/O 方法放进接口。工厂返回配置结果：

```go
type ConfiguredBackend struct {
    Kind    string // local | s3
    Backend Backend
}
```

业务层在创建 `CapabilityAsset` 时写入：

```go
asset.StorageBackend = configured.Kind
asset.StorageKey = storageKey
```

需要移除当前接口中的：

- `Delete`
- `PresignURL`
- `Exists`

本期不为 S3 模式伪造这些能力，也不通过 GET 模拟 Head/Exists。

## 7. 写入流程

```text
读取源文件
  -> 判断为非文本
  -> 计算 SHA-256
  -> 服务层生成完整 key
  -> Backend.Put(key, content, size)
  -> DB 事务写入 storage_backend/storage_key/size/sha
```

详细步骤：

1. 校验相对路径和文件大小。
2. 读取非文本内容并计算 SHA-256。
3. 使用 item ID、SHA-256 和相对路径生成不可变 key。
4. 调用当前配置的 Backend.Put。
5. Put 成功后写 DB 记录。
6. Put 失败时不写入指向不存在对象的 DB 记录。
7. DB 事务失败时允许遗留不可访问对象，本期不调用 Delete。

使用确定性 key 后，任务重试会复用同一个对象位置，减少重复对象。

## 8. 读取流程

```text
请求 itemId + relPath
  -> 鉴权
  -> 查询 capability_assets
  -> text_content 非空：从 DB 返回
  -> storage_key 非空：Backend.Get(storage_key)
  -> API 流式转发
  -> CSC 校验 size + SHA-256
```

规则：

- 文件下载必须先查询 DB，禁止根据 URL 直接拼任意 storage key。
- artifact 和 admin import upload bundle 同样必须先读取 DB 中的
  `storage_backend + storage_key`，再校验 backend 后执行 Get。
- S3 模式遇到 `storage_key` 非空但 `storage_backend` 为空的历史记录必须失败，
  禁止把空值隐式回退到当前 S3 backend。
- 私有仓库的 manifest 和文件下载使用同一套访问控制。
- API 不返回 S3 endpoint 或预签名 URL。
- Get 返回流后由调用方负责关闭。
- S3 的 ETag 只可用于诊断，不能作为内容 SHA-256。

## 9. 删除语义

本期核心接口没有 Delete。

删除或替换 asset 时：

1. 删除或更新 PostgreSQL 中的映射。
2. 旧对象不再能通过 costrict-web 被发现或下载。
3. Local/S3 中的底层字节可能继续存在。

这是逻辑删除，不是物理擦除。物理删除和 GC 作为后续独立能力设计，不阻塞本期 Put/Get 适配。

上线前需要确认：

- 目标 bucket 容量是否允许旧对象累积。
- 是否存在平台侧生命周期、整桶清理或运维删除能力。
- 逻辑删除是否满足当前数据保留和合规要求。

## 10. 后端实现

### 10.1 LocalBackend

LocalBackend 保留当前文件读写方式，但只实现 Put/Get：

- Put：将完整 key 映射到 `ARTIFACT_STORAGE_PATH` 下的文件。
- Get：按完整 key 打开文件并返回流与大小。
- 继续执行 BasePath 边界校验，禁止路径穿越。

LocalBackend 仅在部署环境具备持久化文件系统时使用。

### 10.2 S3Backend

S3Backend 使用 AWS SDK for Go v2 作为请求客户端，但只调用精确 PutObject/GetObject 子集。SDK 是实现机制，不改变本期的能力边界。

配置要求：

- endpoint 必须显式配置。
- region 显式配置。
- path-style 可配置。
- 凭据通过 Kubernetes Secret 注入，并由 SDK credential provider chain 读取。
- 内部 endpoint 可配置自定义 CA。

Put：

- 直接调用 `s3.Client.PutObject`。
- 显式设置 Bucket、Key、Body、ContentLength。
- 不使用 `manager.Uploader`。
- 不启用 multipart。
- 不设置 `ChecksumAlgorithm`。

Get：

- 直接调用 `s3.Client.GetObject`。
- 只传 Bucket 和完整 Key。
- 返回 Body 和 ContentLength。
- 不请求 `ChecksumMode`。

### 10.3 禁用 SDK 默认 CRC32

新版 AWS SDK for Go v2 默认可能为 PutObject 添加 CRC32。初始化 SDK 时必须在代码中显式设置：

```go
config.WithRequestChecksumCalculation(
    aws.RequestChecksumCalculationWhenRequired,
)

config.WithResponseChecksumValidation(
    aws.ResponseChecksumValidationWhenRequired,
)
```

对应环境配置也设置为：

```text
AWS_REQUEST_CHECKSUM_CALCULATION=when_required
AWS_RESPONSE_CHECKSUM_VALIDATION=when_required
```

代码配置是最终保障，不能只依赖部署环境变量。

CRC32 与 SigV4 使用的 SHA-256 不是同一个功能。关闭 CRC32 后仍需保留 endpoint 要求的认证签名。

AWS SDK for Go v2 会为精确对象操作附加固定操作标识：

- PutObject：`?x-id=PutObject`
- GetObject：`?x-id=GetObject`

该参数不触发额外 S3 API，也不表示 List 或 multipart 能力。请求约束只允许与 HTTP method 精确匹配的单一 `x-id` 参数；缺失、重复、值不匹配或附加其他 query 均拒绝。

## 11. 配置

### 11.1 Local 模式

```text
ARTIFACT_STORAGE_BACKEND=local
ARTIFACT_STORAGE_PATH=/app/data/artifacts
```

### 11.2 S3 模式

```text
ARTIFACT_STORAGE_BACKEND=s3

S3_ENDPOINT=https://object-storage.example.internal
S3_BUCKET=costrict-artifacts
S3_REGION=internal
S3_FORCE_PATH_STYLE=true
S3_CA_FILE=/etc/costrict/object-storage/ca.crt

AWS_ACCESS_KEY_ID=...
AWS_SECRET_ACCESS_KEY=...
AWS_SESSION_TOKEN=... # 可选
AWS_REQUEST_CHECKSUM_CALCULATION=when_required
AWS_RESPONSE_CHECKSUM_VALIDATION=when_required
```

要求：

- 凭据通过 Kubernetes Secret 注入。
- Secret 和 Authorization header 不写入日志。
- API、migrate 和 worker 使用相同配置。
- 未知 backend 或缺少必填配置时启动失败。

## 12. 初始化与调用方改造

新增统一工厂：

```go
func NewFromConfig(cfg Config) (*ConfiguredBackend, error)
```

改造范围：

1. `cmd/api` 使用工厂，不再直接构造 LocalBackend。
2. `cmd/migrate` 使用同一个工厂。
3. `CatalogIngestService` 注入 `ConfiguredBackend`。
4. 所有非文本记录使用配置中的 `Kind`，不再写死 `"local"`。
5. 下载 handler 使用注入的 Backend.Get。
6. 清理当前 `Delete/Exists/PresignURL` 调用。
7. 删除与清理路径改为仅维护 DB 引用。

需要检查的现有消费者：

- Catalog 二进制 asset
- Archive upload asset/artifact
- Plugin 子 Skill 二进制 asset
- Artifact 上传和下载
- Admin bundle upload

本期实现时必须确认这些消费者是否只需要 Put/Get；依赖物理删除语义的接口需要明确改为逻辑删除。
其中 admin bundle upload 的 `capability_import_jobs` 必须与 asset/artifact 使用
同一映射规则：上传成功后同时写入 `storage_backend` 和 `storage_key`，runner
读取 bundle 前校验记录 backend。

Memory 文件版本不纳入本期适配。当前 memory 写路径已禁用，但历史
`memory_versions` 只有 `storage_key`，没有 `storage_backend`。因此本期不能声称
该表满足上述不变量，也不能在存在历史 memory 记录时直接切换存储模式；它需要
独立的 schema、对象迁移和读取校验改造。

## 13. Helm 部署

values 建议：

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

persistence:
  enabled: false
```

模板规则：

- API `backend=local`：按现有 persistence 配置决定是否创建和挂载 PVC。
- Worker `backend=local`：不创建 PVC；`artifactStorage.local.existingClaim` 必须指向 API 使用的同一个 RWX claim，且两者 `mountPath` 保持一致。默认值 `api-artifacts` 对应 API release 名为 `api`；自定义 API release 名或 `persistence.existingClaim` 时必须同步覆盖该值，置空会导致 Helm 渲染失败。
- API/Worker `backend=s3`：不创建 artifacts PVC/StorageClass，不挂载 artifacts volume。
- API/Worker `backend=s3`：注入相同的 endpoint、bucket、region、path-style、Secret、可选 CA，以及两项 `when_required` checksum 环境变量。
- API Deployment 中的服务进程、启动时 migrate 子进程和人工执行的 ingest 命令继承同一套配置。
- Worker Deployment 中的同步任务使用同一套配置。
- 未知 backend 或 S3 必填项缺失时 Helm 渲染失败。

## 14. 测试状态

### 14.1 Backend contract test

同一组最小测试运行于 LocalBackend 和 S3Backend：

- Put 后按同一 key Get。
- 字节内容、长度和 SHA-256 一致。
- 空对象。
- 嵌套和 Unicode key。
- 不存在 key 的错误映射。
- context cancel。
- 同一 key、相同内容重复 Put。

### 14.2 S3 请求约束测试（已落地）

使用 `httptest.Server` 模拟 endpoint，验证：

- 只出现 PUT 和 GET。
- PUT 只允许单一 `x-id=PutObject`，GET 只允许单一 `x-id=GetObject`。
- 不出现其他 query，包括 List、`uploads`、`uploadId` 和 `partNumber`。
- 不出现 HEAD、DELETE。
- 不出现 multipart 请求。
- PUT 包含明确 Content-Length。
- 不出现 `x-amz-checksum-crc32`。
- 不出现 CRC32 trailer。
- Get 不请求 checksum mode。

### 14.3 服务端集成测试

- 文本 asset 继续写 DB，Backend 零调用。
- PNG asset 通过 S3Backend Put，DB 写入 `storage_backend=s3`。
- manifest 只从 DB 生成。
- 下载按 DB 中的完整 key 调用 Get。
- Put 失败时不写错误 DB 引用。
- 私有仓库 manifest 和下载权限一致。

### 14.4 MinIO 自动化 E2E（已落地）

测试 Skill：

```text
SKILL.md
scripts/setup.sh
assets/icon.png
```

已实现以下测试设施：

- `server/test/storage-e2e/run.sh`：使用固定版本的 MinIO 和 `minio/mc` 镜像，通过独立 Docker network 和动态宿主端口启动测试环境，退出时清理容器和 network。
- `server/test/storage-e2e/minio-policy.json`：应用凭据只授予 `s3:PutObject` 和 `s3:GetObject`。
- `server/internal/handlers/storage_s3_e2e_test.go`：真实连接 MinIO，并在 S3Backend 与 MinIO 之间加入 recording proxy。
- `.github/workflows/storage-e2e.yaml`：独立 Storage E2E workflow，不改变 Helm 校验或镜像发布 workflow 的触发与执行语义。

runner 初始化完成后先使用应用凭据执行 ListBucket 负向检查，必须返回 AccessDenied；root 凭据只用于 MinIO 初始化，不注入 Go E2E 测试。

自动化验收已覆盖：

1. `SKILL.md` 和 UTF-8 脚本存 DB，不访问对象存储。
2. PNG 通过生产 S3Backend 精确 Put 到 MinIO。
3. DB 保存 `rel_path`、`storage_backend=s3`、完整 key、size 和 SHA-256。
4. asset manifest 只由 DB 生成，不泄露 backend 或 storage key。
5. 文本下载来自 DB，PNG 按 DB 中的完整 key 从 MinIO Get。
6. PNG 下载内容、Content-Length 和 SHA-256 与 DB manifest 一致。
7. recording proxy 只观察到一次 PutObject 和一次 GetObject。
8. 请求使用精确对象路径和明确 Content-Length，不包含 CRC32、trailer、chunked、HEAD、DELETE、List 或 multipart。
9. 请求 query 仅允许 SDK 固定的 `x-id=PutObject` / `x-id=GetObject`，拒绝其他 query。

本地和 CI 使用同一个入口：

```bash
bash server/test/storage-e2e/run.sh
```

截至 2026-07-23，本地 runner 已通过，尚无远端 GitHub Actions run 记录；是否
CI 通过以对应 run 为准。

### 14.5 验收边界

“真实 MinIO E2E”只表示服务端使用生产 S3Backend 与真实对象存储协议完成了
Put/Get，并通过真实 handler 验证 DB manifest/download。它不等同于真实 CSC
下发、订阅和安装。

| 链路 | 状态 | 说明 |
|---|---|---|
| 真实 MinIO Put/Get | 已通过 | 受限应用账号真实写入、读回；ListBucket 为 `AccessDenied` |
| 服务端 manifest/download | 已通过 | 文本来自 DB，非文本来自 MinIO，size、SHA-256 和字节一致 |
| 真实 PostgreSQL / Kubernetes | 未执行 | handler E2E 使用 SQLite 测试 DB，未部署真实 PostgreSQL 和 K8s |
| CSC `skillBundle` / favorites sync | 仅 mock/unit 通过 | 未连接本次真实 costrict-web/MinIO |
| costrict-web distribution -> CSC subscription/install | 未执行 | 尚未运行真实跨仓下发、订阅和原子安装 E2E |
| 目标对象存储 endpoint smoke | 未执行 | 尚未验证实际凭据、CA、path-style、固定 `x-id` 和 CRC32 兼容性 |

### 14.6 上线环境 E2E（待完成）

MinIO 自动化测试不替代目标对象存储 endpoint 的最终验证。上线前仍需完成：

1. 使用目标 endpoint、bucket、应用凭据、region、path-style 和实际 CA 运行精确 Put/Get 冒烟。
2. 确认 endpoint 接受 SDK 固定的 `x-id=PutObject` / `x-id=GetObject`，且不要求 CRC32、List、Head 或 multipart。
3. 在实际 PostgreSQL 和 Kubernetes 部署中验证 API/worker 共用配置，S3 模式不创建 artifacts PVC。
4. 完成 costrict-web + CSC 文件树下载，逐文件校验 size 和 SHA-256，并验证原子安装。
5. 验证 Pod 重启后仍可从 S3 下载非文本对象。

## 15. 实施步骤

### A. 存储抽象（已完成）

- [x] 将 Backend 收敛为 Put/Get。
- [x] 增加 `ConfiguredBackend{Kind, Backend}`。
- [x] 增加 local/s3 工厂。
- [x] 更新测试 backend。

### B. S3Backend（已完成）

- [x] 接入 AWS SDK for Go v2。
- [x] 支持已配置 endpoint、path-style、region、凭据和 CA。
- [x] 在代码中关闭默认 CRC32。
- [x] 增加严格请求约束测试。

### C. 业务接入（已完成）

- [x] API/migrate/worker 统一使用工厂。
- [x] 去除 `storage_backend="local"` 硬编码。
- [x] DB 维护本期 artifact/Skill 存储域全部对象的 backend/key 映射。
- [x] Admin import upload job 记录并校验 `storage_backend + storage_key`。
- [x] 调整 Delete/Exists/Presign 调用方。
- [x] 补齐最小权限和请求约束测试。

### D. 部署与自动化（已完成）

- [x] 扩展 Helm values/templates，S3 模式不渲染 artifacts persistence。
- [x] 使用真实 MinIO、Put/Get-only policy 和 recording proxy 运行服务端 E2E。
- [x] 增加独立 Storage E2E workflow 定义。

### E. 目标环境联调与迁移（待完成）

- [ ] 在目标 endpoint 运行精确 Put/Get 和 CRC32 兼容性冒烟。
- [ ] 在目标 Kubernetes 环境完成 costrict-web + CSC 文件树 E2E。
- [ ] 实现并验证 local→s3 数据迁移工具。
- [ ] 根据 DB 记录迁移存量非文本对象，校验 SHA-256 后原子更新 backend/key 映射。
- [ ] 迁移 `capability_import_jobs` 的 upload bundle 映射，或在切换前完成/取消
  所有未完成的 upload job。

## 16. 发布与回滚

发布前检查：

- 目标 endpoint 的精确 Put/Get 通过。
- SDK 请求不包含 CRC32。
- 未调用 List、Head、Delete 或 multipart。
- bucket、凭据、CA 和网络配置完成。
- E2E SHA-256 一致。
- 目标环境接受本期仅逻辑删除。

若数据库已有 local 非文本对象，切换到 s3 前必须迁移。迁移枚举范围至少包括
`capability_assets`、`capability_artifacts` 和 `capability_import_jobs` 中的
upload bundle，不能只迁移可从 manifest 发现的 asset。

`memory_versions` 不在本期迁移工具范围内。切换前必须检查该表；只要存在记录，
就应阻断模式切换，直到独立 memory 迁移完成并为其补齐 backend 判别。不能把
memory 的 `storage_key` 当作当前 backend 的 key 直接读取。

以下是迁移工具必须实现的流程，当前尚未实现：

1. 进入维护窗口，停止创建新的对象写入和 admin import upload job，并确保
   import runner 不再领取新任务。
2. 从 PostgreSQL 枚举所有非空 `storage_key` 及对应 `storage_backend`，不扫描
   本地目录或 S3。
3. 从 LocalBackend Get。
4. 向 S3Backend Put 相同或新 key。
5. 对源流和目标流计算 SHA-256 并比对；记录已有内容摘要时还要与 DB 值比对。
6. 更新 DB 的 `storage_backend` 和 `storage_key`。

切换前还必须检查 admin import upload job：

- `pending`、`running`、`previewed` 等未完成任务仍可能由 runner 读取上传 bundle。
- 对这些任务必须迁移 bundle 并原子更新 backend/key，或者先在 local 模式完成
  或取消任务，使其不再被调度。
- 禁止在存在未迁移且未取消的 upload job 时切换 API/worker 到 S3。
- 任意 `storage_key` 非空而 `storage_backend` 为空的历史记录都是迁移阻断项；
  S3 模式不会兼容读取这种记录。
- `memory_versions` 非空同样是迁移阻断项，不能被本期枚举范围静默遗漏。

一旦 DB 已写入 `storage_backend=s3`，回滚版本必须保留 S3 Get 能力，否则已入库对象会不可读。

## 17. 风险与部署输入

| 项目 | 当前处理 |
|---|---|
| List 不可用 | DB 是唯一对象索引 |
| CRC32 不可用 | SDK 代码和环境均设为 `when_required` |
| Delete 不可用 | 本期只做 DB 逻辑删除 |
| 旧对象累积 | 接受并监控容量，GC 后续设计 |
| PUT 后 DB 失败 | 可能留下不可发现对象，确定性 key 降低重复量 |
| ETag 语义 | 不依赖，继续校验 SHA-256 |
| Put 的 size/context 契约 | 本期不扩展；后续统一 local 与 S3 的长度校验和取消语义 |

部署时需要由环境提供以下参数：

1. endpoint、region、path-style 和认证方式。
2. bucket、访问凭据和可选内部 CA。
3. 单对象大小、并发和超时限制。
4. 对象容量配额及平台侧生命周期或运维清理方式。
