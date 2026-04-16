package channel

import (
	"context"
	"fmt"
)

type MessageHandler interface {
	Handle(ctx context.Context, msg *InboundMessage, sender Sender) error
}

type EchoMessageHandler struct{}

func (h *EchoMessageHandler) Handle(_ context.Context, msg *InboundMessage, sender Sender) error {
	return sender.Send(context.Background(), fmt.Sprintf("[Echo] %s", msg.Content))
}
