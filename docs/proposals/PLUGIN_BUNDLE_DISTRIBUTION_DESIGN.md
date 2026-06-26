# Plugin Bundle Distribution（Plugin DB+HTTP 订阅即分发）功能设计文档

**Status**: Implemented (未 push / 未上线)
**Author**: papysans
**Date**: 2026-06-26
**关联实现**: 后端 `68103a3`(PR1)+ `3e36a17`(PR2);csc `62fbe2bea`(PR3)+ `d13470978`(E2E 修复),均在分支 `feat/plugin-db-http-distribution`

---

## 1. 背景与目标

### 1.1 背景

costrict-web 里 Skill / MCP 等类型走 **DB + REST**:内容存 `capability_items.content`,客户端 `favorite → reconcile → 写盘`。而 **Plugin 是多文件目录树**,历史上靠**客户端 git clone** 外部仓库分发,与其它类型割裂:

- catalog ingest 的 `syncAssetsForItem` 是 no-op(`services/catalog_ingest_service.go`),catalog plugin 在 DB 里只存了合成主文件 + 提升出的 typed 子项,**不是完整文件树** → `DownloadPluginZip` 对 catalog plugin 产残缺包。
- 国内 git clone 不可达,为此搭了 Gitea 自托管镜像(`gitea.costrict.ai`)兜底(见 [[../CATALOG_INGEST.md]] 上游链路 + Gitea 镜像项目)。
- csc 侧 plugin 与 skill/mcp 是两套传输,「订阅即分发」逻辑不统一。

### 1.2 目标

1. 新增一条 **DB+HTTP「订阅即分发」通道**:plugin 与 skill/mcp 一样,在系统内 favorite/订阅 → 后端持有**整包(ZIP)** → 经 HTTP 下发 → csc 解压落盘,**全程不调 git**。
2. **整包无损**:hooks / 辅助脚本 / 二进制 / 非 typed 文件 / 可执行位全部保留,文件级等价于 git clone。
3. **统一**:plugin 复用 csc 既有 `favorite → reconcile` 机制,不再特殊。
4. **存量自然迁移**:已 git 装的 plugin 不动,仅新订阅 / 下次更新走新链路。

### 1.3 非目标

- **不下线、不改** 现有 git 通道(GitHub Marketplace / Gitea 镜像)——它们继续服务非 csc 用户(Claude Code 生态)与 git-wanting 客户端。本设计是**叠加**一条通道,不是替换。
- 不改上游 `everything-ai-coding` / `build_catalog_bundle.py` 的 catalog 构建。
- 不做 S3 存储后端(本期单实例 `LocalBackend` 可接受)。
- 不做客户端热重载(更新在 csc 重启时生效,与既有语义一致)。

---

## 2. 术语定义

| 术语 | 定义 |
|------|------|
| **整包 / Bundle** | 一个 plugin 的完整文件树打成的 **ZIP**(非 tar) |
| **lazy clone-and-pack** | 后端首次需要某 plugin 整包时,server-side `git clone` 其 `source_url` → 打 ZIP → 缓存为 artifact |
| **版本键 / bundleVersion** | 整包的确定性版本标识 = catalog plugin 用**上游 commit SHA**;upload plugin 用 **ZIP content-hash**。**不用** `source_sha`(只 hash 合成主文件,漏 hooks/脚本) |
| **clone_pack / upload_pack** | `CapabilityArtifact.SourceType` 的两个新值,区分整包来源(clone 自上游 / 从上传 assets 打包) |
| **installMethod** | csc installation entry 上的标记 `'git' \| 'bundle'`,用于**自然迁移**识别 |
| **多通道** | git(GitHub Marketplace / Gitea 镜像)+ DB+HTTP(本设计,csc 主路径)并存 |
| **订阅即分发** | favorite 一个 item(含 plugin)即触发其向设备的分发,逻辑对所有类型统一 |

---

## 3. 链路总览

