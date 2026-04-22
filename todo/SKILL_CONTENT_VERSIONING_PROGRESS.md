# Skill 内容版本化实施清单

参考方案：`docs/proposals/SKILL_CONTENT_VERSIONING_DESIGN.md`

## 总体进度

- **P0 阶段**（数据模型 + 内容哈希基础能力）：🟡 进行中
- **P1 阶段**（创建/更新升版规则改造）：🟡 进行中
- **P2 阶段**（接口增强 + 一致性校验）：✅ 已完成
- **P3 阶段**（存量数据迁移 + 稳定性验证）：✅ 已完成

---

## P0 任务清单（数据模型 + 内容哈希基础能力）

| # | 任务 | 状态 | 说明 |
|---|------|------|------|
| 1 | `CapabilityItem` 新增 `ContentMD5` 字段 | ✅ 已完成 | 已在模型层补充字段 |
| 2 | `CapabilityItem` 新增 `CurrentRevision` 字段 | ✅ 已完成 | 已在模型层补充字段 |
| 3 | `CapabilityVersion` 新增 `ContentMD5` 字段 | ✅ 已完成 | 已在模型层补充字段 |
| 4 | AutoMigrate / 迁移逻辑补充新字段 | 🟡 进行中 | `AutoMigrate` 已覆盖模型；测试建表与导入迁移逻辑已补充，仍需补正式回填迁移 |
| 5 | 新增 `ContentHashService` | ✅ 已完成 | 已新增统一哈希服务 |
| 6 | 文本内容规范化规则实现 | ✅ 已完成 | 已统一 CRLF/LF |
| 7 | JSON canonical 序列化能力实现 | ✅ 已完成 | MCP/JSON 内容已走 canonical JSON |
| 8 | archive 整体内容 MD5 计算实现 | ✅ 已完成 | 已基于主文件+assets manifest 计算 |

### P0 设计约束

1. `CapabilityItem.Version` 继续保留为业务版本字段，不改造成 `v1/v2/v3`
2. `CapabilityVersion.Revision` 继续作为内容修订号主语义
3. archive 场景**不能**直接对 zip/tgz 二进制本身计算 MD5
4. archive 整体内容 MD5 必须基于 `RelPath + 文件内容摘要` 形成稳定 manifest 后再计算

---

## P1 任务清单（创建/更新升版规则改造）

| # | 任务 | 状态 | 说明 |
|---|------|------|------|
| 9 | `persistNewItem` 支持写入 `ContentMD5` | ✅ 已完成 | 创建 item/version 时同步落库 |
| 10 | 创建路径初始化 `CurrentRevision = 1` | ✅ 已完成 | 新建 item 默认 revision=1 |
| 11 | 首个 `CapabilityVersion` 写入 `ContentMD5` | ✅ 已完成 | 初始版本已记录内容摘要 |
| 12 | JSON 创建路径接入内容哈希计算 | ✅ 已完成 | `CreateItem` / `CreateItemDirect` 已接入 |
| 13 | archive 创建路径接入整体内容哈希计算 | ✅ 已完成 | archive 创建前已计算整体 MD5 |
| 14 | `UpdateItem` JSON 路径改造为“内容变化才升版” | ✅ 已完成 | 相同内容不再新增版本 |
| 15 | `UpdateItem` archive 路径改造为“整体内容变化才升版” | ✅ 已完成 | asset 变化可触发升版，相同整体内容不升版 |
| 16 | 非内容字段更新不升版 | ✅ 已完成 | 非内容更新仅保存 item |
| 17 | 内容变化时同步更新 `CurrentRevision` | ✅ 已完成 | 升版时同步更新当前 revision |
| 18 | 升版路径的 `CommitMsg` 与 metadata 保持一致 | ✅ 已完成 | 新版本已保留 metadata 与 commitMsg |
| 19 | scan 入队逻辑适配新 revision 规则 | ✅ 已完成 | 仅在新版本产生时入队对应 revision |

## 当前实施备注

- 已完成 P0 + P1 核心代码改造。
- 已完成 P2 主要能力：对外返回 `versionLabel/currentVersionLabel` 与一致性校验接口。
- P3 的正式数据库回填迁移仍待补充。

### P1 验收口径

1. 相同内容重复提交，不新增 `CapabilityVersion`
2. archive 主文件不变但 asset 变化，必须新增 `CapabilityVersion`
3. 更新业务版本 `Version` 时不影响内容版本 `Revision`

---

## P2 任务清单（接口增强 + 一致性校验）

