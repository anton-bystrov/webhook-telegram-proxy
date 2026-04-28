package config

import (
	"os"
	"testing"
)

func TestFlagsOverrideEnv(t *testing.T) {
	t.Setenv("APP_PORT", "8080")
	t.Setenv("TELEGRAM_BOT_TOKEN", "env-token")
	t.Setenv("TELEGRAM_CHAT_ID", "env-chat")
	t.Setenv("ALERT_TEMPLATE_PATH", "templates/telegram_alert.tmpl")
	t.Setenv("STORE_PATH", "data/env.db")

	cfg, err := Parse([]string{
		"--app-port", "9090",
		"--telegram-bot-token", "flag-token",
		"--telegram-chat-id", "flag-chat",
		"--alert-template-path", "templates/telegram_alert.tmpl",
		"--store-path", "data/flag.db",
	})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if cfg.AppPort != 9090 {
		t.Fatalf("expected app port 9090, got %d", cfg.AppPort)
	}
	if cfg.TelegramBotToken != "flag-token" {
		t.Fatalf("expected token from flags, got %q", cfg.TelegramBotToken)
	}
	if cfg.StorePath != "data/flag.db" {
		t.Fatalf("expected store path from flags, got %q", cfg.StorePath)
	}
}

func TestValidateRequiresTelegramSettings(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	t.Setenv("TELEGRAM_CHAT_ID", "")
	t.Setenv("ALERT_TEMPLATE_PATH", "templates/telegram_alert.tmpl")
	t.Setenv("STORE_PATH", "data/test.db")

	_, err := Parse([]string{})
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestValidateRejectsPartialBasicAuthConfig(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "token")
	t.Setenv("TELEGRAM_CHAT_ID", "chat")
	t.Setenv("ALERT_TEMPLATE_PATH", "templates/telegram_alert.tmpl")
	t.Setenv("STORE_PATH", "data/test.db")
	t.Setenv("BASIC_AUTH_USERNAME", "user")
	t.Setenv("BASIC_AUTH_PASSWORD", "")

	_, err := Parse([]string{})
	if err == nil {
		t.Fatal("expected validation error for partial basic auth config")
	}
}

func TestValidateRejectsNonPositiveHTTPTimeouts(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "token")
	t.Setenv("TELEGRAM_CHAT_ID", "chat")
	t.Setenv("ALERT_TEMPLATE_PATH", "templates/telegram_alert.tmpl")
	t.Setenv("STORE_PATH", "data/test.db")
	t.Setenv("HTTP_WRITE_TIMEOUT", "0s")

	_, err := Parse([]string{})
	if err == nil {
		t.Fatal("expected validation error for non-positive HTTP timeout")
	}
}

func TestParseAcceptsTelegramProxyAndBaseURL(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "token")
	t.Setenv("TELEGRAM_CHAT_ID", "chat")
	t.Setenv("ALERT_TEMPLATE_PATH", "templates/telegram_alert.tmpl")
	t.Setenv("STORE_PATH", "data/test.db")

	cfg, err := Parse([]string{
		"--telegram-base-url", "https://botapi.internal.example",
		"--telegram-proxy-url", "socks5://user:pass@proxy.internal.example:1080",
	})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if cfg.TelegramBaseURL != "https://botapi.internal.example" {
		t.Fatalf("expected base URL from flags, got %q", cfg.TelegramBaseURL)
	}
	if cfg.TelegramProxyURL != "socks5://user:pass@proxy.internal.example:1080" {
		t.Fatalf("expected proxy URL from flags, got %q", cfg.TelegramProxyURL)
	}
}

func TestParseAcceptsAlertmanagerMessageSourceWithoutTemplatePath(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "token")
	t.Setenv("TELEGRAM_CHAT_ID", "chat")
	t.Setenv("ALERT_MESSAGE_SOURCE", "alertmanager")
	t.Setenv("ALERT_TEMPLATE_PATH", "")
	t.Setenv("STORE_PATH", "data/test.db")

	cfg, err := Parse([]string{})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.AlertMessageSource != "alertmanager" {
		t.Fatalf("expected alertmanager message source, got %q", cfg.AlertMessageSource)
	}
	if cfg.UsesTemplateRenderer() {
		t.Fatal("expected alertmanager mode to skip template renderer")
	}
}

func TestValidateRejectsUnsupportedAlertMessageSource(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "token")
	t.Setenv("TELEGRAM_CHAT_ID", "chat")
	t.Setenv("ALERT_MESSAGE_SOURCE", "custom")
	t.Setenv("ALERT_TEMPLATE_PATH", "templates/telegram_alert.tmpl")
	t.Setenv("STORE_PATH", "data/test.db")

	_, err := Parse([]string{})
	if err == nil {
		t.Fatal("expected validation error for unsupported alert message source")
	}
}

func TestValidateRejectsUnsupportedTelegramProxyScheme(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "token")
	t.Setenv("TELEGRAM_CHAT_ID", "chat")
	t.Setenv("ALERT_TEMPLATE_PATH", "templates/telegram_alert.tmpl")
	t.Setenv("STORE_PATH", "data/test.db")
	t.Setenv("TELEGRAM_PROXY_URL", "mtproto://proxy.example:443")

	_, err := Parse([]string{})
	if err == nil {
		t.Fatal("expected validation error for unsupported telegram proxy scheme")
	}
}

func TestValidateRejectsTelegramBaseURLWithCredentials(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "token")
	t.Setenv("TELEGRAM_CHAT_ID", "chat")
	t.Setenv("ALERT_TEMPLATE_PATH", "templates/telegram_alert.tmpl")
	t.Setenv("STORE_PATH", "data/test.db")
	t.Setenv("TELEGRAM_BASE_URL", "https://user:pass@botapi.internal.example")

	_, err := Parse([]string{})
	if err == nil {
		t.Fatal("expected validation error for telegram base URL with credentials")
	}
}

func TestMain(m *testing.M) {
	code := m.Run()
	os.Exit(code)
}
