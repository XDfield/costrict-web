package services

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/costrict/costrict-web/server/internal/llm"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

var allowedScanCategories = []string{
	"frontend-development",
	"backend-development",
	"system-architecture",
	"requirements-analysis",
	"system-design",
	"data-processing",
	"software-testing",
	"tdd-development",
	"information-security",
	"command-execution",
	"tool-invocation",
	"deployment-operations",
	"document-editing",
	"meeting-management",
	"task-scheduling",
	"file-management",
	"team-collaboration",
}

const scanSystemPrompt = `你是一个专业的 AI 能力项安全审查员。
你需要对用户提交的 AI Agent Skill / MCP Server 配置进行安全审查。

## 分类要求

你还需要为该能力项选择一个最匹配的分类，只能从以下 slug 中选择一个：
- frontend-development（前端开发）
- backend-development（后端开发）
- system-architecture（系统架构）
- requirements-analysis（需求分析）
- system-design（系统设计）
- data-processing（数据处理）
- software-testing（软件测试）
- tdd-development（TDD 开发）
- information-security（信息安全）
- command-execution（命令执行）
- tool-invocation（工具调用）
- deployment-operations（部署运维）
- document-editing（文档编辑）
- meeting-management（会议管理）
- task-scheduling（任务调度）
- file-management（文件管理）
- team-collaboration（团队协作）

## 审查维度

### 红线行为（出现任意一条 → risk_level=extreme）
- 向未知外部 URL 发送数据（curl/wget/fetch 到非文档域名）
- 读取凭证文件（~/.ssh、~/.aws、~/.config、.env 等）
- 请求用户提供 API Key / Token / 密码并外传
- 使用 base64/eval/exec 处理外部输入
- 修改系统文件（/etc、/usr、注册表等）
- 访问浏览器 Cookie 或会话数据
- 使用混淆/压缩/编码的代码片段

### 高风险行为（risk_level=high）
- 网络请求到 IP 地址而非域名
- 安装未列明的软件包
- 请求 sudo / 管理员权限
- 读取工作区外的文件系统路径

### 中风险行为（risk_level=medium）
- 需要网络访问但目标域名可信
- 需要读写本地文件（工作区内）
- 使用环境变量传递敏感配置（本身合理，需确认是否外传）

### 低风险行为（risk_level=low）
- 纯文本处理、格式化、注释生成
- 访问公开 API（天气、汇率等）
- 本地计算，无网络无文件操作

## 输出格式

严格输出以下 JSON，不要添加任何额外文字：

{
  "category": "从固定分类 slug 列表中选择一个",
  "risk_level": "clean | low | medium | high | extreme",
  "verdict": "safe | caution | reject",
  "red_flags": ["具体描述发现的红线行为，引用原文"],
  "permissions": {
    "files": ["列出需要访问的文件路径"],
    "network": ["列出需要访问的域名或 IP"],
    "commands": ["列出执行的系统命令"]
  },
  "summary": "100字以内的风险摘要，中文",
  "recommendations": ["具体的修改建议"]
}

verdict 规则：
- risk_level=clean/low → verdict=safe
- risk_level=medium    → verdict=caution
- risk_level=high/extreme → verdict=reject`

const scanUserPromptTemplate = `请对以下 AI 能力项进行安全审查：

## 基本信息
- 名称：%s
- 类型：%s
- 来源：%s
- 描述：%s

## 配置信息（metadata）
%s

## 完整内容
%s

请输出 JSON 格式的审查报告。`

const maxInputRunes = 6000

type ScanReport struct {
	Category        string      `json:"category"`
	RiskLevel       string      `json:"risk_level"`
	Verdict         string      `json:"verdict"`
	RedFlags        []string    `json:"red_flags"`
	Permissions     Permissions `json:"permissions"`
	Summary         string      `json:"summary"`
	Recommendations []string    `json:"recommendations"`
}

type Permissions struct {
	Files    []string `json:"files"`
	Network  []string `json:"network"`
	Commands []string `json:"commands"`
}

type ScanService struct {
	DB          *gorm.DB
	LLMClient   *llm.Client
	ModelName   string
	CategorySvc *CategoryService
}

