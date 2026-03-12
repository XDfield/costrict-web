# Capability 表重构 & 安全扫描 TodoList

基于审查报告与决策结果，覆盖数据模型、解析服务、同步服务、安全扫描四个方向。

## 决策基准

| 决策项 | 结论 |
|---|---|
| slug 唯一性 | `(registry_id, item_type, slug)` 三元唯一索引 |
| mcp 设计 | 多条，slug 从 `mcpServers.<key>` 推断（如 `mcp-github`） |
| hook | 暂不处理，维持现状 |
| 版本体系 | semver 为主；`CapabilityVersion.Version` 整数字段重命名为 `Revision` |
| Visibility | item 完全继承 registry，item.visibility 字段废弃 |
| Sync 高危阻断 | 先写入（`status=pending`），异步扫描，高危改为 `blocked` |
| CapabilityAsset | 保留，Sync 时写入 skill 目录下除 `SKILL.md` 之外的所有附属文件 |

---

## P0 — 数据模型变更

| # | 任务 | 状态 | 说明 |
|---|------|------|------|
| P0-1 | `CapabilityItem` 添加三元唯一索引 | ✅ 完成 | `gorm:"uniqueIndex:idx_item_slug,composite:registry_id,item_type,slug"` |
| P0-2 | `CapabilityItem.Visibility` 字段废弃 | ✅ 完成 | 字段已移除，读取时从 registry 继承 |
| P0-3 | `CapabilityVersion.Version` 重命名为 `Revision` | ✅ 完成 | 整数序号字段，column tag 同步改为 `revision` |
| P0-4 | 数据库迁移脚本（结构变更部分） | ✅ 完成 | AutoMigrate 自动处理，新增 SecurityScan 表 |

---

## P1 — ParserService 改造

| # | 任务 | 状态 | 说明 |
|---|------|------|------|
| P1-1 | `ParseSKILLMD` 补充返回完整 `Metadata` | ✅ 完成 | 确保 frontmatter 所有字段都在 `ParsedItem.Metadata` 中，不丢失 |
| P1-2 | `ParseMCPJSON` 改为返回多条 `[]*ParsedItem` | ✅ 完成 | 遍历 `mcpServers` 的每个 key，每条生成独立 item，slug=`mcp-{key}` |
| P1-3 | `InferItemType` 适配 mcp 多条场景 | ✅ 完成 | 单个 mcp server 的 itemType 统一为 `"mcp"` |
| P1-4 | `ParsedItem` 补充 `AssetPaths []string` 字段 | ✅ 完成 | 记录同目录下需要同步的附属文件相对路径，由 SyncService 填充 |

---

## P2 — SyncService 改造

| # | 任务 | 状态 | 说明 |
|---|------|------|------|
| P2-1 | `parseFile` 适配 mcp 多条返回 | ✅ 完成 | `.mcp.json` 解析结果从单条改为切片，主循环统一用切片遍历处理 |
| P2-2 | 同步写入 `item.Metadata` 字段 | ✅ 完成 | create/update item 时将 `parsed.Metadata` 序列化写入，不再丢失 |
| P2-3 | 同步写入 `CapabilityVersion.Metadata` | ✅ 完成 | 创建版本记录时写入 frontmatter metadata，不再硬编码 `{}` |
| P2-4 | item visibility 改为从 registry 继承 | ✅ 完成 | 移除所有 `item.Visibility = registry.Visibility` 赋值，item 表移除该字段 |
| P2-5 | `CapabilityAsset` 写入逻辑 | ✅ 完成 | syncAssets 方法实现，skill 目录附属文件自动同步 |
| P2-6 | `SyncLog.ErrorMessage` 改为记录全量错误 | ✅ 完成 | 将 `result.Errors` 全部 JSON 序列化存入 |

### P2-5 CapabilityAsset 写入逻辑说明

