package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// CardStatusUpdater updates interactive card status after user action.
// Implemented by adapters that support interactive cards (e.g., WeComAdapter).
type CardStatusUpdater interface {
	UpdateCardStatus(responseCode, statusText, action string, cardData []byte, externalUserID string) error
}

type ChannelService struct {
	db             *gorm.DB
	adapters       map[string]ChannelAdapter
	messageHandler MessageHandler
	cloudBaseURL   string
	sessionStore   ReplyContextStore
	enabledTypes   map[string]bool
	// System-level availability controls (platform admin configuration)
	weComEnabled        bool
	weComWebhookEnabled bool
	weChatEnabled       bool
	weComBotEnabled     bool
	wecomBotQRCode      string
	actionHandler       ActionCallbackHandler
}

type ActionCallbackHandler func(ctx context.Context, action, token, responseCode, externalUserID string) error

func (s *ChannelService) SetActionHandler(h ActionCallbackHandler) {
	s.actionHandler = h
}

func (s *ChannelService) SetWeComBotQRCode(url string) {
	s.wecomBotQRCode = url
}

func (s *ChannelService) SetReplyContextStore(store ReplyContextStore) {
	s.sessionStore = store
}

func (s *ChannelService) SetMessageHandler(h MessageHandler) {
	s.messageHandler = h
}

func NewChannelService(db *gorm.DB, handler MessageHandler, cloudBaseURL string, enabledTypes []string, weComEnabled, weComWebhookEnabled, weChatEnabled, weComBotEnabled bool) *ChannelService {
	adapters := make(map[string]ChannelAdapter)
	// 只注册系统级别启用的适配器
	for k, a := range adapterRegistry {
		// 根据系统配置过滤适配器
		switch k {
		case "wecom":
			if !weComEnabled {
				continue
			}
		case "wecom-webhook":
			if !weComWebhookEnabled {
				continue
			}
		case "wechat":
			if !weChatEnabled {
				continue
			}
		case "wecom-bot":
			if !weComBotEnabled {
				continue
			}
		}
		adapters[k] = a
	}

	// 构建 enabledTypes map（保留向后兼容）
	enabledMap := make(map[string]bool)
	for _, t := range enabledTypes {
		enabledMap[t] = true
	}

	return &ChannelService{
		db:                  db,
		adapters:            adapters,
		messageHandler:      handler,
		cloudBaseURL:        cloudBaseURL,
		sessionStore:        NewReplyContextStore(),
		enabledTypes:        enabledMap,
		weComEnabled:        weComEnabled,
		weComWebhookEnabled: weComWebhookEnabled,
		weChatEnabled:       weChatEnabled,
		weComBotEnabled:     weComBotEnabled,
	}
}

