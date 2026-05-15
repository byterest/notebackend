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
	DatabasePath   string
	UploadDir      string
	SessionTTL     time.Duration
	MaxUploadSize  int64
	AIBaseURL      string
	AIAPIKey       string
	AIModel        string
}

// Load reads configuration from environment variables and applies sane defaults.
func Load() Config {
	return Config{
		Port:           envOrDefault("PORT", "8081"),
		FrontendOrigin: envOrDefault("FRONTEND_ORIGIN", "http://localhost:5174"),
		DatabasePath:   envOrDefault("DATABASE_PATH", "./note.db"),
		UploadDir:      envOrDefault("UPLOAD_DIR", "./uploads"),
		SessionTTL:     30 * 24 * time.Hour,
		MaxUploadSize:  defaultMaxUploadSize,
		AIBaseURL:      envOrDefault("AI_BASE_URL", "https://api.openai.com/v1"),
		AIAPIKey:       envOrDefault("AI_API_KEY", ""),
		AIModel:        envOrDefault("AI_MODEL", "gpt-4o-mini"),
	}
}

func envOrDefault(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}