- Sync 解析到 skill 目录（如 `skills/my-skill/`）时，额外收集该目录下除 `SKILL.md` 之外的所有文件
- 每个附属文件写入一条 `CapabilityAsset`，`ItemID` 关联到主 item
- `RelPath` 存相对于 skill 目录的路径（如 `examples/demo.sh`）
- 文本文件写 `TextContent`，二进制文件写 `StorageKey`（存对象存储）
- 已存在且 `ContentSHA` 未变化的 asset 跳过写入
- item 归档（archived）时级联软删除关联 assets

---

## P3 — 数据一致性修复

| # | 任务 | 状态 | 说明 |
|---|------|------|------|
| P3-1 | `CapabilityArtifact.IsLatest` 唯一性保证 | ✅ 完成 | 上传新 artifact 时在事务内将旧记录 `IsLatest=false`，再插入新记录 |
| P3-2 | `CapabilityVersion.Revision` 并发安全 | ✅ 完成 | 改为事务内 MAX(revision)+1，避免并发重复序号 |
| P3-3 | `InstallCount` 写入逻辑 | ✅ 完成 | `DownloadItem` 中原子递增 `install_count + 1` |
| P3-4 | `SyncStatus` 崩溃恢复 | ✅ 完成 | 服务启动时重置所有 `sync_status='syncing'` 为 `error` |
| P3-5 | `CapabilityRegistry.LastSyncLogID` 补充 GORM 关联 | ✅ 完成 | 添加 `LastSyncLog *SyncLog` 关联字段，支持 Preload |

---

## P4 — 安全扫描服务（待设计完成后实施）

> 当前阶段暂缓，待安全扫描方案设计完成后补充任务细节。

数据模型预留字段（随 P0 一并加入，不阻塞其他阶段）：

| # | 任务 | 状态 | 说明 |
|---|------|------|------|
| P4-0 | `CapabilityItem` 新增 `SecurityStatus` 字段 | ✅ 完成（预留） | `gorm:"default:'unscanned'"` 枚举：`unscanned / pending / passed / blocked` |
| P4-0 | `CapabilityItem` 新增 `LastScanID` 字段 | ✅ 完成（预留） | `*string`，指向最新一次 `SecurityScan.ID` |
| P4-0 | 新增 `SecurityScan` 表 | ✅ 完成（预留） | 随 P0 AutoMigrate 一并创建 |

### `SecurityScan` 表结构（预留）

```go
type SecurityScan struct {
    ID          string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
    ItemID      string         `gorm:"not null;index" json:"itemId"`
    RevisionID  string         `gorm:"not null" json:"revisionId"`
    TriggerType string         `gorm:"not null" json:"triggerType"`     // auto | manual
    Status      string         `gorm:"not null;default:'pending'" json:"status"` // pending | running | passed | failed
    RiskLevel   string         `json:"riskLevel"`                       // low | medium | high | extreme
    Verdict     string         `json:"verdict"`                         // safe | caution | reject
    RedFlags    datatypes.JSON `gorm:"type:jsonb;default:'[]'" json:"redFlags"`
    Permissions datatypes.JSON `gorm:"type:jsonb;default:'{}'" json:"permissions"`
    Report      datatypes.JSON `gorm:"type:jsonb;default:'{}'" json:"report"`
    ErrorList   datatypes.JSON `gorm:"type:jsonb;default:'[]'" json:"errorList"`
    ScannedBy   string         `json:"scannedBy"`
    CreatedAt   time.Time      `json:"createdAt"`
    FinishedAt  *time.Time     `json:"finishedAt"`
}
```

---

## 总体进度

- **P0**（数据模型）：✅ 4/4
- **P1**（Parser）：✅ 4/4
- **P2**（Sync）：✅ 6/6
- **P3**（一致性修复）：✅ 5/5
- **P4**（安全扫描，待设计）：⏸ 暂缓（预留字段已完成）

**当前可执行合计：19 / 19 项完成**
