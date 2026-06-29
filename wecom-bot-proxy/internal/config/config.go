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
	Bot      BotConfig      `yaml:"bot"`
	WecomAPI WecomAPIConfig `yaml:"wecom_api"`
	Server   ServerConfig   `yaml:"server"`
	Backend  BackendConfig  `yaml:"backend"`
	Dedup    DedupConfig    `yaml:"dedup"`
	Logging  LoggingConfig  `yaml:"logging"`
}

type BotConfig struct {
	BotID                   string        `yaml:"bot_id"`
	Secret                  string        `yaml:"secret"`
	WSURL                   string        `yaml:"ws_url"`
	HeartbeatInterval       time.Duration `yaml:"heartbeat_interval"`
	ReconnectInitialBackoff time.Duration `yaml:"reconnect_initial_backoff"`
	ReconnectMaxBackoff     time.Duration `yaml:"reconnect_max_backoff"`
	InputMaxLength          int           `yaml:"input_max_length"`
	// SessionLinkMode controls whether session_ref is rendered as a clickable
	// markdown link. "enabled" (default) appends the link; "restricted" ignores
	// session_ref entirely and forwards content verbatim.
	SessionLinkMode         string        `yaml:"session_link_mode"`
	// SessionTitleMaxLength caps the session_ref title length (in runes) when
	// rendered as link text, to avoid bloating the message bubble.
	SessionTitleMaxLength   int           `yaml:"session_title_max_length"`
}

// WecomAPIConfig holds credentials for calling WeCom server APIs
// (access-token + open_userid → userid conversion).
type WecomAPIConfig struct {
	CorpID      string `yaml:"corp_id"`
	AgentSecret string `yaml:"agent_secret"`
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
	if c.Bot.InputMaxLength == 0 {
		c.Bot.InputMaxLength = 120
	}
	if c.Bot.SessionLinkMode == "" {
		c.Bot.SessionLinkMode = "enabled"
	}
	if c.Bot.SessionTitleMaxLength == 0 {
		c.Bot.SessionTitleMaxLength = 50
	}
	if c.Server.Listen == "" {
		c.Server.Listen = ":9090"
	}
	if c.Backend.URL == "" {
		return fmt.Errorf("backend.url is required")
	}
	if c.Backend.AuthToken == "" {
		return fmt.Errorf("backend.auth_token is required")
	}
	if c.Backend.Timeout == 0 {
		c.Backend.Timeout = 10 * time.Second
	}
	if c.Backend.Retry == 0 {
		c.Backend.Retry = 3
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
	// wecom_api is optional; if corp_id is set, agent_secret is required
	if c.WecomAPI.CorpID != "" && c.WecomAPI.AgentSecret == "" {
		return fmt.Errorf("wecom_api.agent_secret is required when wecom_api.corp_id is set")
	}
	return nil
}

// ValidateAuthToken checks if the given token matches the configured backend auth token.
func (c *Config) ValidateAuthToken(token string) bool {
	if len(token) > 7 && token[:7] == "Bearer " {
		token = token[7:]
	}
	return c.Backend.AuthToken != "" && c.Backend.AuthToken == token
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
