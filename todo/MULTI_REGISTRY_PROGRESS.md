# 组织多 Git 地址同步改造进度

## 总体进度

- **后端**：✅ 完成
- **前端**：✅ 完成

---

## 任务清单

| # | 任务 | 文件 | 状态 |
|---|------|------|------|
| 1 | `handlers.go` CreateOrganization 支持 `syncRegistries` 数组 | `internal/handlers/handlers.go` | ✅ |
| 2 | `handlers.go` 新增 ListOrgRegistries / AddOrgRegistry / UpdateOrgRegistry / RemoveOrgRegistry | `internal/handlers/handlers.go` | ✅ |
| 3 | `sync.go` TriggerOrgSync / CancelOrgSync / GetOrgSyncStatus / ListOrgSyncLogs 支持多 registry | `internal/handlers/sync.go` | ✅ |
| 4 | `cmd/api/main.go` 注册新路由 | `cmd/api/main.go` | ✅ |
| 5 | `api-client.ts` 新增 orgRegistryApi + 更新 syncApi 支持 registryId 参数 | `lib/api-client.ts` | ✅ |
| 6 | `org-sync-tab.tsx` 重构为多 registry 列表视图（卡片展开 + 独立同步/删除） | `components/org-sync-tab.tsx` | ✅ |
| 7 | `create-org-dialog.tsx` syncRegistry → syncRegistries 数组 | `components/create-org-dialog.tsx` | ✅ |