```
┌───────────────── 上游 everything-ai-coding(外部仓,既有)─────────────────┐
│  检索(trending / claude-plugins-* / 自己人 onboarding)+ AI 评分          │
│  → catalog/plugins/index.json:每条带 id/type/source/★source_url/        │
│     final_score/security/version/stars                                    │
│  → build_catalog_bundle.py → catalog-bundle.tar.gz                        │
└───────────────────────────────────┬──────────────────────────────────────┘
                                     │  migrate ingest-upstream --source=...
                                     ▼
┌──────────────────────────────── costrict-web ─────────────────────────────┐
│  CatalogIngestService.Ingest                                               │
│    └─ ★解析并持久化 source_url → capability_items.source_url               │
│    └─ 内容变更分支 → ★enqueueBundleRefresh(plugin 且有 source_url)        │
│                                                                            │
│  capability_items(source_url ★)  ──/api/items──▶ ★bundleUrl/Version/Ready │
│                                                                            │
│  ★BundleJob 队列 ──worker(FOR UPDATE SKIP LOCKED)──▶ ★BundlePackService   │
│      parseSourceURL → mapToMirror(★Gitea 映射,默认空=直连)→ GitService    │
│      .Clone → ★PackZip(无损 ZIP)→ ★CapabilityArtifact(clone_pack,        │
│      version=commit SHA,IsLatest)                                         │
│                                                                            │
│  ★GET /api/plugins/:slug/bundle                                            │
│      命中成品 → 200 application/zip(X-Bundle-Version/X-Checksum-SHA256)   │
│      catalog 未打包 → 202 + 入队                                           │
│      upload plugin(无 source_url 有 assets)→ ★PackUploadBundle 同步返回   │
└───────────────────────────────────┬──────────────────────────────────────┘
                                     │  鉴权 HTTP(createCoStrictFetch)
                                     ▼
┌────────────────────────────────── csc 客户端 ─────────────────────────────┐
│  reconcileCloudPlugins(favorited 同步)                                    │
│    └─ Case A 新装:有 bundleUrl 且 bundleReady → ★bundle source 安装        │
│    └─ Case B 更新:比对 bundleVersion → ★updatePluginToBundleOp            │
│    └─ ★自然迁移守卫:installMethod!='bundle' 的旧 git 装 → 跳过不动         │
│  ★cachePluginFromBundle:fetch ZIP → sha 校验 → extractZipToDirectory      │
│      (保 exec 位)→ 装到 ~/.claude/plugins/cache/{mkt}/{plugin}/{ver}/     │
│  全程零 git(installFromGit* 保留作存量/marketplace 回退)                  │
└────────────────────────────────────────────────────────────────────────────┘

★ = 本次新增 / 修改
```

---

## 4. 数据模型设计

### 4.1 `CapabilityItem.SourceURL`(新增字段,PR1)

catalog 每条 plugin 同时带 `source`(provenance 标签,如 `claude-plugins-dev`)和 `source_url`(**真实 clone URL**,含 branch + 子目录)。旧 ingest 只存了 `source`,**丢弃了 `source_url`** → DB 里没有可 clone 的地址。本次补上:

```go
// models/models.go — CapabilityItem
SourceURL string `gorm:"column:source_url" json:"sourceUrl"`
// 上游真实 clone URL(含 branch/subdir),如
// https://github.com/owner/repo/tree/main/subdir
```

迁移:`migrations/20260625000000_add_source_url_to_capability_items.sql`(`TEXT NOT NULL DEFAULT ''`,TEXT 因 monorepo 子目录 URL 可能超 255)。
写入点:`insertItem` / `updateItem` / `applyMetadataDelta`(后者保证**存量行 re-ingest 时回填**,即便主文件 SHA 未变)。

### 4.2 `BundleJob`(新表,PR2)

异步 clone-and-pack 队列,仿 `ScanJob` + `SyncJob` 范式。

```go
type BundleJob struct {
    ID          string
    ItemID      string   // text
    TriggerType string   // refresh(ingest) | on_demand(202)
    TriggerUser string
    Status      string   // pending | running | success | failed
    RetryCount  int
    MaxAttempts int
    LastError   string
    ArtifactID  *string  // 成功后指向产出的 CapabilityArtifact
    ScheduledAt, StartedAt, FinishedAt, CreatedAt, UpdatedAt time.Time
}
```

