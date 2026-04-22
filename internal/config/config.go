package config

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultAppHost                        = "0.0.0.0"
	defaultAppPort                        = 8080
	defaultLogLevel                       = "INFO"
	defaultEnvironment                    = "production"
	defaultAlertTemplatePath              = "templates/telegram_alert.tmpl"
	defaultMessageParseMode               = "HTML"
	defaultBasicAuthRealm                 = "webhook-telegram-proxy"
	defaultHTTPReadTimeout                = 5 * time.Second
	defaultHTTPWriteTimeout               = 10 * time.Second
	defaultHTTPShutdownTimeout            = 10 * time.Second
	defaultHTTPIdleTimeout                = 60 * time.Second
	defaultMaxRequestBodyBytes      int64 = 1 << 20
	defaultMaxHeaderBytes                 = 1 << 20
	defaultMaxDeliveryAttempts            = 5
	defaultRetryBaseDelay                 = 2 * time.Second
	defaultRetryMaxDelay                  = 2 * time.Minute
	defaultRecoveryInterval               = 15 * time.Second
	defaultWorkerBatchSize                = 32
	defaultStorePath                      = "data/webhook-telegram-proxy.db"
	defaultStoreMaxSizeBytes        int64 = 100 * 1024 * 1024
	defaultStoreHighWatermark       int64 = 80 * 1024 * 1024
	defaultStoreLowWatermark        int64 = 60 * 1024 * 1024
	defaultRotationInterval               = 1 * time.Minute
	defaultDeliveredRetentionHours        = 168
	defaultDeadLetterRetentionHours       = 720
)

type Config struct {
	AppHost                    string
	AppPort                    int
	LogLevel                   string
	Environment                string
	TelegramBotToken           string
	TelegramChatID             string
	TelegramBaseURL            string
	TelegramProxyURL           string
	WebhookSecret              string
	BasicAuthUsername          string
	BasicAuthPassword          string
	BasicAuthRealm             string
	AlertTemplatePath          string
	MessageParseMode           string
	HTTPReadTimeout            time.Duration
	HTTPWriteTimeout           time.Duration
	HTTPShutdownTimeout        time.Duration
	HTTPIdleTimeout            time.Duration
	MaxRequestBodyBytes        int64
	MaxHeaderBytes             int
	MaxDeliveryAttempts        int
	RetryBaseDelay             time.Duration
	RetryMaxDelay              time.Duration
	RecoveryInterval           time.Duration
	WorkerBatchSize            int
	StorePath                  string
	StoreMaxSizeBytes          int64
	StoreRotationEnabled       bool
	StoreRotationHighWatermark int64
	StoreRotationLowWatermark  int64
	StoreRetentionDelivered    time.Duration
	StoreRetentionDeadLetter   time.Duration
	StoreRotationInterval      time.Duration
	StoreVacuumAfterRotation   bool
}