func (s *ChannelService) HandleWebhook(channelType string, r *http.Request) (string, int, error) {
	adapter, ok := s.adapters[channelType]
	if !ok {
		return "", http.StatusBadRequest, fmt.Errorf("unsupported channel type: %s", channelType)
	}

	body, handled, err := adapter.HandleVerification(r, nil)
	if err == nil && handled {
		return body, http.StatusOK, nil
	}

	var configs []models.ChannelConfig
	if err := s.db.Where("channel_type = ? AND enabled = true AND deleted_at IS NULL", channelType).
		Find(&configs).Error; err != nil {
		return "", http.StatusInternalServerError, err
	}

	if len(configs) == 0 {
		// wecom-bot is system-level: auto-create config and process
		if channelType == "wecom-bot" && s.weComBotEnabled {
			sysCfg := models.ChannelConfig{
				ChannelType: "wecom-bot",
				Name:        "wecom-bot 系统",
				Enabled:     true,
			}
			if err := s.db.Create(&sysCfg).Error; err == nil {
				configs = append(configs, sysCfg)
			}
		}
		if len(configs) == 0 {
			return "success", http.StatusOK, nil
		}
	}

	msg, err := adapter.ParseInbound(r, nil)
	if err != nil || msg == nil {
		log.Printf("ChannelService: ParseInbound result: err=%v, msg=%+v", err, msg)
		return "success", http.StatusOK, nil
	}

	log.Printf("[inbound] externalUserID=%s chatID=%s chatType=%s content=%q contentType=%s",
		msg.ExternalUserID, msg.ExternalChatID, msg.ExternalChatType, msg.Content, msg.ContentType)

	// Handle interactive card callbacks
	if msg.ContentType == "action_callback" {
		action := msg.Content
		token, _ := msg.Metadata["actionToken"].(string)
		respCode, _ := msg.Metadata["responseCode"].(string)
		log.Printf("[channel] interactive card callback: action=%q, token=%q, responseCode=%q, fromUser=%s", action, token, respCode, msg.ExternalUserID)

		if s.actionHandler != nil {
			if err := s.actionHandler(r.Context(), action, token, respCode, msg.ExternalUserID); err != nil {
				log.Printf("[channel] action callback error: %v", err)
			}
		}


		return "success", http.StatusOK, nil
	}

	// For wecom-bot (system-level config with empty UserID), resolve the platform
	// user ID from the WeCom external user ID via idtrust identity binding.
	// This ensures inbound sessions use the same userID as outbound (device events).
	resolvedUserID := ""
	if len(configs) > 0 {
		resolvedUserID = configs[0].UserID
	}
	if resolvedUserID == "" && channelType == "wecom-bot" && msg.ExternalUserID != "" {
		var identity models.UserAuthIdentity
		if err := s.db.Where("provider_user_id = ? AND provider = ? AND deleted_at IS NULL",
			msg.ExternalUserID, "idtrust").First(&identity).Error; err != nil {
			log.Printf("[inbound] failed to resolve platformUserID from externalUserID=%s: %v",
				msg.ExternalUserID, err)
		} else {
			resolvedUserID = identity.UserSubjectID
			log.Printf("[inbound] resolved platformUserID=%s from externalUserID=%s (wecom-bot)",
				resolvedUserID, msg.ExternalUserID)
		}
	}

	if resolvedUserID != "" {
		log.Printf("[inbound] session key userID=%s externalUserID=%s", resolvedUserID, msg.ExternalUserID)
	}

	// wecom-bot first-contact detection: when a user's first inbound message
	// arrives, flip WebhookVerified to true so the UI can render the "bound"
	// state. The proxy reads the returned JSON and sends a welcome message.
	firstContact := false
	bound := false
	resolveErr := ""
	if channelType == "wecom-bot" {
		if msg.ExternalUserID != "" && resolvedUserID == "" {
			resolveErr = "未找到企微账号绑定信息，请先在 CoStrict 控制台完成企微账号绑定后再试"
		} else if resolvedUserID != "" {
			var userCfg models.ChannelConfig
			if err := s.db.Where("channel_type = ? AND user_id = ? AND deleted_at IS NULL", "wecom-bot", resolvedUserID).
				First(&userCfg).Error; err == nil {
				bound = true
				if !userCfg.WebhookVerified {
					firstContact = true
					if err := s.db.Model(&userCfg).Update("webhook_verified", true).Error; err != nil {
						log.Printf("[inbound] failed to set webhook_verified: %v", err)
					}
				}
			}
		}
	}

	for _, cfg := range configs {
		rc := ReplyContext{
			ChannelConfigID: cfg.ID,
			ChannelType:     cfg.ChannelType,
			UserID:          resolvedUserID,
			Target: ReplyTarget{
				ExternalChatID: msg.ExternalChatID,
				ExternalUserID: msg.ExternalUserID,
			},
			Metadata: msg.Metadata,
		}
		s.sessionStore.Record(rc)

		sender := NewAdapterSender(adapter, json.RawMessage(cfg.Config), rc)
		if err := s.messageHandler.Handle(r.Context(), msg, sender); err != nil {
			log.Printf("ChannelService: message handler error: %v", err)
		}

		now := time.Now()
		s.db.Model(&cfg).Updates(map[string]any{
			"last_active_at": now,
			"last_error":     "",
		})
	}

	if channelType == "wecom-bot" {
		resp := map[string]any{
			"success":      true,
			"firstContact": firstContact,
			"bound":        bound,
		}
		if firstContact {
			resp["welcome"] = "✅ 绑定成功！后续任务通知将通过此企微机器人推送给你。如需调整通知偏好，可在 CoStrict 控制台随时管理。"
		}
		if resolveErr != "" {
			resp["error"] = resolveErr
		}
		body, _ := json.Marshal(resp)
		return string(body), http.StatusOK, nil
	}

	return "success", http.StatusOK, nil
}

