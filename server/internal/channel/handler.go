package channel

import (
	"context"
)

type MessageHandler interface {
	Handle(ctx context.Context, msg *InboundMessage, sender Sender) error
}

// NoopMessageHandler does nothing when receiving messages
type NoopMessageHandler struct{}

func (h *NoopMessageHandler) Handle(_ context.Context, msg *InboundMessage, sender Sender) error {
	// No-op: message is logged but no automatic response is sent
	return nil
}