迁移:`migrations/20260625100000_create_bundle_jobs.sql`。**去重**:partial unique index `idx_bundle_jobs_active_item (item_id) WHERE status IN ('pending','running')` —— 同 item 同时只允许一个在飞 job,防并发重复 clone。

### 4.3 `CapabilityArtifact`(复用既有,新增两个 SourceType)

整包直接落已有的 artifact 表 + `StorageBackend`(S3 接口已留,当前仅 `LocalBackend`):

| 字段 | 整包语义 |
|------|---------|
| `ArtifactVersion` | **版本键** = clone_pack 用 commit SHA / upload_pack 用 ZIP content-hash |
| `ChecksumSHA256` | ZIP 自身 sha256 |
| `SourceType` | `clone_pack`(lazy clone)/ `upload_pack`(上传 assets 打包)/ `upload`(既有) |
| `IsLatest` | 同 item 同 SourceType 互斥,标记最新整包 |
| `StorageKey` | `<repoID>/<itemID>/bundle/<version>.zip` |

---

## 5. 后端设计

### 5.1 source_url 持久化(PR1)

- `catalogEntry` 加 `SourceURL string json:"source_url"`(`services/catalog_ingest_service.go`)。
- 写入三处:`insertItem` / `updateItem`(内容变更路径)+ `computeMetadataDelta`/`applyMetadataDelta`(metadata-only 路径,保证存量回填)。

### 5.2 lazy clone-and-pack + 异步(PR2)

- `services/source_url.go`:
  - `parseSourceURL(raw) → (cloneURL, branch, subPath, err)`:解析 `.../tree/<branch>/<subPath>`,无 `/tree` → 默认分支 + 整仓。
  - `mapToMirror(url)`:GitHub→镜像重写,`GIT_MIRROR_BASE` 为空时**原样直连**(Gitea 命名方案当前 parked,见 §11)。
  - `validateCloneURL`:**只允许 http(s)**,拒绝 `file://`/非 http(防 SSRF / 本地仓泄漏,见 §8)。
- `services/git_service.go` `PackZip(localPath, subPath) → (zipBytes, sha256)`:`archive/zip`,排除 `.git`/`node_modules`,**保留 mode 位(exec)**,软链接存为 target string(防 zip-slip),确定性(排序 + 固定 modtime)。
- `services/bundle_pack_service.go`:
  - `PackItemBundle(ctx, item)`:`parseSourceURL → mapToMirror → validateCloneURL → GitService.Clone(Depth=1)→ PackZip → Storage.Put → upsert CapabilityArtifact`。**幂等**(commit SHA 已有成品则复用);`IsLatest` 在单事务内 demote+create;失败清临时目录 + 删孤儿。
  - `PackUploadBundle(item)`:上传 plugin 从 `capability_assets`(+ `item.Content` 兜底)同步打 ZIP,`SourceType=upload_pack`,版本键 = content-hash。
- `worker/bundle_worker.go`:`BundleWorkerPool`,`FOR UPDATE SKIP LOCKED` 取 pending → `PackItemBundle` → finalize(成功记 artifact_id / 指数退避重试 / 永久失败)。装在 `cmd/worker`。

### 5.3 bundle 下发接口(PR2)

`GET /api/plugins/:slug/bundle` → `handlers.DownloadPluginBundle`(`cmd/api/main.go`)。**不复用** `DownloadPluginZip`(它从空 assets 重建对 catalog plugin 产残包)。

| 情况 | 响应 |
|------|------|
| 命中 `IsLatest` 整包 artifact | `200 application/zip` 流式 + `X-Bundle-Version` + `X-Checksum-SHA256`,异步 `download_count++` |
| catalog plugin(有 source_url)未打包 | `202 Accepted` + `BundleJobService.Enqueue` |
| upload plugin(无 source_url 有 assets)未打包 | 同步 `PackUploadBundle` 后 `200` |
| 私有 / 不可见 | 沿用 `DownloadPluginZip` 的 visibility 检查 |

### 5.4 下发元数据与更新触发

