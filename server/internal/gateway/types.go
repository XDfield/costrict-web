package gateway

type GatewayInfo struct {
	ID            string
	Endpoint      string
	InternalURL   string
	Region        string
	Capacity      int
	CurrentConns  int
	LastHeartbeat int64
}

type DeviceAllocation struct {
	GatewayID  string `json:"gatewayID"`
	GatewayURL string `json:"gatewayURL"`
}

const (
	GatewayHeartbeatTimeoutMs = 60_000
	GatewayCleanupIntervalMs  = 10_000
)
