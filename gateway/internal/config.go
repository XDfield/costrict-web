package internal

import (
	"os"
	"strconv"
)

type Config struct {
	GatewayID   string // 网关唯一标识符，用于在 API 服务中注册和识别
	Port        string // 网关服务监听端口
	Endpoint    string // 网关外部访问地址，客户端通过此地址建立 WebSocket 隧道连接
	InternalURL string // 网关内部访问地址，API 服务通过此地址代理请求到设备
	Region      string // 网关所属区域，用于就近分配和区域隔离
	Capacity    int    // 网关最大连接容量，超过容量后不再分配新设备
	ServerURL   string // costrict-web-api 服务地址，用于注册和心跳
}

func LoadConfig() *Config {
	return &Config{
		GatewayID:   getEnv("GATEWAY_ID", "gw-01"),                 // 网关唯一标识符
		Port:        getEnv("GATEWAY_PORT", "8081"),                // 网关监听端口
		Endpoint:    getEnv("GATEWAY_ENDPOINT", "http://localhost:8081"), // 外部访问地址，客户端连接此地址
		InternalURL: getEnv("GATEWAY_INTERNAL_URL", "http://localhost:8081"), // 内部访问地址，API 代理用
		Region:      getEnv("GATEWAY_REGION", "default"),          // 所属区域
		Capacity:    getEnvInt("GATEWAY_CAPACITY", 1000),          // 最大连接数
		ServerURL:   getEnv("SERVER_URL", "http://localhost:8080"), // API 服务地址
	}
}

func getEnv(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return defaultValue
}
