package channel

import (
	"context"
	"encoding/json"
	"net/http"
)

type ChannelAdapter interface {
	Type() string
	Capabilities() ChannelCapabilities
	ValidateConfig(config json.RawMessage) error
	ConfigSchema() []ConfigField
	ParseInbound(r *http.Request, config json.RawMessage) (*InboundMessage, error)
	HandleVerification(r *http.Request, config json.RawMessage) (body string, handled bool, err error)
	Reply(ctx context.Context, config json.RawMessage, target ReplyTarget, message OutboundMessage) error
}

type StartOptions struct {
	ConfigID string
}

type StartableChannel interface {
	ChannelAdapter
	Start(ctx context.Context, config json.RawMessage, handler InboundMessageHandler, opts StartOptions) error
}

type LoginProvider interface {
	ChannelAdapter
	GetQRCode(ctx context.Context) (qrcodeID string, qrcodeImage string, err error)
	GetLoginStatus(ctx context.Context, qrcodeID string) (status string, token string, err error)
}

var adapterRegistry = map[string]ChannelAdapter{}

func RegisterAdapter(a ChannelAdapter) {
	adapterRegistry[a.Type()] = a
}

func GetAdapter(channelType string) (ChannelAdapter, bool) {
	a, ok := adapterRegistry[channelType]
	return a, ok
}

func AllAdapters() map[string]ChannelAdapter {
	return adapterRegistry
}