func Parse(args []string) (Config, error) {
	fs := flag.NewFlagSet("webhook-telegram-proxy", flag.ContinueOnError)

	cfg := Config{}

	fs.StringVar(&cfg.AppHost, "app-host", envString("APP_HOST", defaultAppHost), "HTTP bind host")
	fs.IntVar(&cfg.AppPort, "app-port", envInt("APP_PORT", defaultAppPort), "HTTP bind port")
	fs.StringVar(&cfg.LogLevel, "log-level", envString("LOG_LEVEL", defaultLogLevel), "log level")
	fs.StringVar(&cfg.Environment, "environment", envString("ENVIRONMENT", defaultEnvironment), "environment name")
	fs.StringVar(&cfg.TelegramBotToken, "telegram-bot-token", envString("TELEGRAM_BOT_TOKEN", ""), "Telegram bot token")
	fs.StringVar(&cfg.TelegramChatID, "telegram-chat-id", envString("TELEGRAM_CHAT_ID", ""), "Telegram chat or channel id")
	fs.StringVar(&cfg.TelegramBaseURL, "telegram-base-url", envString("TELEGRAM_BASE_URL", ""), "override Telegram Bot API base URL")
	fs.StringVar(&cfg.TelegramProxyURL, "telegram-proxy-url", envString("TELEGRAM_PROXY_URL", ""), "explicit proxy URL for Telegram Bot API requests")
	fs.StringVar(&cfg.WebhookSecret, "webhook-secret", envString("WEBHOOK_SECRET", ""), "shared secret for webhook authentication")
	fs.StringVar(&cfg.BasicAuthUsername, "basic-auth-username", envString("BASIC_AUTH_USERNAME", ""), "optional HTTP basic auth username")
	fs.StringVar(&cfg.BasicAuthPassword, "basic-auth-password", envString("BASIC_AUTH_PASSWORD", ""), "optional HTTP basic auth password")
	fs.StringVar(&cfg.BasicAuthRealm, "basic-auth-realm", envString("BASIC_AUTH_REALM", defaultBasicAuthRealm), "HTTP basic auth realm")
	fs.StringVar(&cfg.AlertTemplatePath, "alert-template-path", envString("ALERT_TEMPLATE_PATH", defaultAlertTemplatePath), "path to Telegram alert template")
	fs.StringVar(&cfg.MessageParseMode, "message-parse-mode", envString("MESSAGE_PARSE_MODE", defaultMessageParseMode), "Telegram parse mode")
	fs.DurationVar(&cfg.HTTPReadTimeout, "http-read-timeout", envDuration("HTTP_READ_TIMEOUT", defaultHTTPReadTimeout), "HTTP read timeout")
	fs.DurationVar(&cfg.HTTPWriteTimeout, "http-write-timeout", envDuration("HTTP_WRITE_TIMEOUT", defaultHTTPWriteTimeout), "HTTP write timeout")
	fs.DurationVar(&cfg.HTTPShutdownTimeout, "http-shutdown-timeout", envDuration("HTTP_SHUTDOWN_TIMEOUT", defaultHTTPShutdownTimeout), "HTTP shutdown timeout")
	fs.DurationVar(&cfg.HTTPIdleTimeout, "http-idle-timeout", envDuration("HTTP_IDLE_TIMEOUT", defaultHTTPIdleTimeout), "HTTP idle timeout")
	fs.Int64Var(&cfg.MaxRequestBodyBytes, "max-request-body-bytes", envInt64("MAX_REQUEST_BODY_BYTES", defaultMaxRequestBodyBytes), "max webhook request body size in bytes")
	fs.IntVar(&cfg.MaxHeaderBytes, "max-header-bytes", envInt("MAX_HEADER_BYTES", defaultMaxHeaderBytes), "maximum HTTP header size in bytes")
	fs.IntVar(&cfg.MaxDeliveryAttempts, "max-delivery-attempts", envInt("MAX_DELIVERY_ATTEMPTS", defaultMaxDeliveryAttempts), "maximum delivery attempts")
	fs.DurationVar(&cfg.RetryBaseDelay, "retry-base-delay", envDuration("RETRY_BASE_DELAY", defaultRetryBaseDelay), "base retry delay")
	fs.DurationVar(&cfg.RetryMaxDelay, "retry-max-delay", envDuration("RETRY_MAX_DELAY", defaultRetryMaxDelay), "max retry delay")
	fs.DurationVar(&cfg.RecoveryInterval, "recovery-interval", envDuration("RECOVERY_INTERVAL", defaultRecoveryInterval), "interval for recovery worker")
	fs.IntVar(&cfg.WorkerBatchSize, "worker-batch-size", envInt("WORKER_BATCH_SIZE", defaultWorkerBatchSize), "number of deliveries processed in one recovery tick")
	fs.StringVar(&cfg.StorePath, "store-path", envString("STORE_PATH", defaultStorePath), "SQLite store path")
	fs.Int64Var(&cfg.StoreMaxSizeBytes, "store-max-size-bytes", envInt64("STORE_MAX_SIZE_BYTES", defaultStoreMaxSizeBytes), "maximum store size in bytes")
	fs.BoolVar(&cfg.StoreRotationEnabled, "store-rotation-enabled", envBool("STORE_ROTATION_ENABLED", true), "enable store rotation")
	fs.Int64Var(&cfg.StoreRotationHighWatermark, "store-rotation-high-watermark-bytes", envInt64("STORE_ROTATION_HIGH_WATERMARK_BYTES", defaultStoreHighWatermark), "rotation high watermark in bytes")
	fs.Int64Var(&cfg.StoreRotationLowWatermark, "store-rotation-low-watermark-bytes", envInt64("STORE_ROTATION_LOW_WATERMARK_BYTES", defaultStoreLowWatermark), "rotation low watermark in bytes")
	fs.DurationVar(&cfg.StoreRotationInterval, "store-rotation-interval", envDuration("STORE_ROTATION_INTERVAL", defaultRotationInterval), "store rotation interval")
	fs.BoolVar(&cfg.StoreVacuumAfterRotation, "store-vacuum-after-rotation", envBool("STORE_VACUUM_AFTER_ROTATION", false), "run VACUUM after rotation")

	deliveredRetentionHours := fs.Int("store-retention-delivered-hours", envInt("STORE_RETENTION_DELIVERED_HOURS", defaultDeliveredRetentionHours), "retention for delivered records in hours")
	deadLetterRetentionHours := fs.Int("store-retention-dead-letter-hours", envInt("STORE_RETENTION_DEAD_LETTER_HOURS", defaultDeadLetterRetentionHours), "retention for dead-letter and failed records in hours")

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	cfg.StoreRetentionDelivered = time.Duration(*deliveredRetentionHours) * time.Hour
	cfg.StoreRetentionDeadLetter = time.Duration(*deadLetterRetentionHours) * time.Hour

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func (c Config) Validate() error {
	var problems []string

	if c.AppHost == "" {
		problems = append(problems, "app host is required")
	}
	if c.AppPort <= 0 || c.AppPort > 65535 {
		problems = append(problems, "app port must be between 1 and 65535")
	}
	if c.TelegramBotToken == "" {
		problems = append(problems, "telegram bot token is required")
	}
	if c.TelegramChatID == "" {
		problems = append(problems, "telegram chat id is required")
	}
	if err := validateTelegramBaseURL(c.TelegramBaseURL); err != nil {
		problems = append(problems, err.Error())
	}
	if err := validateTelegramProxyURL(c.TelegramProxyURL); err != nil {
		problems = append(problems, err.Error())
	}
	if c.AlertTemplatePath == "" {
		problems = append(problems, "alert template path is required")
	}
	if (c.BasicAuthUsername == "") != (c.BasicAuthPassword == "") {
		problems = append(problems, "basic auth username and password must be configured together")
	}
	if c.BasicAuthEnabled() && c.BasicAuthRealm == "" {
		problems = append(problems, "basic auth realm must not be empty when basic auth is enabled")
	}
	if strings.ToUpper(c.MessageParseMode) != defaultMessageParseMode {
		problems = append(problems, "only HTML parse mode is supported in v1")
	}
	if c.MaxRequestBodyBytes <= 0 {
		problems = append(problems, "max request body bytes must be positive")
	}
	if c.MaxHeaderBytes <= 0 {
		problems = append(problems, "max header bytes must be positive")
	}
	if c.HTTPReadTimeout <= 0 {
		problems = append(problems, "http read timeout must be positive")
	}
	if c.HTTPWriteTimeout <= 0 {
		problems = append(problems, "http write timeout must be positive")
	}
	if c.HTTPShutdownTimeout <= 0 {
		problems = append(problems, "http shutdown timeout must be positive")
	}
	if c.HTTPIdleTimeout <= 0 {
		problems = append(problems, "http idle timeout must be positive")
	}
	if c.MaxDeliveryAttempts < 1 {
		problems = append(problems, "max delivery attempts must be positive")
	}
	if c.RetryBaseDelay <= 0 || c.RetryMaxDelay <= 0 || c.RetryBaseDelay > c.RetryMaxDelay {
		problems = append(problems, "retry delays must be positive and base must not exceed max")
	}
	if c.RecoveryInterval <= 0 {
		problems = append(problems, "recovery interval must be positive")
	}
	if c.WorkerBatchSize <= 0 {
		problems = append(problems, "worker batch size must be positive")
	}
	if c.StorePath == "" {
		problems = append(problems, "store path is required")
	}
	if c.StoreMaxSizeBytes <= 0 {
		problems = append(problems, "store max size must be positive")
	}
	if c.StoreRotationHighWatermark <= 0 || c.StoreRotationHighWatermark > c.StoreMaxSizeBytes {
		problems = append(problems, "store rotation high watermark must be positive and not exceed store max size")
	}
	if c.StoreRotationLowWatermark <= 0 || c.StoreRotationLowWatermark >= c.StoreRotationHighWatermark {
		problems = append(problems, "store rotation low watermark must be positive and below high watermark")
	}
	if c.StoreRetentionDelivered <= 0 || c.StoreRetentionDeadLetter <= 0 {
		problems = append(problems, "store retention windows must be positive")
	}
	if c.StoreRotationInterval <= 0 {
		problems = append(problems, "store rotation interval must be positive")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}

	return nil
}

func (c Config) Address() string {
	return fmt.Sprintf("%s:%d", c.AppHost, c.AppPort)
}

func (c Config) BasicAuthEnabled() bool {
	return c.BasicAuthUsername != "" && c.BasicAuthPassword != ""
}

func envString(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return strings.TrimSpace(value)
	}
	return fallback
}

