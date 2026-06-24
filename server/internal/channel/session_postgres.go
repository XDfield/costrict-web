package channel

import (
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// PostgresReplyContextStore persists reply contexts in PostgreSQL so that
// multiple API replicas can share webhook conversation state.
type PostgresReplyContextStore struct {
	db *gorm.DB
}

func NewPostgresReplyContextStore(db *gorm.DB) ReplyContextStore {
	return &PostgresReplyContextStore{db: db}
}

func (s *PostgresReplyContextStore) Record(rc ReplyContext) {
	ctx := models.ChannelReplyContext{
		ChannelConfigID: rc.ChannelConfigID,
		ExternalUserID:  rc.Target.ExternalUserID,
		UserID:          rc.UserID,
		ChannelType:     rc.ChannelType,
		ExternalChatID:  rc.Target.ExternalChatID,
		ContextToken:    rc.Target.ContextToken,
	}
	_ = s.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "channel_config_id"},
			{Name: "external_user_id"},
		},
		DoUpdates: clause.AssignmentColumns([]string{
			"user_id", "channel_type", "external_chat_id", "context_token", "updated_at",
		}),
	}).Create(&ctx).Error
}

func (s *PostgresReplyContextStore) Lookup(channelConfigID, externalUserID string) (ReplyContext, bool) {
	var ctx models.ChannelReplyContext
	err := s.db.Where("channel_config_id = ? AND external_user_id = ?", channelConfigID, externalUserID).First(&ctx).Error
	if err != nil {
		return ReplyContext{}, false
	}
	return toReplyContext(ctx), true
}

func (s *PostgresReplyContextStore) LookupByUser(userID string) []ReplyContext {
	var rows []models.ChannelReplyContext
	if err := s.db.Where("user_id = ?", userID).Find(&rows).Error; err != nil {
		return nil
	}
	out := make([]ReplyContext, 0, len(rows))
	for _, r := range rows {
		out = append(out, toReplyContext(r))
	}
	return out
}

func (s *PostgresReplyContextStore) Cleanup(maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge)
	_ = s.db.Where("updated_at < ?", cutoff).Delete(&models.ChannelReplyContext{}).Error
}

func toReplyContext(r models.ChannelReplyContext) ReplyContext {
	return ReplyContext{
		ChannelConfigID: r.ChannelConfigID,
		ChannelType:     r.ChannelType,
		UserID:          r.UserID,
		Target: ReplyTarget{
			ExternalChatID: r.ExternalChatID,
			ExternalUserID: r.ExternalUserID,
			ContextToken:   r.ContextToken,
		},
	}
}