func (s *ChannelService) CreateConfig(userID string, channelType string, name string, config json.RawMessage) (*models.ChannelConfig, error) {
	adapter, ok := s.adapters[channelType]
	if !ok {
		return nil, fmt.Errorf("unsupported channel type: %s", channelType)
	}
	if err := adapter.ValidateConfig(config); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	ch := models.ChannelConfig{
		UserID:      userID,
		ChannelType: channelType,
		Name:        name,
		Enabled:     true,
		Config:      datatypes.JSON(config),
	}
	if err := s.db.Create(&ch).Error; err != nil {
		return nil, fmt.Errorf("failed to create channel config: %w", err)
	}
	return &ch, nil
}

func (s *ChannelService) UpdateConfig(userID, configID string, updates map[string]any) (*models.ChannelConfig, error) {
	var ch models.ChannelConfig
	if err := s.db.Where("id = ? AND user_id = ? AND deleted_at IS NULL", configID, userID).
		First(&ch).Error; err != nil {
		return nil, fmt.Errorf("channel config not found")
	}

	if rawConfig, ok := updates["config"]; ok {
		configBytes, _ := json.Marshal(rawConfig)
		adapter, ok := s.adapters[ch.ChannelType]
		if !ok {
			return nil, fmt.Errorf("unsupported channel type")
		}
		if err := adapter.ValidateConfig(configBytes); err != nil {
			return nil, fmt.Errorf("invalid config: %w", err)
		}
		updates["config"] = datatypes.JSON(configBytes)
	}

	if err := s.db.Model(&ch).Updates(updates).Error; err != nil {
		return nil, fmt.Errorf("failed to update channel config: %w", err)
	}
	s.db.Where("id = ?", configID).First(&ch)
	return &ch, nil
}

func (s *ChannelService) DeleteConfig(userID, configID string) error {
	var ch models.ChannelConfig
	if err := s.db.Where("id = ? AND user_id = ? AND deleted_at IS NULL", configID, userID).
		First(&ch).Error; err != nil {
		return fmt.Errorf("channel config not found")
	}
	return s.db.Delete(&ch).Error
}

func (s *ChannelService) GetConfig(userID, configID string) (*models.ChannelConfig, error) {
	var ch models.ChannelConfig
	if err := s.db.Where("id = ? AND user_id = ? AND deleted_at IS NULL", configID, userID).
		First(&ch).Error; err != nil {
		return nil, fmt.Errorf("channel config not found")
	}

	// 对企微应用渠道和 bot 渠道，动态组合IDTrust绑定信息
	if ch.ChannelType == "wecom" || ch.ChannelType == "wecom-bot" {
		ch = s.enrichWecomChannelConfig(userID, ch)
	}

	return &ch, nil
}

func (s *ChannelService) ListConfigs(userID string) ([]models.ChannelConfig, error) {
	var configs []models.ChannelConfig
	if err := s.db.Where("user_id = ? AND deleted_at IS NULL", userID).
		Order("created_at DESC").Find(&configs).Error; err != nil {
		return nil, err
	}

	// 检查用户是否有IDTrust绑定但没有企微应用channel，如果有则自动创建
	hasIDTrust := false
	var idtrustIdentity models.UserAuthIdentity
	if err := s.db.Where("user_subject_id = ? AND provider = ? AND deleted_at IS NULL", userID, "idtrust").
		First(&idtrustIdentity).Error; err == nil {
		hasIDTrust = true
	}

	hasWecomChannel := false
	for _, cfg := range configs {
		if cfg.ChannelType == "wecom" {
			hasWecomChannel = true
			break
		}
	}

	// 如果有IDTrust绑定但没有企微channel，自动创建
	if s.weComEnabled && hasIDTrust && !hasWecomChannel {
		autoCreatedChannel := models.ChannelConfig{
			UserID:      userID,
			ChannelType: "wecom",
			Name:        "企微应用通知",
			Enabled:     true,
			Config:      nil, // 留空，由查询时动态组合
		}
		if err := s.db.Create(&autoCreatedChannel).Error; err == nil {
			configs = append([]models.ChannelConfig{autoCreatedChannel}, configs...)
		}
	}

	// wecom-bot 长连接机器人：同样按 IDTrust 绑定自动创建 config，默认禁用
	if s.weComBotEnabled && hasIDTrust {
		hasWecomBotChannel := false
		for _, cfg := range configs {
			if cfg.ChannelType == "wecom-bot" {
				hasWecomBotChannel = true
				break
			}
		}
		if !hasWecomBotChannel {
			botChannel := models.ChannelConfig{
				UserID:      userID,
				ChannelType: "wecom-bot",
				Name:        "企微 bot",
				Enabled:     false,
			}
			if err := s.db.Create(&botChannel).Error; err == nil {
				configs = append([]models.ChannelConfig{botChannel}, configs...)
			}
		}
	}

	// 对企微应用渠道和 bot 渠道，动态组合IDTrust绑定信息
	for i := range configs {
		if configs[i].ChannelType == "wecom" || configs[i].ChannelType == "wecom-bot" {
			configs[i] = s.enrichWecomChannelConfig(userID, configs[i])
		}
	}

	// 仅返回系统层面启用的频道类型（过滤掉被 .env 禁用的类型）
	filtered := make([]models.ChannelConfig, 0, len(configs))
	for _, cfg := range configs {
		if _, ok := s.adapters[cfg.ChannelType]; ok {
			filtered = append(filtered, cfg)
		}
	}

	return filtered, nil
}

