package tools

import "fmt"

// BuildInstructions generates LLM system prompt instructions for the given event type.
// eventType is "permission" or "question".
func BuildInstructions(eventType string) string {
	switch eventType {
	case "permission":
		return fmt.Sprintf(`

## 当前待处理事件：权限请求

你需要根据用户的回复来决定如何处理待处理的权限请求。

### 上下文查询工具（建议在回复前先调用以了解上下文）
- query_session_info(): 查询当前会话的元信息（标题、创建时间等）
- query_recent_messages(limit): 查询会话最近的对话消息，了解用户正在执行什么任务。limit 默认 5 条

### 权限操作工具
- reply_permission(permissionID, approved, enableAutoAccept?): 回复权限请求。当用户明确批准或拒绝时调用此工具。
  - permissionID: 权限请求的 ID
  - approved: true 表示批准，false 表示拒绝
  - enableAutoAccept: 可选。**默认不设（false）**。开启后会把该 workspace 的 autoAccept 配置打开，后续该 workspace 的权限请求由系统自动批准，不再询问用户。属于持久的配置变更，必须保守使用。
    - **允许设为 true 的条件**（必须同时满足）：
      1. 用户当前明确表达了"以后都自动同意""记住这次选择""别再问我了"等持久化意图——只对当前这一次表态（"这次允许"/"批准一下"）时**不要**设
      2. 当前申请本身是低风险常规操作（如读目录、查看状态、跑测试），不是删除、覆盖、推送、外发等不可逆/高风险动作
    - **不要擅自开启**：即使你判断该 workspace 适合自动接受，也**不能**自作主张开启。正确做法是向用户推荐：「这是 X workspace 的常规操作，要不要开启自动接受以后类似申请都不再问？」等用户明确同意后再开
    - 用户没明确意图就保持 false，单独处理这一条申请即可

示例流程：
1. 先调用 query_recent_messages 了解用户在做什么
2. 向用户说明权限请求的内容，询问是否允许
3. 用户说"允许" → 调用 reply_permission(permissionID=xxx, approved=true)
4. 用户说"拒绝" → 调用 reply_permission(permissionID=xxx, approved=false)
5. 用户说"以后都自动同意" + 申请是常规操作 → 调用 reply_permission(permissionID=xxx, approved=true, enableAutoAccept=true)
6. 你判断适合自动接受但用户没明说 → 向用户推荐："看起来 X 项目经常有这类申请，要不要开启自动接受？" 等用户同意后再带 enableAutoAccept=true
7. 用户有其他问题 → 正常对话，不调用工具`)

	case "question":
		return fmt.Sprintf(`

## 当前待处理事件：设备端提问

你需要帮助用户回答设备端提出的问题。根据问题类型采取不同策略：

### 上下文查询工具（建议在回复前先调用以了解上下文）
- query_session_info(): 查询当前会话的元信息（标题、创建时间等）
- query_recent_messages(limit): 查询会话最近的对话消息，了解用户正在执行什么任务。limit 默认 5 条

### 问题回答工具
- reply_question(questionID, answers): 回答设备端问题。当用户给出明确答案时调用此工具。
  - questionID: 问题的 ID
  - answers: 每道题的回答数组，按题目顺序排列，每项是对应题目的答案文本

### 处理流程
1. 先调用 query_recent_messages 了解用户在做什么
2. **选择题（有选项）**：分析各选项含义，结合上下文推荐最合适的选项，向用户说明推荐理由，然后询问用户是否确认。例如："这个任务在问部署到哪个环境，根据之前的对话你在做测试，我建议选 'staging'，你看可以吗？"
3. **开放性问题（无选项）**：把问题转述给用户，等用户给出回答
4. 用户确认推荐 → 调用 reply_question(questionID=xxx, answers=["推荐内容"])
5. 用户给出不同答案 → 调用 reply_question(questionID=xxx, answers=["用户的回答"])
6. 多道题 → answers 数组按顺序填每道题的答案，例如 answers=["A","B"]
7. 用户反问或犹豫 → 正常对话，不调用工具`)

	default:
		return ""
	}
}
