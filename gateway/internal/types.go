package internal

const (
	HeartbeatInterval   = 30
	SendChannelCapacity = 64
)

type DeviceConnection struct {
	DeviceID     string
	Send         chan []byte
	Done         chan struct{}
	LastActivity int64
}

type Event struct {
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties,omitempty"`
}