// enrichWecomChannelConfig 为企微应用渠道动态组合IDTrust绑定信息
func (s *ChannelService) enrichWecomChannelConfig(userID string, config models.ChannelConfig) models.ChannelConfig {
	// 从IDTrust绑定中获取userId
	var idtrustIdentity models.UserAuthIdentity
	if err := s.db.Where("user_subject_id = ? AND provider = ? AND deleted_at IS NULL", userID, "idtrust").
		First(&idtrustIdentity).Error; err != nil {
		// 无IDTrust绑定，标记为不可用
		config.Enabled = false
		return config
	}

	// 动态组合userId到config中
	configData := map[string]interface{}{}
	if len(config.Config) > 0 {
		if err := json.Unmarshal(config.Config, &configData); err != nil {
			configData = make(map[string]interface{})
		}
	}
	configData["userId"] = idtrustIdentity.ProviderUserID
	if config.ChannelType == "wecom-bot" && s.wecomBotQRCode != "" {
		configData["botQRCode"] = s.wecomBotQRCode
	}

	updatedConfig, _ := json.Marshal(configData)
	config.Config = datatypes.JSON(updatedConfig)
	return config
}

func (s *ChannelService) GetAvailableChannelTypes() []map[string]any {
	var result []map[string]any
	for _, adapter := range s.adapters {
		entry := map[string]any{
			"type":         adapter.Type(),
			"capabilities": adapter.Capabilities(),
			"schema":       adapter.ConfigSchema(),
		}
		result = append(result, entry)
	}
	return result
}

func (s *ChannelService) SendTestMessage(userID, configID string) error {
	var ch models.ChannelConfig
	if err := s.db.Where("id = ? AND user_id = ? AND deleted_at IS NULL", configID, userID).
		First(&ch).Error; err != nil {
		return fmt.Errorf("channel config not found")
	}

	if ch.ChannelType == "wechat" {
		return fmt.Errorf("wechat channel does not support test messages, please send a message to the bot in WeChat to verify")
	}

	adapter, ok := s.adapters[ch.ChannelType]
	if !ok {
		return fmt.Errorf("unsupported channel type")
	}

	// wecom / wecom-bot 的 config 在数据库中以空存储，由 enrichWecomChannelConfig 动态填充 userId
	if ch.ChannelType == "wecom" || ch.ChannelType == "wecom-bot" {
		ch = s.enrichWecomChannelConfig(userID, ch)
	}

	var userCfg struct {
		UserID string `json:"userId"`
	}
	if err := json.Unmarshal(ch.Config, &userCfg); err != nil || userCfg.UserID == "" {
		return fmt.Errorf("userId not configured in channel config")
	}

	msg := OutboundMessage{
		ContentType: "text",
		Content:     "[Test] This is a test message from CoStrict Cloud.",
	}
	target := ReplyTarget{ExternalChatID: userCfg.UserID, ExternalUserID: userCfg.UserID}
	return adapter.Reply(context.Background(), json.RawMessage(ch.Config), target, msg)
}

func (s *ChannelService) SendMessage(ctx context.Context, userID string, channelType string, externalUserID string, content string) error {
	contexts := s.sessionStore.LookupByUser(userID)
	var rc *ReplyContext
	for i := range contexts {
		if contexts[i].ChannelType == channelType && contexts[i].Target.ExternalUserID == externalUserID {
			rc = &contexts[i]
			break
		}
	}
	if rc == nil {
		return fmt.Errorf("no reply context found for user %s on %s", userID, channelType)
	}

	var ch models.ChannelConfig
	if err := s.db.Where("id = ? AND deleted_at IS NULL", rc.ChannelConfigID).First(&ch).Error; err != nil {
		return fmt.Errorf("channel config not found: %w", err)
	}

	adapter, ok := s.adapters[channelType]
	if !ok {
		return fmt.Errorf("unsupported channel type: %s", channelType)
	}

	return adapter.Reply(ctx, json.RawMessage(ch.Config), rc.Target, OutboundMessage{
		ContentType: "text",
		Content:     content,
	})
}

