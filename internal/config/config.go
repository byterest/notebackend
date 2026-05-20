package config

import (
	"os"
	"strings"
	"time"
)

const defaultMaxUploadSize = 20 << 20

// S3Config holds S3-compatible storage settings.
type S3Config struct {
	Endpoint         string
	InternalEndpoint string
	AccessKeyID      string
	SecretAccessKey  string
	Bucket           string
	Region           string
	UseSSL           bool
}

// Config contains runtime settings loaded from environment variables.
type Config struct {
	Port           string
	FrontendOrigin string
	DatabaseURL    string
	UploadDir      string
	SessionTTL     time.Duration
	MaxUploadSize  int64
	S3             *S3Config
}

// Load reads configuration from environment variables and applies sane defaults.
func Load() Config {
	cfg := Config{
		Port:           envOrDefault("PORT", "8081"),
		FrontendOrigin: envOrDefault("FRONTEND_ORIGIN", "http://localhost:5174"),
		DatabaseURL:    envOrDefault("DATABASE_URL", "postgresql://postgres:123456@localhost:5438/note?sslmode=disable"),
		UploadDir:      envOrDefault("UPLOAD_DIR", "./uploads"),
		SessionTTL:     30 * 24 * time.Hour,
		MaxUploadSize:  defaultMaxUploadSize,
	}

	if endpoint := envOrDefault("S3_ENDPOINT", ""); endpoint != "" {
		cfg.S3 = &S3Config{
			Endpoint:         endpoint,
			InternalEndpoint: envOrDefault("S3_INTERNAL_ENDPOINT", ""),
			AccessKeyID:      envOrDefault("S3_ACCESS_KEY", ""),
			SecretAccessKey:  envOrDefault("S3_SECRET_KEY", ""),
			Bucket:           envOrDefault("S3_BUCKET", "notedev"),
			Region:           envOrDefault("S3_REGION", "us-east-1"),
			UseSSL:           envOrDefault("S3_USE_SSL", "true") == "true",
		}
	}

	return cfg
}

func envOrDefault(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}
