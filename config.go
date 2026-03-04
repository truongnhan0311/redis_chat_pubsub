package chat

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// LoadConfigFromEnv reads chat configuration from environment variables,
// optionally loading a .env file first.
//
// Variables read (all optional, with defaults):
//
//	CHAT_REDIS_ADDR      — Redis address             (default: localhost:6379)
//	CHAT_REDIS_PASSWORD  — Redis password             (default: "")
//	CHAT_REDIS_DB        — Redis DB index             (default: 0)
//	CHAT_API_KEYS        — Comma-separated API keys   (default: "" = disabled)
//	CHAT_WS_ADDR         — WebSocket server address   (default: :8080)
//	CHAT_HTTP_ADDR       — REST API server address    (default: :8081)
//	CHAT_LOG_LEVEL       — Log level: debug|info|warn|error (default: info)
//	CHAT_LOG_FORMAT      — Log format: text|json      (default: text)
//
// Typical usage:
//
//	cfg, err := chat.LoadConfigFromEnv(".env")   // or pass "" to skip .env
//	if err != nil { log.Fatal(err) }
//	module, err := chat.New(cfg)
func LoadConfigFromEnv(envFile string) (Config, error) {
	// Load .env file if a path is given and the file exists.
	if envFile != "" {
		if err := godotenv.Load(envFile); err != nil && !os.IsNotExist(err) {
			return Config{}, fmt.Errorf("loading env file %q: %w", envFile, err)
		}
	}

	redisDB := 0
	if raw := os.Getenv("CHAT_REDIS_DB"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid CHAT_REDIS_DB %q: %w", raw, err)
		}
		redisDB = v
	}

	redisAddr := os.Getenv("CHAT_REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	// Parse API keys (comma-separated).
	var apiKeys []string
	if raw := os.Getenv("CHAT_API_KEYS"); raw != "" {
		for _, k := range strings.Split(raw, ",") {
			if k = strings.TrimSpace(k); k != "" {
				apiKeys = append(apiKeys, k)
			}
		}
	}

	logger := NewSlogLogger(
		os.Getenv("CHAT_LOG_LEVEL"),
		os.Getenv("CHAT_LOG_FORMAT"),
	)

	// Server addresses.
	wsAddr := os.Getenv("CHAT_WS_ADDR")
	if wsAddr == "" {
		wsAddr = ":8080"
	}
	httpAddr := os.Getenv("CHAT_HTTP_ADDR")
	if httpAddr == "" {
		httpAddr = ":8081"
	}

	return Config{
		RedisAddr:     redisAddr,
		RedisPassword: os.Getenv("CHAT_REDIS_PASSWORD"),
		RedisDB:       redisDB,
		APIKeys:       apiKeys,
		WSAddr:        wsAddr,
		HTTPAddr:      httpAddr,
		Logger:        slog.NewLogLogger(logger.Handler(), slog.LevelInfo),
		SlogLogger:    logger,
		AllowedOriginFn: func(r *http.Request) bool {
			return true
		},
	}, nil
}

// NewSlogLogger creates a [*slog.Logger] from level/format strings.
//
//	level:  "debug" | "info" | "warn" | "error"  (default: info)
//	format: "json"  | "text"                      (default: text)
func NewSlogLogger(level, format string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: lvl}

	var handler slog.Handler
	if format == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}

	return slog.New(handler)
}
