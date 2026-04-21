package transporthttp

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anton-bystrov/webhook-telegram-proxy/internal/config"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/metrics"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/service"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/store"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/telegram"
	alerttemplate "github.com/anton-bystrov/webhook-telegram-proxy/internal/template"
)

type fakeTelegramClient struct {
	mu        sync.Mutex
	callCount int
}

func (f *fakeTelegramClient) SendMessage(ctx context.Context, chatID, text, parseMode string) (telegram.SentMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callCount++
	return telegram.SentMessage{MessageID: int64(f.callCount), ChatID: -100123}, nil
}

func TestHealthEndpoint(t *testing.T) {
	server := newHTTPTestServer(t)
	defer server.Close()

	resp, err := http.Get(server.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health error = %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if strings.Contains(string(body), "store_path") {
		t.Fatal("health response must not expose store_path")
	}
}

func TestMetricsEndpoint(t *testing.T) {
	server := newHTTPTestServer(t)
	defer server.Close()

	resp, err := http.Get(server.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics error = %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if !strings.Contains(string(body), "grafana_telegram_proxy_build_info") {
		t.Fatal("expected Prometheus build_info metric in response body")
	}
}

func TestWebhookRejectsInvalidSecret(t *testing.T) {
	server := newHTTPTestServer(t)
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/webhook/grafana", strings.NewReader(samplePayload))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Webhook-Secret", "wrong")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized, got %d", resp.StatusCode)
	}
}

func TestWebhookRejectsInvalidJSON(t *testing.T) {
	server := newHTTPTestServer(t)
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/webhook/grafana", strings.NewReader(`{`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Webhook-Secret", "secret")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request, got %d", resp.StatusCode)
	}
}

func TestWebhookRejectsUnsupportedContentType(t *testing.T) {
	server := newHTTPTestServer(t)
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/webhook/grafana", strings.NewReader(samplePayload))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-Webhook-Secret", "secret")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("expected 415 Unsupported Media Type, got %d", resp.StatusCode)
	}
}

func TestBasicAuthProtectsRoutesWhenConfigured(t *testing.T) {
	server := newHTTPTestServerWithConfig(t, func(cfg *config.Config) {
		cfg.BasicAuthUsername = "admin"
		cfg.BasicAuthPassword = "very-secret-password"
	})
	defer server.Close()

	resp, err := http.Get(server.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health error = %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized, got %d", resp.StatusCode)
	}
	if header := resp.Header.Get("WWW-Authenticate"); !strings.Contains(header, "Basic") {
		t.Fatalf("expected WWW-Authenticate header, got %q", header)
	}
}

func TestWebhookAcceptsValidBasicAuthAndSecret(t *testing.T) {
	server := newHTTPTestServerWithConfig(t, func(cfg *config.Config) {
		cfg.BasicAuthUsername = "admin"
		cfg.BasicAuthPassword = "very-secret-password"
	})
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/webhook/grafana", strings.NewReader(samplePayload))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Webhook-Secret", "secret")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("admin:very-secret-password")))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}
}

func newHTTPTestServer(t *testing.T) *httptest.Server {
	return newHTTPTestServerWithConfig(t, nil)
}

func newHTTPTestServerWithConfig(t *testing.T, mutate func(*config.Config)) *httptest.Server {
	t.Helper()

	templatePath := filepath.Join(t.TempDir(), "alert.tmpl")
	if err := os.WriteFile(templatePath, []byte("<b>{{ .Status }}</b>{{ range .Alerts }} {{ .Name }}{{ end }}"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg := config.Config{
		AppHost:                    "127.0.0.1",
		AppPort:                    8080,
		LogLevel:                   "INFO",
		Environment:                "test",
		TelegramBotToken:           "test-token",
		TelegramChatID:             "-100123",
		WebhookSecret:              "secret",
		AlertTemplatePath:          templatePath,
		MessageParseMode:           "HTML",
		HTTPReadTimeout:            time.Second,
		HTTPWriteTimeout:           time.Second,
		HTTPShutdownTimeout:        time.Second,
		HTTPIdleTimeout:            time.Second,
		MaxRequestBodyBytes:        1 << 20,
		MaxHeaderBytes:             1 << 20,
		MaxDeliveryAttempts:        2,
		RetryBaseDelay:             time.Millisecond,
		RetryMaxDelay:              time.Second,
		RecoveryInterval:           time.Millisecond,
		WorkerBatchSize:            16,
		StorePath:                  filepath.Join(t.TempDir(), "store.db"),
		StoreMaxSizeBytes:          10 * 1024 * 1024,
		StoreRotationEnabled:       true,
		StoreRotationHighWatermark: 8 * 1024 * 1024,
		StoreRotationLowWatermark:  6 * 1024 * 1024,
		StoreRetentionDelivered:    time.Hour,
		StoreRetentionDeadLetter:   time.Hour,
		StoreRotationInterval:      time.Second,
	}
	if mutate != nil {
		mutate(&cfg)
	}

	registry, err := metrics.New("test", "test")
	if err != nil {
		t.Fatalf("metrics.New() error = %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sqliteStore, err := store.New(context.Background(), cfg.StorePath, registry, logger)
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() {
		_ = sqliteStore.Close()
	})

	renderer, err := alerttemplate.Load(templatePath, registry)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	client := &fakeTelegramClient{}
	delivery := service.NewDeliveryService(cfg, sqliteStore, renderer, client, registry, logger)
	alerts := service.NewAlertService(sqliteStore, delivery, registry, logger)

	return httptest.NewServer(NewRouter(cfg, alerts, sqliteStore, registry, logger))
}

const samplePayload = `{
  "receiver": "telegram-proxy",
  "status": "firing",
  "alerts": [
    {
      "status": "firing",
      "labels": {
        "alertname": "HighErrorRate",
        "severity": "critical"
      },
      "annotations": {
        "summary": "Error rate is above threshold"
      },
      "startsAt": "2026-04-21T18:22:00Z"
    }
  ]
}`
