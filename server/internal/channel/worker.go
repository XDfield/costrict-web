package channel

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

type ChannelWorker struct {
	db             *gorm.DB
	adapters       map[string]ChannelAdapter
	messageHandler MessageHandler
	sessionStore   *ReplyContextStore
	pollers        map[string]context.CancelFunc
	configHashes   map[string]string
	mu             sync.Mutex
}

func NewChannelWorker(db *gorm.DB, handler MessageHandler, enabledTypes []string) *ChannelWorker {
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
	return &ChannelWorker{
		db:             db,
		adapters:       adapters,
		messageHandler: handler,
		sessionStore:   NewReplyContextStore(),
		pollers:        make(map[string]context.CancelFunc),
		configHashes:   make(map[string]string),
	}
}

func (w *ChannelWorker) Run(ctx context.Context) error {
	w.StartPollers(ctx)

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			w.StopAll()
			return ctx.Err()
		case <-ticker.C:
			w.refreshPollers(ctx)
		}
	}
}

func (w *ChannelWorker) StartPollers(ctx context.Context) {
	var configs []models.ChannelConfig
	if err := w.db.Where("enabled = true AND deleted_at IS NULL").Find(&configs).Error; err != nil {
		log.Printf("ChannelWorker: failed to load configs: %v", err)
		return
	}

	for _, cfg := range configs {
		w.configHashes[cfg.ID] = configSHA256(cfg.Config)
		w.maybeStartPoller(ctx, cfg)
	}
}

func (w *ChannelWorker) maybeStartPoller(ctx context.Context, cfg models.ChannelConfig) {
	adapter, ok := w.adapters[cfg.ChannelType]
	if !ok {
		return
	}

	startable, ok := adapter.(StartableChannel)
	if !ok {
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if _, exists := w.pollers[cfg.ID]; exists {
		return
	}

	pollerCtx, cancel := context.WithCancel(ctx)
	w.pollers[cfg.ID] = cancel

	handler := func(ctx context.Context, msg *InboundMessage, sender Sender) error {
		w.sessionStore.Record(sender.ReplyContext())
		return w.messageHandler.Handle(ctx, msg, sender)
	}

	go func() {
		log.Printf("ChannelWorker: starting poller for config %s (%s)", cfg.ID, cfg.ChannelType)
		opts := StartOptions{ConfigID: cfg.ID}
		if err := startable.Start(pollerCtx, json.RawMessage(cfg.Config), handler, opts); err != nil {
			log.Printf("ChannelWorker: poller %s stopped with error: %v", cfg.ID, err)
		}
	}()
}

func (w *ChannelWorker) stopPoller(configID string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if cancel, ok := w.pollers[configID]; ok {
		cancel()
		delete(w.pollers, configID)
		delete(w.configHashes, configID)
		log.Printf("ChannelWorker: stopped poller for config %s", configID)
	}
}

func (w *ChannelWorker) refreshPollers(ctx context.Context) {
	var configs []models.ChannelConfig
	if err := w.db.Where("deleted_at IS NULL").Find(&configs).Error; err != nil {
		return
	}

	activeIDs := make(map[string]bool)
	for _, cfg := range configs {
		activeIDs[cfg.ID] = true

		if !cfg.Enabled {
			w.stopPoller(cfg.ID)
			continue
		}

		configHash := configSHA256(cfg.Config)
		if prevHash, exists := w.configHashes[cfg.ID]; exists && prevHash != configHash {
			log.Printf("ChannelWorker: config changed for %s, restarting poller", cfg.ID)
			w.stopPoller(cfg.ID)
		}

		w.configHashes[cfg.ID] = configHash
		w.maybeStartPoller(ctx, cfg)
	}

	w.mu.Lock()
	for id := range w.pollers {
		if !activeIDs[id] {
			w.pollers[id]()
			delete(w.pollers, id)
			delete(w.configHashes, id)
			log.Printf("ChannelWorker: stopped stale poller %s", id)
		}
	}
	w.mu.Unlock()
}

func configSHA256(data []byte) string {
	h := sha256.Sum256(data)
	return string(h[:])
}

func (w *ChannelWorker) StopAll() {
	w.mu.Lock()
	defer w.mu.Unlock()

	for id, cancel := range w.pollers {
		cancel()
		delete(w.pollers, id)
		log.Printf("ChannelWorker: stopped poller %s on shutdown", id)
	}
}

func (w *ChannelWorker) SessionStore() *ReplyContextStore {
	return w.sessionStore
}