func envInt(key string, fallback int) int {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback
	}
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return fallback
	}
	return value
}

func envInt64(key string, fallback int64) int64 {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback
	}
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return fallback
	}
	return value
}

func envBool(key string, fallback bool) bool {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback
	}
	value, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return fallback
	}
	return value
}

func envDuration(key string, fallback time.Duration) time.Duration {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback
	}
	value, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return fallback
	}
	return value
}

func validateTelegramBaseURL(rawURL string) error {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse telegram base URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("telegram base URL scheme %q is not supported", parsed.Scheme)
	}
	if parsed.Host == "" {
		return errors.New("telegram base URL must include a host")
	}
	if parsed.User != nil {
		return errors.New("telegram base URL must not contain credentials")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("telegram base URL must not contain query parameters or fragments")
	}

	return nil
}

func validateTelegramProxyURL(rawURL string) error {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse telegram proxy URL: %w", err)
	}
	if parsed.Host == "" {
		return errors.New("telegram proxy URL must include a host")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("telegram proxy URL must not contain query parameters or fragments")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return errors.New("telegram proxy URL must not contain a path")
	}

	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "socks5":
		return nil
	case "mtproto":
		return errors.New("mtproto proxies are not supported for the HTTP Telegram Bot API client; use socks5/http(s), a local Bot API server, or network-level egress instead")
	default:
		return fmt.Errorf("telegram proxy URL scheme %q is not supported", parsed.Scheme)
	}
}