func (s *ScanService) ScanItem(ctx context.Context, itemID string, itemRevision int, triggerType string) (*models.SecurityScan, error) {
	var item models.CapabilityItem
	if err := s.DB.First(&item, "id = ?", itemID).Error; err != nil {
		return nil, fmt.Errorf("item not found: %w", err)
	}

	s.DB.Model(&item).Updates(map[string]any{"security_status": "scanning"})

	startTime := time.Now()

	metaStr := "{}"
	if len(item.Metadata) > 0 {
		metaStr = string(item.Metadata)
	}

	content := truncateContent(item.Content, maxInputRunes)
	userPrompt := fmt.Sprintf(scanUserPromptTemplate,
		item.Name,
		item.ItemType,
		item.SourcePath,
		item.Description,
		metaStr,
		content,
	)

	report, rawOutput, err := s.callLLMWithRetry(ctx, userPrompt)
	durationMs := time.Since(startTime).Milliseconds()

	scanRecord := &models.SecurityScan{
		ID:           uuid.New().String(),
		ItemID:       itemID,
		ItemRevision: itemRevision,
		TriggerType:  triggerType,
		ScanModel:    s.ModelName,
		Category:     reportCategoryValue(report),
		DurationMs:   durationMs,
		RawOutput:    rawOutput,
	}

	now := time.Now()
	scanRecord.FinishedAt = &now

	if err != nil {
		scanRecord.RiskLevel = ""
		scanRecord.Verdict = ""
		scanRecord.Summary = fmt.Sprintf("扫描失败：%v", err)
		scanRecord.RedFlags = datatypes.JSON([]byte("[]"))
		scanRecord.Permissions = datatypes.JSON([]byte("{}"))
		scanRecord.Recommendations = datatypes.JSON([]byte("[]"))

		if dbErr := s.DB.Create(scanRecord).Error; dbErr != nil {
			return nil, dbErr
		}
		s.DB.Model(&item).Updates(map[string]any{
			"security_status": "error",
			"last_scan_id":    scanRecord.ID,
		})
		return scanRecord, err
	}

	redFlagsJSON, _ := json.Marshal(report.RedFlags)
	permsJSON, _ := json.Marshal(report.Permissions)
	recsJSON, _ := json.Marshal(report.Recommendations)

	scanRecord.RiskLevel = report.RiskLevel
	scanRecord.Verdict = report.Verdict
	scanRecord.Summary = report.Summary
	scanRecord.RedFlags = datatypes.JSON(redFlagsJSON)
	scanRecord.Permissions = datatypes.JSON(permsJSON)
	scanRecord.Recommendations = datatypes.JSON(recsJSON)

	if dbErr := s.DB.Create(scanRecord).Error; dbErr != nil {
		return nil, dbErr
	}

	itemUpdates := map[string]any{
		"security_status": report.RiskLevel,
		"last_scan_id":    scanRecord.ID,
	}
	if scanRecord.Category != "" {
		itemUpdates["category"] = scanRecord.Category
	}
	s.DB.Model(&item).Updates(itemUpdates)
	if scanRecord.Category != "" && s.CategorySvc != nil {
		_, _ = s.CategorySvc.EnsureCategory(scanRecord.Category, "scan")
	}

	return scanRecord, nil
}

func (s *ScanService) callLLMWithRetry(ctx context.Context, userPrompt string) (*ScanReport, string, error) {
	report, raw, err := s.callLLM(ctx, userPrompt)
	if err == nil {
		return report, raw, nil
	}

	retryPrompt := userPrompt + "\n\n注意：请只输出 JSON，不要有其他内容。"
	report, raw, err = s.callLLM(ctx, retryPrompt)
	return report, raw, err
}

func (s *ScanService) callLLM(ctx context.Context, userPrompt string) (*ScanReport, string, error) {
	_ = ctx
	raw, err := s.LLMClient.ChatSimple(scanSystemPrompt, userPrompt)
	if err != nil {
		return nil, "", fmt.Errorf("LLM call failed: %w", err)
	}

	cleaned := extractJSON(raw)
	var report ScanReport
	if err := json.Unmarshal([]byte(cleaned), &report); err != nil {
		return nil, raw, fmt.Errorf("failed to parse LLM output as JSON: %w", err)
	}

	if !isValidRiskLevel(report.RiskLevel) {
		return nil, raw, fmt.Errorf("invalid risk_level in LLM output: %q", report.RiskLevel)
	}
	if !isValidVerdict(report.Verdict) {
		return nil, raw, fmt.Errorf("invalid verdict in LLM output: %q", report.Verdict)
	}
	if !isValidScanCategory(report.Category) {
		return nil, raw, fmt.Errorf("invalid category in LLM output: %q", report.Category)
	}

	return &report, raw, nil
}

func reportCategoryValue(report *ScanReport) string {
	if report == nil {
		return ""
	}
	return strings.TrimSpace(report.Category)
}

func extractJSON(s string) string {
	// Strip markdown code fences (```json ... ``` or ``` ... ```)
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if idx := strings.Index(s, "\n"); idx != -1 {
			s = s[idx+1:]
		}
		if idx := strings.LastIndex(s, "```"); idx != -1 {
			s = s[:idx]
		}
		s = strings.TrimSpace(s)
	}

	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start == -1 || end == -1 || end <= start {
		return s
	}
	return s[start : end+1]
}

func isValidRiskLevel(v string) bool {
	switch v {
	case "clean", "low", "medium", "high", "extreme":
		return true
	}
	return false
}

func isValidVerdict(v string) bool {
	switch v {
	case "safe", "caution", "reject":
		return true
	}
	return false
}

func isValidScanCategory(v string) bool {
	v = strings.TrimSpace(v)
	for _, category := range allowedScanCategories {
		if v == category {
			return true
		}
	}
	return false
}

func truncateContent(content string, maxRunes int) string {
	if utf8.RuneCountInString(content) <= maxRunes {
		return content
	}
	runes := []rune(content)
	return string(runes[:maxRunes]) + "\n\n[内容已截断，扫描结果仅供参考]"
}
