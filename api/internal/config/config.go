// Package config loads runtime configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/joho/godotenv"
)

// Config holds all runtime configuration. Restaurant-specific values live in the
// settings table, never here — this is deployment/infra config only.
type Config struct {
	Port        string
	DatabaseURL string
	Env         string // dev | prod
	// CORSOrigins is the browser origins allowed to call the API. Must be an
	// explicit list (not "*") because auth rides on session cookies, and the
	// CORS spec forbids a wildcard origin with credentials.
	CORSOrigins []string
	// CookieCrossSite sends session/device cookies as SameSite=None; Secure so a
	// web app on another site (e.g. Vercel) can log in. Set COOKIE_SAMESITE=none
	// (requires HTTPS). Default is Lax, for same-origin hosting and local dev.
	CookieCrossSite bool
}

// Load reads config from the environment. A local .env is loaded if present, but
// real environment variables always win.
func Load() (*Config, error) {
	_ = godotenv.Load()

	cfg := &Config{
		Port:            getenv("PORT", "8080"),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		Env:             getenv("RUNTIME_ENV", "dev"),
		CORSOrigins:     splitList(getenv("CORS_ORIGINS", "http://localhost:5173")),
		CookieCrossSite: strings.EqualFold(os.Getenv("COOKIE_SAMESITE"), "none"),
	}
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	return cfg, nil
}

// splitList parses a comma-separated env value into trimmed, non-empty entries.
func splitList(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
