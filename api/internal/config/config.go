// Package config loads runtime configuration from the environment.
package config

import (
	"fmt"
	"os"

	"github.com/joho/godotenv"
)

// Config holds all runtime configuration. Restaurant-specific values live in the
// settings table, never here — this is deployment/infra config only.
type Config struct {
	Port        string
	DatabaseURL string
	Env         string // dev | prod
}

// Load reads config from the environment. A local .env is loaded if present, but
// real environment variables always win.
func Load() (*Config, error) {
	_ = godotenv.Load()

	cfg := &Config{
		Port:        getenv("PORT", "8080"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		Env:         getenv("RUNTIME_ENV", "dev"),
	}
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	return cfg, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