- `ItemResponse` 加 `BundleURL` / `BundleVersion` / `BundleReady`(`handlers/capability_item.go` `buildItemResponse`,从已 Preload 的 `item.Artifacts` 填,仅 plugin)。
- **更新触发(简单方案,本期决策)**:ingest 时某 plugin 内容变更 → `enqueueBundleRefresh` 重新 clone+pack。**不引入定时轮询调度器**(定时轮询上游 HEAD 自动 refresh = 未来项)。

---

## 6. 客户端设计(csc)

代码在 csc 仓 `feat/plugin-db-http-distribution`(`62fbe2bea` + `d13470978`)。

- `schemas.ts`:`PluginSource` 加 `bundle` 变体 `{ source:'bundle', url, sha256?, version? }`;`PluginInstallationEntry` 加 `installMethod?:'git'|'bundle'` + `bundleVersion?`。
- `pluginLoader.ts` `cachePluginFromBundle`:`createCoStrictFetch`(鉴权)→ `arrayBuffer` → sha256 校验(`source.sha256` 或 `X-Checksum-SHA256`)→ `extractZipToDirectory`(保 exec 位)→ 读 manifest → 返回与 `cachePlugin` 同契约 `{path, manifest}`。`202` → 抛 `PluginBundleNotReadyError`(`pluginBundleErrors.ts`),reconcile 视为瞬时跳过。
- `pluginVersioning.ts`:`calculatePluginVersion` 加 bundle 分支(确定性版本目录名)。
- `pluginInstallationHelpers.ts` / `installedPluginsManager.ts`:bundle 安装写 `installMethod:'bundle'` + `bundleVersion`。
- `reconcileCloudPlugins.ts`:
  - 抽取 item 的 `bundleUrl/Version/Ready`。
  - **Case A 新装**:有 bundleUrl 且 `bundleReady!==false` → 经 `installPluginOp(bundleSource)` 走合成 entry 安装;`bundleReady===false` / 抛 not-ready → 跳过不记 `install_failed`。
  - **Case B 更新感知(从零新增)**:已装 plugin 比对 `bundleVersion` ≠ desired → `updatePluginToBundleOp`(复用 `performPluginUpdate` 骨架,下载换 HTTP)。
  - **自然迁移守卫**:仅对 `installMethod==='bundle'` 的条目做版本比对/更新;旧 git 装的(无 installMethod、有 gitCommitSha)**永不触碰**。
- **R2 约束**:install/update 触发只在 `reconcile` / `pluginOperations` 内;favorite / `PluginAdapter` 侧不得旁路。
- `gitClone` / `installFromGit*` **保留**(存量自然迁移 + marketplace 物化仍需)。

---

## 7. 多通道模型

```
上游 plugin 仓库 (GitHub)
   ├─→ 通道1  GitHub Marketplace (git)      → 非 csc 用户 / Claude 生态   [保留]
   ├─→ 通道0  Gitea 镜像 (git, 国内可达)     → git-wanting 客户端 / 后端 lazy clone 源(规划) [保留]
   └─→ 后端 ingest → DB → 通道2  DB+HTTP     → csc 订阅即分发(主路径)    [本设计]
```

三通道并存、不互斥。csc 主路径切到通道 2;git 通道全程保留 = 天然回退(回退即 csc 退回 git case)。

---

## 8. 安全

| 关注点 | 处理 |
|--------|------|
| SSRF / 本地仓泄漏 | `validateCloneURL` 只允许 http(s),拒绝 `file://`/内网 scheme(`source_url` 经 ingest 落到公开可下载的 bundle,需守卫);`AllowLocalClone` 默认 false,仅测试置 true |
| zip-slip / 软链接逃逸 | `PackZip` 软链接存为 target string、不跟随;`.git`/`node_modules` 排除 |
| 可见性 | bundle 端点复用 `DownloadPluginZip` 的 repo 可见性检查,private item 不泄漏 |
| 完整性 | 端点回 `X-Checksum-SHA256`;csc 下载后 sha256 校验 |
| exec 位 | 全程经 `PackZip`(存 mode 位)+ `extractZipToDirectory`(还原),hooks/scripts 保 +x |

---

## 9. 关键实现文件