func (s *ChannelService) SessionStore() ReplyContextStore {
	return s.sessionStore
}

// CreateSenderForUser creates a Sender for a user on a specific channel type (e.g. "wecom-bot").
// It looks up the user's channel config and resolves the external user ID (e.g. WeChat Work userId).
// Returns an error if the channel type is not available or no config is found.
func (s *ChannelService) CreateSenderForUser(userID string, channelType string) (Sender, error) {
	adapter, ok := s.adapters[channelType]
	if !ok {
		return nil, fmt.Errorf("unsupported channel type: %s", channelType)
	}

	// For wecom/wecom-bot, resolve WeChat Work userId from IDTrust identity
	var externalUserID string
	if channelType == "wecom" || channelType == "wecom-bot" {
		var identity models.UserAuthIdentity
		if err := s.db.Where("user_subject_id = ? AND provider = ? AND deleted_at IS NULL", userID, "idtrust").
			First(&identity).Error; err != nil {
			return nil, fmt.Errorf("no idtrust identity for user %s", userID)
		}
		if identity.ProviderUserID != nil {
			externalUserID = *identity.ProviderUserID
		}
		if externalUserID == "" {
			return nil, fmt.Errorf("empty provider user id for user %s", userID)
		}
		log.Printf("[outbound] platformUserID=%s → externalUserID=%s (wecom-bot)", userID, externalUserID)
	}

	target := ReplyTarget{
		ExternalChatID:   externalUserID,
		ExternalUserID:   externalUserID,
		ExternalChatType: "single",
	}
	rc := ReplyContext{
		ChannelType: channelType,
		UserID:      userID,
		Target:      target,
	}
	// Use empty config — wecom-bot adapter reads webhook URL from its own config
	return NewAdapterSender(adapter, nil, rc), nil
}

func (s *ChannelService) GetQRCode(ctx context.Context, channelType string) (qrcodeID string, qrcodeImage string, err error) {
	adapter, ok := s.adapters[channelType]
	if !ok {
		return "", "", fmt.Errorf("unsupported channel type: %s", channelType)
	}
	provider, ok := adapter.(LoginProvider)
	if !ok {
		return "", "", fmt.Errorf("channel type %s does not support login flow", channelType)
	}
	return provider.GetQRCode(ctx)
}

func (s *ChannelService) GetLoginStatus(ctx context.Context, channelType string, qrcodeID string) (status string, token string, err error) {
	adapter, ok := s.adapters[channelType]
	if !ok {
		return "", "", fmt.Errorf("unsupported channel type: %s", channelType)
	}
	provider, ok := adapter.(LoginProvider)
	if !ok {
		return "", "", fmt.Errorf("channel type %s does not support login flow", channelType)
	}
	return provider.GetLoginStatus(ctx, qrcodeID)
}

func (s *ChannelService) GetWebhookURL(channelType string) string {
	if s.cloudBaseURL == "" {
		return ""
	}
	return s.cloudBaseURL + "/api/webhooks/channels/" + channelType
}

func maskConfig(config datatypes.JSON, sensitiveKeys []string) datatypes.JSON {
	if len(config) == 0 {
		return config
	}
	var m map[string]any
	if err := json.Unmarshal(config, &m); err != nil {
		return config
	}
	for _, key := range sensitiveKeys {
		if _, ok := m[key]; ok {
			m[key] = "***"
		}
	}
	masked, _ := json.Marshal(m)
	return datatypes.JSON(masked)
}

func getConfigWithMask(ch models.ChannelConfig) models.ChannelConfig {
	switch ch.ChannelType {
	case "wechat":
		ch.Config = maskConfig(ch.Config, []string{"token"})
	}
	return ch
}

func (s *ChannelService) UpdateInteractiveCard(channelType, responseCode, statusText, action string, cardData []byte, externalUserID string) {
	updater, ok := s.adapters[channelType].(CardStatusUpdater)
	if !ok {
		log.Printf("[channel] adapter %s does not support card status update", channelType)
		return
	}
	if err := updater.UpdateCardStatus(responseCode, statusText, action, cardData, externalUserID); err != nil {
		log.Printf("[channel] update card status failed: %v", err)
	}
}
