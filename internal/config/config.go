package config

import (
	"os"
	"strings"
	"time"
)

const defaultMaxUploadSize = 20 << 20

// Config contains runtime settings loaded from environment variables.
type Config struct {
	Port           string
	FrontendOrigin string
	DatabaseURL    string
	UploadDir      string
	SessionTTL     time.Duration
	MaxUploadSize  int64
}

// Load reads configuration from environment variables and applies sane defaults.
func Load() Config {
	return Config{
		Port:           envOrDefault("PORT", "8081"),
		FrontendOrigin: envOrDefault("FRONTEND_ORIGIN", "http://localhost:5174"),
		DatabaseURL:    envOrDefault("DATABASE_URL", "postgresql://postgres:123456@localhost:5438/note?sslmode=disable"),
		UploadDir:      envOrDefault("UPLOAD_DIR", "./uploads"),
		SessionTTL:     30 * 24 * time.Hour,
		MaxUploadSize:  defaultMaxUploadSize,
	}
}

func envOrDefault(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}
