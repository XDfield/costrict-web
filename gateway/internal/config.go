package internal

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	GatewayID      string      // 网关唯一标识符，用于在 API 服务中注册和识别
	Port           string      // 网关服务监听端口
	Endpoint       string      // 网关外部访问地址，客户端通过此地址建立 WebSocket 隧道连接
	APIBaseURL     string      // 本集群 Server 公网 API 地址，多集群部署时供 Web 前端路由设备请求；单集群可留空
	InternalURL    string      // 网关内部访问地址，API 服务通过此地址代理请求到设备
	Region         string      // 网关所属区域，用于就近分配和区域隔离
	Capacity       int         // 网关最大连接容量，超过容量后不再分配新设备
	ServerURL      string      // costrict-web-api 服务地址，用于注册和心跳
	InternalSecret string      // 与 Server 通信的共享密钥，用于内部接口认证
	Nacos          NacosConfig // Nacos 动态端点解析配置
}

// NacosConfig configures dynamic endpoint resolution via Nacos.
// When ServerAddr and DataID are non-empty, the gateway will fetch the
// endpoint from Nacos and prefer it over the GATEWAY_ENDPOINT env var.
type NacosConfig struct {
	ServerAddr       string // e.g. "nacos-headless.nacos.svc.cluster.local:8848"
	NamespaceID      string // empty for public namespace
	Group            string // defaults to "DEFAULT_GROUP"
	DataID           string // required to enable Nacos lookup
	APIBaseURLDataID string // optional: data ID for resolving APIBaseURL from Nacos
	TimeoutMs        uint64 // request timeout, defaults to 5000
	Username         string // optional Nacos auth username
	Password         string // optional Nacos auth password
	AccessToken      string // optional Nacos auth access token
}

func LoadConfig() *Config {
	// Load .env file in current directory (written by sync-env-gateway.js)
	loadEnvFile(".env")

	return &Config{
		GatewayID:      getEnv("GATEWAY_ID", "gw-01"),
		Port:           getEnv("GATEWAY_PORT", "8081"),
		Endpoint:       getEnv("GATEWAY_ENDPOINT", "http://localhost:8081"),
		APIBaseURL:     getEnv("GATEWAY_API_BASE_URL", ""),
		InternalURL:    getEnv("GATEWAY_INTERNAL_URL", "http://localhost:8081"),
		Region:         getEnv("GATEWAY_REGION", "default"),
		Capacity:       getEnvInt("GATEWAY_CAPACITY", 1000),
		ServerURL:      getEnv("SERVER_URL", "http://localhost:8080"),
		InternalSecret: getEnv("INTERNAL_SECRET", ""),
		Nacos: NacosConfig{
			ServerAddr:       getEnv("GATEWAY_NACOS_SERVER_ADDR", ""),
			NamespaceID:      getEnv("GATEWAY_NACOS_NAMESPACE_ID", ""),
			Group:            getEnv("GATEWAY_NACOS_GROUP", "DEFAULT_GROUP"),
			DataID:           getEnv("GATEWAY_NACOS_DATA_ID", ""),
			APIBaseURLDataID: getEnv("GATEWAY_NACOS_API_BASE_URL_DATA_ID", ""),
			TimeoutMs:        getEnvUint64("GATEWAY_NACOS_TIMEOUT_MS", 5000),
			Username:         getEnv("GATEWAY_NACOS_USERNAME", ""),
			Password:         getEnv("GATEWAY_NACOS_PASSWORD", ""),
			AccessToken:      getEnv("GATEWAY_NACOS_ACCESS_TOKEN", ""),
		},
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

func getEnvUint64(key string, defaultValue uint64) uint64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n
		}
	}
	return defaultValue
}
