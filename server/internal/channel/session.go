package channel

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

type sessionKey struct {
	ChannelConfigID string
	ExternalUserID  string
}

type storedContext struct {
	ReplyContext
	UpdatedAt time.Time
}

type ReplyContextStore struct {
	mu       sync.RWMutex
	sessions map[sessionKey]storedContext
}

func NewReplyContextStore() *ReplyContextStore {
	return &ReplyContextStore{
		sessions: make(map[sessionKey]storedContext),
	}
}

func (s *ReplyContextStore) Record(rc ReplyContext) {
	key := sessionKey{
		ChannelConfigID: rc.ChannelConfigID,
		ExternalUserID:  rc.Target.ExternalUserID,
	}
	s.mu.Lock()
	s.sessions[key] = storedContext{ReplyContext: rc, UpdatedAt: time.Now()}
	s.mu.Unlock()
}

func (s *ReplyContextStore) Lookup(channelConfigID, externalUserID string) (ReplyContext, bool) {
	key := sessionKey{
		ChannelConfigID: channelConfigID,
		ExternalUserID:  externalUserID,
	}
	s.mu.RLock()
	sc, ok := s.sessions[key]
	s.mu.RUnlock()
	return sc.ReplyContext, ok
}

func (s *ReplyContextStore) LookupByUser(userID string) []ReplyContext {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var results []ReplyContext
	for _, sc := range s.sessions {
		if sc.UserID == userID {
			results = append(results, sc.ReplyContext)
		}
	}
	return results
}

func (s *ReplyContextStore) Cleanup(maxAge time.Duration) {
	s.mu.Lock()
	cutoff := time.Now().Add(-maxAge)
	for k, v := range s.sessions {
		if v.UpdatedAt.Before(cutoff) {
			delete(s.sessions, k)
		}
	}
	s.mu.Unlock()
}

type adapterSender struct {
	adapter     ChannelAdapter
	config      json.RawMessage
	replyCtx    ReplyContext
}

func NewAdapterSender(adapter ChannelAdapter, config json.RawMessage, rc ReplyContext) Sender {
	return &adapterSender{adapter: adapter, config: config, replyCtx: rc}
}

func (s *adapterSender) Send(ctx context.Context, content string) error {
	return s.SendMessage(ctx, OutboundMessage{ContentType: "text", Content: content})
}

func (s *adapterSender) SendMessage(ctx context.Context, msg OutboundMessage) error {
	return s.adapter.Reply(ctx, s.config, s.replyCtx.Target, msg)
}

func (s *adapterSender) ReplyContext() ReplyContext {
	return s.replyCtx
}