| 仓 / 文件 | 用途 |
|------|------|
| `server/internal/models/models.go` | `CapabilityItem.SourceURL`、`BundleJob` 模型 |
| `server/migrations/20260625000000_*.sql` / `20260625100000_*.sql` | source_url 列 / bundle_jobs 表 |
| `server/internal/services/catalog_ingest_service.go` | source_url 解析持久化 + enqueueBundleRefresh |
| `server/internal/services/source_url.go` | parseSourceURL / mapToMirror / validateCloneURL |
| `server/internal/services/git_service.go` | `PackZip` |
| `server/internal/services/bundle_pack_service.go` | PackItemBundle / PackUploadBundle |
| `server/internal/services/bundle_job_service.go` | Enqueue / dedup |
| `server/internal/worker/bundle_worker.go` | 异步 worker loop |
| `server/internal/handlers/plugin_bundle.go` | `GET /api/plugins/:slug/bundle` |
| `server/internal/handlers/capability_item.go` | ItemResponse bundle 字段 |
| `csc: src/utils/plugins/pluginLoader.ts` | `cachePluginFromBundle` + finalize 守卫 |
| `csc: src/utils/plugins/schemas.ts` | bundle source 变体 + installMethod/bundleVersion |
| `csc: src/utils/plugins/pluginBundleErrors.ts` | `PluginBundleNotReadyError`(202 信号) |
| `csc: src/services/plugins/pluginOperations.ts` | installPluginOp bundle 参数 + `updatePluginToBundleOp` |
| `csc: src/costrict/favorite/reconcileCloudPlugins.ts` | bundle 安装 + 更新感知 + 自然迁移守卫 |

---

## 10. 验证现状

| 维度 | 状态 |
|------|------|
| 单测 | 后端 services/handlers/worker 全绿;csc reconcile 20/20 + cachePluginFromBundle 回归测试 |
| 对抗审(trellis-check) | 后端 1 P1 修(SSRF 守卫);csc 0 P0/P1,17 个高风险面逐条核实(更新死循环/自然迁移误覆盖/202→install_failed/R2 旁路/资源泄漏…) |
| 后端整包链路真机 | ✅ source_url plugin → 202 → worker clone(真实 GitHub)+ pack → 200 无损 ZIP,版本键=真实 commit SHA,幂等缓存 |
| csc 安装原语真机 | ✅ 沙箱化跑真实 `cachePluginFromBundle` 打真实端点,无损落盘零 git;**并抓修一个单测漏掉的 bug**(`d13470978` finalizeCachedPluginDir 自我 rename) |
| 未活体 | 完整 TUI reconcile 定时循环 / upload_pack / subdir clone / 更新场景 / 国内网络 |

---

## 11. 上线前置 与 风险

### 11.1 上线前置(必做)
1. **存量 source_url 全量回填**:对真实库跑 `migrate ingest-upstream`(PR1 已支持 metadata-only 路径回填);否则存量 catalog plugin 无 clone 源,bundle 端点一直 202/失败。
2. **后端到上游的 git 可达性**:lazy clone 现直连 `github.com`;prod 国内机房若不通 → 打包失败。上 prod 前须**二选一**:确认后端出网,或接入 Gitea 镜像映射(见 11.2)。
3. **部署序**:后端 PR1→PR2 必须**先于** csc PR3 上线(csc 依赖 bundle 端点与 ItemResponse 字段)。

### 11.2 已知限制 / Parked
| 项 | 状态 |
|----|------|
| Gitea 仓命名映射(`mapToMirror`) | **Parked**:`<base>/<owner>-<repo>` 占位 + TODO,`GIT_MIRROR_BASE` 空=直连 inert;真走国内镜像需确认 `gitea.costrict.ai` 的 per-plugin 仓命名 |
| S3 storage backend | 接口已留,仅 LocalBackend;多副本部署不共享整包缓存 |
| worker `processOne` 无 panic recovery | 与既有 `scan_worker` 一致;clone 任意上游仓风险面更大,建议补 `defer recover` 硬化 |

---

## 12. 离线 / 私有化部署（air-gap，可选）

