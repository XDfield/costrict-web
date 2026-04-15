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

type ChannelService struct {
	db             *gorm.DB
	adapters       map[string]ChannelAdapter
	messageHandler MessageHandler
	cloudBaseURL   string
	sessionStore   *ReplyContextStore
	enabledTypes   map[string]bool
}

func NewChannelService(db *gorm.DB, handler MessageHandler, cloudBaseURL string, enabledTypes []string) *ChannelService {
	enabled := make(map[string]bool)
	if len(enabledTypes) > 0 {
		for _, t := range enabledTypes {
			enabled[t] = true
		}
	}
	adapters := make(map[string]ChannelAdapter)
	for k, a := range adapterRegistry {
		if len(enabled) == 0 || enabled[k] {
			adapters[k] = a
		}
	}
	return &ChannelService{
		db:             db,
		adapters:       adapters,
		messageHandler: handler,
		cloudBaseURL:   cloudBaseURL,
		sessionStore:   NewReplyContextStore(),
		enabledTypes:   enabled,
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
		return "success", http.StatusOK, nil
	}

	msg, err := adapter.ParseInbound(r, nil)
	if err != nil || msg == nil {
		log.Printf("ChannelService: ParseInbound result: err=%v, msg=%+v", err, msg)
		return "success", http.StatusOK, nil
	}

	log.Printf("ChannelService: received inbound message: chatID=%s, userID=%s, content=%q, contentType=%s", msg.ExternalChatID, msg.ExternalUserID, msg.Content, msg.ContentType)

	for _, cfg := range configs {
		rc := ReplyContext{
			ChannelConfigID: cfg.ID,
			ChannelType:     cfg.ChannelType,
			UserID:          cfg.UserID,
			Target: ReplyTarget{
				ExternalChatID: msg.ExternalChatID,
				ExternalUserID: msg.ExternalUserID,
			},
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
	return &ch, nil
}

func (s *ChannelService) ListConfigs(userID string) ([]models.ChannelConfig, error) {
	var configs []models.ChannelConfig
	if err := s.db.Where("user_id = ? AND deleted_at IS NULL", userID).
		Order("created_at DESC").Find(&configs).Error; err != nil {
		return nil, err
	}
	return configs, nil
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

func (s *ChannelService) SessionStore() *ReplyContextStore {
	return s.sessionStore
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
