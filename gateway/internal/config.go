package internal

import (
	"os"
	"strconv"
)

type Config struct {
	GatewayID          string
	Port               string
	Endpoint           string
	InternalURL        string
	Region             string
	Capacity           int
	ServerURL          string
}

func LoadConfig() *Config {
	return &Config{
		GatewayID:   getEnv("GATEWAY_ID", "gw-01"),
		Port:        getEnv("GATEWAY_PORT", "8081"),
		Endpoint:    getEnv("GATEWAY_ENDPOINT", "http://localhost:8081"),
		InternalURL: getEnv("GATEWAY_INTERNAL_URL", "http://localhost:8081"),
		Region:      getEnv("GATEWAY_REGION", "default"),
		Capacity:    getEnvInt("GATEWAY_CAPACITY", 1000),
		ServerURL:   getEnv("SERVER_URL", "http://localhost:8080"),
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