### 12.1 两种填充模式（同一套分发，种子来源不同）

| 模式 | 适用 | artifact 怎么进 DB | 是否需外网 |
|------|------|-------------------|-----------|
| **online（默认，线上 SaaS）** | 公网环境 | lazy clone-and-pack（§5）：首次需要时 clone `source_url` → 打 ZIP → 缓存 | 后端需访问上游(GitHub/Gitea) |
| **offline（air-gap 私有化）** | 内网/离线 | **不 clone**：一个**完整离线包**导入时,ingest 直接把 plugin 整包写成 `CapabilityArtifact` | 否 |

两模式产出的 DB 形态、bundle 端点、csc 行为**完全一致** —— 区别只在「artifact 怎么进 DB」。

### 12.2 为什么 air-gap 天然适配
- 客户端(csc)连客户**内网** costrict-web 走纯 HTTP 取整包,**零 git、零外网**(PR3 已解决)。
- 后端 serve 阶段只读 DB + 本地 artifact 存储,**零外网**。
- 唯一需外网的 lazy clone,在 offline 模式被**离线种子**替代。

### 12.3 离线包内容（全类型一次性）
在现有 `catalog-bundle.tar.gz`(已含 skill/mcp 内联内容 + 元数据)基础上**追加 plugin 整包 ZIP + 版本键**,合成一个「**全类型完整离线包**」(skill + mcp + plugin 一起)。客户一次导入即全部入库。

### 12.4 离线包怎么 bake（有外网的一方预先做）
- 由 vendor 在联网环境跑一遍 lazy clone-pack(或上游 `build_catalog_bundle.py` 直接产出 plugin ZIP),把整包 + commit SHA 版本键打进离线包。
- 客户通过既有离线导入入口 `migrate ingest-upstream --source=<offline-bundle>`(U 盘/内部镜像搬入)一键导入。

### 12.5 后端改动（已实现 PR5 `dbe497c`）
- ingest「**从 bundle 写 `CapabilityArtifact`**」路径(`seeded` SourceType,IsLatest,跳过 clone),模式自动判定(有整包就 seed、否则 lazy),seed 失败降级 lazy。**已真机验证**(离线 bundle → ingest seed → bundle 端点直出 ZIP → csc 装上,source_url 用假仓证 0-clone)。

### 12.6 上游改动:air-gap 合成 bundle 构建器（待实现,跨仓上游）

**决策(用户锁定):只两档,air-gap 不保留部分 lazy。**
- **online**(有 git 仓库)→ lazy clone,**catalog bundle 不带 plugin 整包**(现状,不动)。
- **air-gap**(内网完全无网)→ **一个更大的合成 bundle**,带全部 plugin 整包。

**air-gap = 把"最新的两个 bundle"合成一个(单独新增路径,不破坏 online 链路):**
1. **catalog bundle**(`build_catalog_bundle.py` 产出,skill/mcp 内联 + plugin 元数据)——`everything-ai-coding`/`costrict-skills-repo`。
2. **marketplace bundle**(`costrict-plugin-marketplace/build/costrict-marketplace-bundle-vX.Y.Z`)——每 plugin 一个 **bare git 仓** `repos/plugins/<owner>-<repo>-marketplace-<plugin>.git`;`manifest.json` 带 `plugins:[{id,name,version,size_bytes}]` + `catalog_source`/`catalog_sha`(两 bundle 同源)。

**合成器(已实现:`everything-ai-coding/scripts/build_airgap_bundle.py` + `.md`,commit main `fd84d92`,不改 `build_catalog_bundle.py`/`build.py`):**
> 实现要点:catalog `id` 与 marketplace bare 仓目录名**恒等**(marketplace build.py 用 entry id 原样建仓),无需 slug 化;catalog index 的 plugin 条目**本来就有 `bundle` 键**(子项计数+source_ref/plugin_root),合成器 **merge** 注入 `file/version/sha256` 而非覆盖,后端 `catalogBundleRef` 只读这三键、忽略其余,双向无损;CLI `--catalog-bundle/--marketplace-bundle/--output/--limit`;已对后端真实 Ingest 验证 seed 兼容。
- 输入:catalog bundle + marketplace bundle。
- 对每个 catalog plugin 条目 → 映射到 marketplace bare 仓(via `install.marketplace_repo`+`plugin_name` ↔ marketplace id)→ `git archive --format=zip HEAD`(出整包,含 exec 位,无 .git)→ `version=git rev-parse HEAD`,`sha256` of zip。
- 写 `catalog-download/plugins/<id>/bundle.zip` + 注入 `bundle:{file,version,sha256}` 块进 index.json(后端 PR5 消费此块)。
- 重 tar → 更大的 air-gap bundle。
- skill/mcp 原样内联(不需整包)。

