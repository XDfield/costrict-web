package config

import (
	"log"
	"os"

	"github.com/spf13/viper"
)

type Config struct {
	Port       string
	DatabaseURL string
	Casdoor   CasdoorConfig
}

type CasdoorConfig struct {
	Endpoint   string
	ClientID   string
	Secret     string
	CallbackURL string
}

func Load() *Config {
	viper.SetConfigName(".env")
	viper.SetConfigType("env")
	viper.AddConfigPath(".")
	viper.AddConfigPath("..")
	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err != nil {
		log.Printf("Warning: .env file not found, using environment variables")
	}

	return &Config{
		Port:       getEnv("PORT", "8080"),
		DatabaseURL: getEnv("DATABASE_URL", "postgres://costrict:costrict_password@localhost:5432/costrict_db?sslmode=disable"),
		Casdoor: CasdoorConfig{
			Endpoint:   getEnv("CASDOOR_ENDPOINT", "http://localhost:8000"),
			ClientID:   getEnv("CASDOOR_CLIENT_ID", ""),
			Secret:     getEnv("CASDOOR_CLIENT_SECRET", ""),
			CallbackURL: getEnv("CASDOOR_CALLBACK_URL", "http://localhost:8080/api/auth/callback"),
		},
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