| # | 任务 | 状态 | 说明 |
|---|------|------|------|
| 20 | `GET /api/items/:id` 返回 `contentMd5` | ✅ 已完成 | 通过 item 模型字段直接暴露 |
| 21 | `GET /api/items/:id` 返回 `currentRevision` | ✅ 已完成 | 通过 item 模型字段直接暴露 |
| 22 | `GET /api/items/:id` 返回 `currentVersionLabel` | ✅ 已完成 | 已统一返回 `vN` |
| 23 | `GET /api/items/:id/versions` 返回 `contentMd5` | ✅ 已完成 | 每个版本已返回内容摘要 |
| 24 | `GET /api/items/:id/versions` 返回 `versionLabel` | ✅ 已完成 | 已由 `revision` 派生 |
| 25 | `GET /api/items/:id/versions/:version` 返回 `versionLabel` | ✅ 已完成 | 详情接口已补充 |
| 26 | 新增 `BuildVersionLabel(revision)` 统一方法 | ✅ 已完成 | 已在 `ContentHashService` 提供 |
| 27 | 新增 `POST /api/items/:id/check-consistency` 接口 | ✅ 已完成 | 已支持按 `content` 或 `md5` 校验 |
| 28 | 一致性校验支持定位匹配 revision | ✅ 已完成 | 已返回 `matchedRevision` / `matchedVersionLabel` |
| 29 | OpenAPI / 注释文档补充版本化字段 | ✅ 已完成 | 已补 item/version 与 consistency 接口注释 |

### P2 接口兼容要求

1. 现有 `GET /api/items/:id/versions/:version` 的 `:version` 继续沿用 revision 数字，不做破坏性变更
2. 新增字段应尽量以向后兼容方式扩展返回体
3. 若一致性校验接口同时接收 `content` 与 `md5`，需明确定义优先级或直接返回 400

---

## P3 任务清单（存量数据迁移 + 稳定性验证）

| # | 任务 | 状态 | 说明 |
|---|------|------|------|
| 30 | 为存量 `CapabilityItem` 回填 `ContentMD5` | ✅ 已完成 | 已实现回填逻辑，单文件重算、archive 尽力基于 assets manifest 回填 |
| 31 | 为存量 `CapabilityVersion` 回填 `ContentMD5` | ✅ 已完成 | 已基于历史内容快照回填 |
| 32 | 为存量 `CapabilityItem` 回填 `CurrentRevision` | ✅ 已完成 | 已基于 `MAX(revision)` 或回退为 1 回填 |
| 33 | archive 存量数据回填策略落地 | ✅ 已完成 | 已采用“优先 assets manifest，缺失时尽力降级”策略 |
| 34 | 增加内容哈希单元测试 | ✅ 已完成 | 已覆盖文本、JSON、archive manifest |
| 35 | 增加创建/更新 handler 测试 | ✅ 已完成 | 已覆盖“内容不变不升版”“asset 改动升版” |
| 36 | 增加接口响应测试 | ✅ 已完成 | 已覆盖 `contentMd5` / `currentRevision` / `versionLabel` |
| 37 | 增加一致性校验接口测试 | ✅ 已完成 | 已覆盖命中当前版本、命中历史版本、非法输入 |
| 38 | 执行回归验证命令 | ✅ 已完成 | 已执行 `go test ./internal/services ./internal/handlers ./cmd/migrate` |

---

## 建议实施顺序

### 第一批（最小可交付）

1. 完成 P0-1 ~ P0-8
2. 完成 P1-9 ~ P1-17
3. 先实现“相同内容不升版”的核心规则

这一步完成后，系统已经具备：

- 单文件/多文件内容 MD5
- `Revision -> vN` 的稳定语义基础
- 相同内容不重复创建历史版本

### 第二批（可用性增强）

1. 完成 P2-20 ~ P2-29
2. 对外返回 `contentMd5` 与 `currentVersionLabel`
3. 增加一致性校验接口

### 第三批（上线前补齐）

1. 完成 P3-30 ~ P3-38
2. 回填存量数据
3. 补全测试与回归验证

---

## 风险提示

1. **archive 历史数据回填可能不完整**：若旧数据无法恢复完整 asset 集合，只能按降级策略回填
2. **JSON 规范化口径需要固定**：否则不同路径计算出的 MD5 可能不一致
3. **扫描/同步逻辑依赖 revision**：修改升版规则后，需要核对相关异步任务是否仍然基于正确 revision 执行
4. **字段兼容性**：前端或外部调用若直接消费 item/version 结构，新增字段虽是兼容式扩展，也需要联调验证

---

## 完成标准

满足以下条件即可认为版本化能力落地完成：

1. 新旧内容能通过 `ContentMD5` 判断是否一致
2. 相同内容重复提交不会生成新 `CapabilityVersion`
3. archive 多文件 skill 的版本判断包含主文件与资产文件
4. `GET /api/items/:id` 与版本接口可返回 `v1/v2/v3` 对应信息
5. 一致性校验接口可以定位到当前版本或某个历史版本
6. 相关测试通过，核心回归命令通过