### 12.7 两档总结
| 档 | 网络 | catalog bundle | plugin 内容 |
|----|------|---------------|-------------|
| **online** | 有 git | 不带整包 | lazy clone(运行时) |
| **air-gap** | 完全无网 | 带全部 plugin 整包(合成 bundle) | seed 进 DB(导入时) |

---

## 附录:设计决策记录 (ADR)

### ADR-1:方案 B(整包无损)vs 从 DB 子项重建
**决策**:server-side clone 整仓打 ZIP,而非从已提升的 typed 子项重建。
**理由**:catalog plugin 的 `capability_assets` 为空(`syncAssetsForItem` no-op),子项重建会丢 hooks/辅助脚本/非 typed 文件;clone 整仓才文件级无损 = 等价 git clone。

### ADR-2:lazy clone(首次订阅)vs eager(ingest 全量)
**决策**:lazy(首次需要时 clone+pack,缓存)。
**理由**:~1563 catalog plugin 多数无人订阅,eager 全量 clone + 存储浪费大;lazy 天然增量,首次一次性延迟可接受(配 202 异步)。

### ADR-3:版本键 = 上游 commit SHA(clone)/ content-hash(upload),禁用 source_sha
**决策**:整包版本独立于 `item.SourceSHA`。
**理由**:`source_sha` 只 hash 合成主文件,**漏 hooks/脚本变更**;且版本键必须确定性,否则 csc `getVersionedCachePath` 缓存永不命中、反复重拉。

### ADR-4:自然迁移用显式 `installMethod` 标记
**决策**:csc installation entry 标 `installMethod:'git'|'bundle'`,reconcile 只对 bundle 条目比对版本。
**理由**:比"gitCommitSha 是否为空"无歧义;存量 git 装的零风险不被触碰。

### ADR-5:整包格式 ZIP(非 tar.gz)
**决策**:后端产 ZIP。
**理由**:csc 已有成熟 ZIP 链路(`mcpbHandler` / `zipCache.extractZipToDirectory`,含 exec 位还原),tar 是新依赖;后端 `archive/zip` 现成。

### ADR-6:保留 git 通道 + Gitea 暂藏
**决策**:不下线 Marketplace/Gitea;Gitea 作为(规划中的)后端 lazy clone 国内源 + git-wanting 客户端通道,但命名映射本期 parked。
**理由**:多通道并存给天然回退;Gitea 每 plugin 独立仓可绕开子目录 clone + 后端自托管可达,是未来国内方案的落点。

### ADR-7:更新触发用简单方案(ingest enqueue)
**决策**:上游更新经 ingest 内容变更 → enqueueBundleRefresh 重 pack;不做定时轮询调度器。
**理由**:靠现有 ingest 节奏驱动,最快落地、满足 AC;定时轮询 HEAD 自动 refresh 留作未来项。

### ADR-8:在线 lazy clone vs 离线 seed 双模式
**决策**:线上 SaaS 用 lazy clone(§5);私有化 / air-gap 用「完整离线包(skill+mcp+plugin)直接 seed DB、跳过 clone」,作为**可选**模式(§12)。
**理由**:DB+HTTP 架构使内容可完全驻留客户 DB;serve 与客户端均无需外网,唯一外网点(lazy clone)用离线种子替代即可 air-gap。两模式产出形态一致,bundle 端点 / csc 零差异。
**待实现**:ingest 从 bundle 写 artifact 的路径 + 模式判定(新 PR);需上游离线包携带 plugin 整包 + 版本键。
