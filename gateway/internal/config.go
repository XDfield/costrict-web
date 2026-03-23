package internal

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	GatewayID      string // 网关唯一标识符，用于在 API 服务中注册和识别
	Port           string // 网关服务监听端口
	Endpoint       string // 网关外部访问地址，客户端通过此地址建立 WebSocket 隧道连接
	InternalURL    string // 网关内部访问地址，API 服务通过此地址代理请求到设备
	Region         string // 网关所属区域，用于就近分配和区域隔离
	Capacity       int    // 网关最大连接容量，超过容量后不再分配新设备
	ServerURL      string // costrict-web-api 服务地址，用于注册和心跳
	InternalSecret string // 与 Server 通信的共享密钥，用于内部接口认证
}

func LoadConfig() *Config {
	// Load .env file in current directory (written by sync-env-gateway.js)
	loadEnvFile(".env")

	return &Config{
		GatewayID:      getEnv("GATEWAY_ID", "gw-01"),
		Port:           getEnv("GATEWAY_PORT", "8081"),
		Endpoint:       getEnv("GATEWAY_ENDPOINT", "http://localhost:8081"),
		InternalURL:    getEnv("GATEWAY_INTERNAL_URL", "http://localhost:8081"),
		Region:         getEnv("GATEWAY_REGION", "default"),
		Capacity:       getEnvInt("GATEWAY_CAPACITY", 1000),
		ServerURL:      getEnv("SERVER_URL", "http://localhost:8080"),
		InternalSecret: getEnv("INTERNAL_SECRET", ""),
	}
}

// loadEnvFile reads a .env file and sets environment variables (does not override existing ones).
func loadEnvFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, "=")
		if idx == -1 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		value := strings.TrimSpace(line[idx+1:])
		// Do not override existing environment variables
		if os.Getenv(key) == "" {
			os.Setenv(key, value)
		}
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
