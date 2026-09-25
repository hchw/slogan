// Package config loads runtime configuration from environment variables and
// secret files. Secrets are never read from command-line arguments and are
// never logged.
package config

import (
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the runtime configuration shared by the API, Gateway and Worker
// processes. Not every field is used by every binary.
type Config struct {
	Env string

	APIAddr     string
	GatewayAddr string

	DatabaseURL string

	RedisAddr     string
	RedisPassword string
	RedisDB       int

	// ProviderSecretKey encrypts stored provider credentials (AES-256-GCM).
	ProviderSecretKey []byte

	SessionTTL time.Duration

	// Reservation safety caps.
	RequestMaxOutputTokens int
	MaxReservationMicro    int64

	Laya LayaConfig

	Evaluation EvaluationConfig

	ContentLogEnabled       bool
	ContentLogRetentionDays int
	LBMembershipURL         string
	LBMembershipToken       string
	ScoreAckTimeout         time.Duration
	// MetricsAddr is the internal scrape listener. Empty disables the
	// endpoint; MetricsToken is required before any scrape is served.
	MetricsAddr  string
	MetricsToken string

	RateLimits RateLimitConfig
}

// RateLimitConfig bounds gateway request rates (0 disables a dimension).
type RateLimitConfig struct {
	GlobalPerMin   int
	UserPerMin     int
	KeyPerMin      int
	ModelPerMin    int
	ProviderPerMin int
}

// LayaConfig configures the local Python classification sidecar client.
type LayaConfig struct {
	BaseURL      string
	APIKey       string
	Timeout      time.Duration
	Confidence   float64
	Enabled      bool
	MaxInputRune int
}

// EvaluationConfig bounds score refresh work.
type EvaluationConfig struct {
	MaxTokens          int
	Concurrency        int
	Timeout            time.Duration
	MinPublishRatio    float64
	MaxRefreshesPerDay int
}

// Load reads configuration, returning an error for missing required values.
func Load() (*Config, error) {
	cfg := &Config{
		Env:                     envOr("APP_ENV", "development"),
		APIAddr:                 envOr("API_ADDR", ":8080"),
		GatewayAddr:             envOr("GATEWAY_ADDR", ":8081"),
		DatabaseURL:             os.Getenv("DATABASE_URL"),
		RedisAddr:               envOr("REDIS_ADDR", "127.0.0.1:6379"),
		RedisPassword:           secret("REDIS_PASSWORD"),
		RedisDB:                 envInt("REDIS_DB", 0),
		SessionTTL:              envDuration("SESSION_TTL", 24*time.Hour),
		RequestMaxOutputTokens:  envInt("REQUEST_MAX_OUTPUT_TOKENS", 8192),
		MaxReservationMicro:     envInt64("MAX_RESERVATION_MICRO", 0),
		ContentLogEnabled:       envBool("CONTENT_LOG_ENABLED", false),
		ContentLogRetentionDays: envInt("CONTENT_LOG_RETENTION_DAYS", 7),
		LBMembershipURL:         envOr("LB_MEMBERSHIP_URL", ""),
		LBMembershipToken:       secret("LB_MEMBERSHIP_TOKEN"),
		ScoreAckTimeout:         envDuration("SCORE_ACK_TIMEOUT", 15*time.Second),
		MetricsAddr:             envOr("METRICS_ADDR", ""),
		MetricsToken:            secret("METRICS_TOKEN"),
		RateLimits: RateLimitConfig{
			GlobalPerMin:   envInt("RATE_LIMIT_GLOBAL_PER_MIN", 0),
			UserPerMin:     envInt("RATE_LIMIT_USER_PER_MIN", 600),
			KeyPerMin:      envInt("RATE_LIMIT_KEY_PER_MIN", 600),
			ModelPerMin:    envInt("RATE_LIMIT_MODEL_PER_MIN", 0),
			ProviderPerMin: envInt("RATE_LIMIT_PROVIDER_PER_MIN", 0),
		},
		Laya: LayaConfig{
			BaseURL:      envOr("LAYA_BASE_URL", ""),
			APIKey:       secret("LAYA_API_KEY"),
			Timeout:      envDuration("LAYA_TIMEOUT", 500*time.Millisecond),
			Confidence:   envFloat("LAYA_MIN_CONFIDENCE", 0.60),
			Enabled:      envBool("LAYA_ENABLED", false),
			MaxInputRune: envInt("LAYA_MAX_INPUT_RUNES", 4000),
		},
		Evaluation: EvaluationConfig{
			MaxTokens:          envInt("EVAL_MAX_TOKENS", 2000),
			Concurrency:        envInt("EVAL_CONCURRENCY", 5),
			Timeout:            envDuration("EVAL_TIMEOUT", 60*time.Second),
			MinPublishRatio:    envFloat("EVAL_MIN_PUBLISH_RATIO", 0.80),
			MaxRefreshesPerDay: envInt("EVAL_MAX_REFRESHES_PER_DAY", 1),
		},
	}

	keyHex := secret("PROVIDER_SECRET_KEY")
	if keyHex == "" {
		return nil, fmt.Errorf("config: PROVIDER_SECRET_KEY is required (32 bytes hex)")
	}
	key, err := hex.DecodeString(strings.TrimSpace(keyHex))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("config: PROVIDER_SECRET_KEY must be 32 bytes hex-encoded")
	}
	cfg.ProviderSecretKey = key

	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("config: DATABASE_URL is required")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	if c.SessionTTL <= 0 {
		return fmt.Errorf("config: SESSION_TTL must be positive")
	}
	if c.RequestMaxOutputTokens <= 0 {
		return fmt.Errorf("config: REQUEST_MAX_OUTPUT_TOKENS must be positive")
	}
	if c.Evaluation.MaxTokens <= 0 || c.Evaluation.Concurrency <= 0 || c.Evaluation.Timeout <= 0 {
		return fmt.Errorf("config: evaluation limits must be positive")
	}
	if c.Evaluation.MinPublishRatio <= 0 || c.Evaluation.MinPublishRatio > 1 {
		return fmt.Errorf("config: EVAL_MIN_PUBLISH_RATIO must be in (0,1]")
	}
	if c.Laya.Confidence < 0 || c.Laya.Confidence > 1 {
		return fmt.Errorf("config: LAYA_MIN_CONFIDENCE must be in [0,1]")
	}
	return nil
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// secret reads a value directly or from <KEY>_FILE. File values are trimmed.
func secret(key string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	if path := strings.TrimSpace(os.Getenv(key + "_FILE")); path != "" {
		b, err := os.ReadFile(path)
		if err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envInt64(key string, def int64) int64 {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
