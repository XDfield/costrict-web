package config

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/goccy/go-yaml"
)

type Config struct {
	Bot     BotConfig    `yaml:"bot"`
	Server  ServerConfig `yaml:"server"`
	Backends BackendsMap `yaml:"backends"`
	Routing RoutingConfig `yaml:"routing"`
	Dedup   DedupConfig  `yaml:"dedup"`
	Logging LoggingConfig `yaml:"logging"`
}

type BotConfig struct {
	BotID                  string        `yaml:"bot_id"`
	Secret                 string        `yaml:"secret"`
	WSURL                  string        `yaml:"ws_url"`
	HeartbeatInterval      time.Duration `yaml:"heartbeat_interval"`
	ReconnectInitialBackoff time.Duration `yaml:"reconnect_initial_backoff"`
	ReconnectMaxBackoff    time.Duration `yaml:"reconnect_max_backoff"`
}

type ServerConfig struct {
	Listen string `yaml:"listen"`
}

type BackendConfig struct {
	URL        string        `yaml:"url"`
	AuthToken  string        `yaml:"auth_token"`
	HMACSecret string        `yaml:"hmac_secret"`
	Timeout    time.Duration `yaml:"timeout"`
	Retry      int           `yaml:"retry"`
}

type BackendsMap map[string]BackendConfig

type RoutingConfig struct {
	DefaultBackend string        `yaml:"default_backend"`
	TaskRouteTTL   time.Duration `yaml:"task_route_ttl"`
}

type DedupConfig struct {
	Enabled    bool `yaml:"enabled"`
	MaxEntries int  `yaml:"max_entries"`
	TTLSeconds int  `yaml:"ttl_seconds"`
}

type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

var envPattern = regexp.MustCompile(`\$\{(\w+)(?::([^}]*))?\}`)

func expandEnvVars(s string) string {
	return envPattern.ReplaceAllStringFunc(s, func(match string) string {
		parts := envPattern.FindStringSubmatch(match)
		name := parts[1]
		defaultVal := ""
		if len(parts) > 2 {
			defaultVal = parts[2]
		}
		if val, ok := os.LookupEnv(name); ok {
			return val
		}
		return defaultVal
	})
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	expanded := expandEnvVars(string(data))

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func (c *Config) Validate() error {
	if c.Bot.BotID == "" {
		return fmt.Errorf("bot.bot_id is required")
	}
	if c.Bot.Secret == "" {
		return fmt.Errorf("bot.secret is required")
	}
	if c.Bot.WSURL == "" {
		c.Bot.WSURL = "wss://openws.work.weixin.qq.com"
	}
	if c.Bot.HeartbeatInterval == 0 {
		c.Bot.HeartbeatInterval = 30 * time.Second
	}
	if c.Bot.ReconnectInitialBackoff == 0 {
		c.Bot.ReconnectInitialBackoff = 5 * time.Second
	}
	if c.Bot.ReconnectMaxBackoff == 0 {
		c.Bot.ReconnectMaxBackoff = 60 * time.Second
	}
	if c.Server.Listen == "" {
		c.Server.Listen = ":9090"
	}
	if len(c.Backends) == 0 {
		return fmt.Errorf("at least one backend is required")
	}
	for name, b := range c.Backends {
		if b.URL == "" {
			return fmt.Errorf("backend %q: url is required", name)
		}
		if b.AuthToken == "" {
			return fmt.Errorf("backend %q: auth_token is required", name)
		}
		if b.Timeout == 0 {
			b.Timeout = 10 * time.Second
			c.Backends[name] = b
		}
		if b.Retry == 0 {
			b.Retry = 3
			c.Backends[name] = b
		}
	}
	if c.Routing.DefaultBackend == "" {
		return fmt.Errorf("routing.default_backend is required")
	}
	if _, ok := c.Backends[c.Routing.DefaultBackend]; !ok {
		return fmt.Errorf("routing.default_backend %q not found in backends", c.Routing.DefaultBackend)
	}
	if c.Routing.TaskRouteTTL == 0 {
		c.Routing.TaskRouteTTL = 24 * time.Hour
	}
	if c.Dedup.MaxEntries == 0 {
		c.Dedup.MaxEntries = 10000
	}
	if c.Dedup.TTLSeconds == 0 {
		c.Dedup.TTLSeconds = 300
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Logging.Format == "" {
		c.Logging.Format = "json"
	}
	return nil
}

func (c *Config) BackendNames() []string {
	names := make([]string, 0, len(c.Backends))
	for name := range c.Backends {
		names = append(names, name)
	}
	return names
}

// FindBackendByToken finds the backend name that matches the given auth token.
// Used for authentication + caller identity in one step.
func (c *Config) FindBackendByToken(token string) string {
	// Strip "Bearer " prefix if present
	if len(token) > 7 && token[:7] == "Bearer " {
		token = token[7:]
	}
	for name, b := range c.Backends {
		if b.AuthToken == token {
			return name
		}
	}
	return ""
}

// GetEnvOrDefault returns the environment variable value or a default.
func GetEnvOrDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

// GetEnvIntOrDefault returns the environment variable as int or a default.
func GetEnvIntOrDefault(key string, defaultVal int) int {
	if val := os.Getenv(key); val != "" {
		if n, err := strconv.Atoi(val); err == nil {
			return n
		}
	}
	return defaultVal
}
