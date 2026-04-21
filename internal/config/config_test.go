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

func TestMain(m *testing.M) {
	code := m.Run()
	os.Exit(code)
}
