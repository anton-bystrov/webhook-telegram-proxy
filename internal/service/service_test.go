package service

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/anton-bystrov/webhook-telegram-proxy/internal/config"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/metrics"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/store"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/telegram"
	alerttemplate "github.com/anton-bystrov/webhook-telegram-proxy/internal/template"
)

type fakeTelegramClient struct {
	mu        sync.Mutex
	callCount int
	responses []fakeTelegramResult
}

type fakeTelegramResult struct {
	message telegram.SentMessage
	err     error
}

func (f *fakeTelegramClient) SendMessage(ctx context.Context, chatID, text, parseMode string) (telegram.SentMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	index := f.callCount
	f.callCount++
	if index >= len(f.responses) {
		return telegram.SentMessage{MessageID: int64(index + 1), ChatID: -100123}, nil
	}
	return f.responses[index].message, f.responses[index].err
}

func (f *fakeTelegramClient) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.callCount
}

func TestAcceptWebhookSuccessfulDelivery(t *testing.T) {
	deps := newServiceDeps(t, &fakeTelegramClient{
		responses: []fakeTelegramResult{{message: telegram.SentMessage{MessageID: 1, ChatID: -100123}}},
	})

	result, statusCode, err := deps.alerts.AcceptWebhook(context.Background(), []byte(samplePayload))
	if err != nil {
		t.Fatalf("AcceptWebhook() error = %v", err)
	}
	if statusCode != 200 {
		t.Fatalf("expected status 200, got %d", statusCode)
	}
	if result.EventID == "" || result.IdempotencyKey == "" {
		t.Fatal("expected event id and idempotency key")
	}

	record, found, err := deps.store.GetByEventID(context.Background(), result.EventID)
	if err != nil {
		t.Fatalf("GetByEventID() error = %v", err)
	}
	if !found {
		t.Fatal("expected stored record")
	}
	if record.Status != store.StatusDelivered {
		t.Fatalf("expected delivered status, got %s", record.Status)
	}
	if deps.client.Calls() != 1 {
		t.Fatalf("expected one Telegram call, got %d", deps.client.Calls())
	}
}

func TestAcceptAlertmanagerWebhookSuccessfulDelivery(t *testing.T) {
	deps := newServiceDeps(t, &fakeTelegramClient{
		responses: []fakeTelegramResult{{message: telegram.SentMessage{MessageID: 2, ChatID: -100123}}},
	})

	result, statusCode, err := deps.alerts.AcceptWebhook(context.Background(), []byte(sampleAlertmanagerPayload))
	if err != nil {
		t.Fatalf("AcceptWebhook() error = %v", err)
	}
	if statusCode != 200 {
		t.Fatalf("expected status 200, got %d", statusCode)
	}
	if result.EventID == "" || result.IdempotencyKey == "" {
		t.Fatal("expected event id and idempotency key")
	}

	record, found, err := deps.store.GetByEventID(context.Background(), result.EventID)
	if err != nil {
		t.Fatalf("GetByEventID() error = %v", err)
	}
	if !found {
		t.Fatal("expected stored record")
	}
	if record.Status != store.StatusDelivered {
		t.Fatalf("expected delivered status, got %s", record.Status)
	}
	if deps.client.Calls() != 1 {
		t.Fatalf("expected one Telegram call, got %d", deps.client.Calls())
	}
}

func TestAcceptWebhookInvalidJSON(t *testing.T) {
	deps := newServiceDeps(t, &fakeTelegramClient{})

	_, statusCode, err := deps.alerts.AcceptWebhook(context.Background(), []byte(`{`))
	if err == nil {
		t.Fatal("expected invalid json error")
	}
	if statusCode != 400 {
		t.Fatalf("expected status 400, got %d", statusCode)
	}
}

func TestDuplicateDeliveredWebhookDoesNotResend(t *testing.T) {
	deps := newServiceDeps(t, &fakeTelegramClient{
		responses: []fakeTelegramResult{{message: telegram.SentMessage{MessageID: 11, ChatID: -100123}}},
	})

	if _, _, err := deps.alerts.AcceptWebhook(context.Background(), []byte(samplePayload)); err != nil {
		t.Fatalf("first AcceptWebhook() error = %v", err)
	}
	result, statusCode, err := deps.alerts.AcceptWebhook(context.Background(), []byte(samplePayload))
	if err != nil {
		t.Fatalf("second AcceptWebhook() error = %v", err)
	}
	if statusCode != 200 {
		t.Fatalf("expected status 200, got %d", statusCode)
	}
	if !result.Duplicate {
		t.Fatal("expected duplicate result")
	}
	if deps.client.Calls() != 1 {
		t.Fatalf("expected a single Telegram send, got %d", deps.client.Calls())
	}
}

func TestRetryableTelegramErrorSchedulesRetry(t *testing.T) {
	deps := newServiceDeps(t, &fakeTelegramClient{
		responses: []fakeTelegramResult{{
			err: &telegram.APIError{StatusCode: 500, Description: "upstream failed", Retryable: true},
		}},
	})
	deps.delivery.now = func() time.Time { return time.Now().UTC() }

	result, statusCode, err := deps.alerts.AcceptWebhook(context.Background(), []byte(samplePayload))
	if err == nil {
		t.Fatal("expected retryable error")
	}
	if statusCode != 502 {
		t.Fatalf("expected status 502, got %d", statusCode)
	}

	record, found, lookupErr := deps.store.GetByEventID(context.Background(), result.EventID)
	if lookupErr != nil {
		t.Fatalf("GetByEventID() error = %v", lookupErr)
	}
	if !found {
		t.Fatal("expected stored record")
	}
	if record.Status != store.StatusRetryScheduled {
		t.Fatalf("expected retry_scheduled, got %s", record.Status)
	}
}

func TestDeadLetterAfterMaxAttempts(t *testing.T) {
	deps := newServiceDeps(t, &fakeTelegramClient{
		responses: []fakeTelegramResult{{
			err: &telegram.APIError{StatusCode: 500, Description: "retryable", Retryable: true},
		}},
	})
	deps.cfg.MaxDeliveryAttempts = 1
	deps.delivery.cfg.MaxDeliveryAttempts = 1

	result, statusCode, err := deps.alerts.AcceptWebhook(context.Background(), []byte(samplePayload))
	if err != nil {
		t.Fatalf("AcceptWebhook() error = %v", err)
	}
	if statusCode != 200 {
		t.Fatalf("expected status 200, got %d", statusCode)
	}

	record, found, lookupErr := deps.store.GetByEventID(context.Background(), result.EventID)
	if lookupErr != nil {
		t.Fatalf("GetByEventID() error = %v", lookupErr)
	}
	if !found {
		t.Fatal("expected stored record")
	}
	if record.Status != store.StatusDeadLetter {
		t.Fatalf("expected dead_letter, got %s", record.Status)
	}
}

func TestDuplicateDeadLetterWebhookDoesNotRequeue(t *testing.T) {
	deps := newServiceDeps(t, &fakeTelegramClient{
		responses: []fakeTelegramResult{{
			err: &telegram.APIError{StatusCode: 500, Description: "retryable", Retryable: true},
		}},
	})
	deps.cfg.MaxDeliveryAttempts = 1
	deps.delivery.cfg.MaxDeliveryAttempts = 1

	first, statusCode, err := deps.alerts.AcceptWebhook(context.Background(), []byte(samplePayload))
	if err != nil {
		t.Fatalf("first AcceptWebhook() error = %v", err)
	}
	if statusCode != 200 {
		t.Fatalf("expected status 200, got %d", statusCode)
	}

	second, statusCode, err := deps.alerts.AcceptWebhook(context.Background(), []byte(samplePayload))
	if err != nil {
		t.Fatalf("second AcceptWebhook() error = %v", err)
	}
	if statusCode != 200 {
		t.Fatalf("expected status 200, got %d", statusCode)
	}
	if !second.Duplicate {
		t.Fatal("expected duplicate result")
	}
	if second.Status != store.StatusDeadLetter {
		t.Fatalf("expected duplicate status dead_letter, got %s", second.Status)
	}
	if second.EventID != first.EventID {
		t.Fatalf("expected same event id, got %s and %s", first.EventID, second.EventID)
	}
	if deps.client.Calls() != 1 {
		t.Fatalf("expected a single Telegram send, got %d", deps.client.Calls())
	}
}

func TestRecoveryProcessesPendingAfterRestart(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "store.db")

	firstClient := &fakeTelegramClient{
		responses: []fakeTelegramResult{{
			err: &telegram.APIError{StatusCode: 500, Description: "temporary", Retryable: true},
		}},
	}
	first := newServiceDepsAtPath(t, dbPath, firstClient)
	first.cfg.RetryBaseDelay = time.Millisecond
	first.delivery.cfg.RetryBaseDelay = time.Millisecond

	result, _, err := first.alerts.AcceptWebhook(context.Background(), []byte(samplePayload))
	if err == nil {
		t.Fatal("expected first attempt to fail")
	}

	if err := first.store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	secondClient := &fakeTelegramClient{
		responses: []fakeTelegramResult{{message: telegram.SentMessage{MessageID: 22, ChatID: -100123}}},
	}
	second := newServiceDepsAtPath(t, dbPath, secondClient)
	time.Sleep(5 * time.Millisecond)
	if err := second.delivery.RecoverPending(context.Background()); err != nil {
		t.Fatalf("RecoverPending() error = %v", err)
	}

	record, found, lookupErr := second.store.GetByEventID(context.Background(), result.EventID)
	if lookupErr != nil {
		t.Fatalf("GetByEventID() error = %v", lookupErr)
	}
	if !found {
		t.Fatal("expected stored record")
	}
	if record.Status != store.StatusDelivered {
		t.Fatalf("expected delivered after recovery, got %s", record.Status)
	}
}

func TestRestartRequeuesSendingRecords(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "store.db")

	first := newServiceDepsAtPath(t, dbPath, &fakeTelegramClient{})

	old := time.Now().Add(-time.Hour).UTC()
	lastAttempt := old
	sending := store.Record{
		EventID:         "sending",
		IdempotencyKey:  "sending-key",
		Status:          store.StatusSending,
		OriginalPayload: []byte(samplePayload),
		CreatedAt:       old,
		UpdatedAt:       old,
		LastAttemptAt:   &lastAttempt,
	}
	if err := first.store.CreateReceived(context.Background(), sending); err != nil {
		t.Fatalf("CreateReceived() error = %v", err)
	}
	if err := first.store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	client := &fakeTelegramClient{
		responses: []fakeTelegramResult{{message: telegram.SentMessage{MessageID: 33, ChatID: -100123}}},
	}
	second := newServiceDepsAtPath(t, dbPath, client)

	requeued, err := second.store.RequeueSending(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("RequeueSending() error = %v", err)
	}
	if requeued != 1 {
		t.Fatalf("expected one sending record to be requeued, got %d", requeued)
	}

	if err := second.delivery.RecoverPending(context.Background()); err != nil {
		t.Fatalf("RecoverPending() error = %v", err)
	}

	record, found, lookupErr := second.store.GetByEventID(context.Background(), "sending")
	if lookupErr != nil {
		t.Fatalf("GetByEventID() error = %v", lookupErr)
	}
	if !found {
		t.Fatal("expected stored record")
	}
	if record.Status != store.StatusDelivered {
		t.Fatalf("expected delivered after restart recovery, got %s", record.Status)
	}
	if client.Calls() != 1 {
		t.Fatalf("expected one Telegram call, got %d", client.Calls())
	}
}

func TestRotationDeletesOnlyTerminalRecords(t *testing.T) {
	deps := newServiceDeps(t, &fakeTelegramClient{})
	deps.cfg.StoreRotationHighWatermark = 1
	deps.cfg.StoreRotationLowWatermark = 1
	deps.cfg.StoreRetentionDelivered = time.Hour
	deps.cfg.StoreRetentionDeadLetter = time.Hour
	deps.delivery.cfg.StoreRotationHighWatermark = 1
	deps.delivery.cfg.StoreRotationLowWatermark = 1
	deps.delivery.cfg.StoreRetentionDelivered = time.Hour
	deps.delivery.cfg.StoreRetentionDeadLetter = time.Hour

	old := time.Now().Add(-2 * time.Hour).UTC()
	queued := store.Record{
		EventID:         "queued",
		IdempotencyKey:  "queued-key",
		Status:          store.StatusQueued,
		OriginalPayload: []byte(samplePayload),
		CreatedAt:       old,
		UpdatedAt:       old,
	}
	delivered := store.Record{
		EventID:         "delivered",
		IdempotencyKey:  "delivered-key",
		Status:          store.StatusDelivered,
		OriginalPayload: []byte(samplePayload),
		CreatedAt:       old,
		UpdatedAt:       old,
	}
	dead := store.Record{
		EventID:         "dead",
		IdempotencyKey:  "dead-key",
		Status:          store.StatusDeadLetter,
		OriginalPayload: []byte(samplePayload),
		CreatedAt:       old,
		UpdatedAt:       old,
	}

	for _, record := range []store.Record{queued, delivered, dead} {
		if err := deps.store.CreateReceived(context.Background(), record); err != nil {
			t.Fatalf("CreateReceived() error = %v", err)
		}
	}

	if err := deps.delivery.RotateStore(context.Background()); err != nil {
		t.Fatalf("RotateStore() error = %v", err)
	}

	if _, found, _ := deps.store.GetByEventID(context.Background(), "queued"); !found {
		t.Fatal("queued record should remain")
	}
	if _, found, _ := deps.store.GetByEventID(context.Background(), "delivered"); found {
		t.Fatal("delivered record should be rotated out")
	}
	if _, found, _ := deps.store.GetByEventID(context.Background(), "dead"); found {
		t.Fatal("dead-letter record should be rotated out")
	}
}

type serviceDeps struct {
	cfg      config.Config
	store    *store.Store
	renderer *alerttemplate.Renderer
	delivery *DeliveryService
	alerts   *AlertService
	client   *fakeTelegramClient
}

func newServiceDeps(t *testing.T, client *fakeTelegramClient) *serviceDeps {
	return newServiceDepsAtPath(t, filepath.Join(t.TempDir(), "store.db"), client)
}

func newServiceDepsAtPath(t *testing.T, dbPath string, client *fakeTelegramClient) *serviceDeps {
	t.Helper()

	if client == nil {
		client = &fakeTelegramClient{}
	}

	templatePath := filepath.Join(t.TempDir(), "alert.tmpl")
	if err := os.WriteFile(templatePath, []byte("<b>{{ .Status }}</b>{{ range .Alerts }} {{ .Name }} {{ .Summary }}{{ end }}"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg := config.Config{
		AppHost:                    "127.0.0.1",
		AppPort:                    8080,
		LogLevel:                   "INFO",
		Environment:                "test",
		TelegramBotToken:           "test-token",
		TelegramChatID:             "-100123",
		AlertTemplatePath:          templatePath,
		MessageParseMode:           "HTML",
		HTTPReadTimeout:            time.Second,
		HTTPWriteTimeout:           time.Second,
		HTTPShutdownTimeout:        time.Second,
		HTTPIdleTimeout:            time.Second,
		MaxRequestBodyBytes:        1 << 20,
		MaxHeaderBytes:             1 << 20,
		MaxDeliveryAttempts:        3,
		RetryBaseDelay:             time.Millisecond,
		RetryMaxDelay:              time.Second,
		RecoveryInterval:           time.Millisecond,
		WorkerBatchSize:            16,
		StorePath:                  dbPath,
		StoreMaxSizeBytes:          10 * 1024 * 1024,
		StoreRotationEnabled:       true,
		StoreRotationHighWatermark: 8 * 1024 * 1024,
		StoreRotationLowWatermark:  6 * 1024 * 1024,
		StoreRetentionDelivered:    time.Hour,
		StoreRetentionDeadLetter:   time.Hour,
		StoreRotationInterval:      time.Second,
	}

	registry, err := metrics.New("test", "test")
	if err != nil {
		t.Fatalf("metrics.New() error = %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sqliteStore, err := store.New(context.Background(), dbPath, registry, logger)
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

	delivery := NewDeliveryService(cfg, sqliteStore, renderer, client, registry, logger)
	alerts := NewAlertService(sqliteStore, delivery, registry, logger)

	return &serviceDeps{
		cfg:      cfg,
		store:    sqliteStore,
		renderer: renderer,
		delivery: delivery,
		alerts:   alerts,
		client:   client,
	}
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
        "summary": "Error rate is above threshold",
        "description": "5xx ratio is above 5%"
      },
      "startsAt": "2026-04-21T18:22:00Z",
      "endsAt": "0001-01-01T00:00:00Z",
      "generatorURL": "https://grafana.example/alert"
    }
  ],
  "groupLabels": {
    "alertname": "HighErrorRate"
  },
  "commonLabels": {
    "severity": "critical"
  },
  "commonAnnotations": {
    "summary": "Error rate is above threshold"
  },
  "externalURL": "https://grafana.example",
  "version": "1",
  "groupKey": "{}:{alertname=\"HighErrorRate\"}",
  "truncatedAlerts": 0,
  "title": "[FIRING:1] HighErrorRate",
  "state": "alerting",
  "message": "1 firing alert"
}`

const sampleAlertmanagerPayload = `{
  "receiver": "telegram-webhook-proxy",
  "status": "firing",
  "alerts": [
    {
      "status": "firing",
      "labels": {
        "alertname": "MongoPrimaryDown",
        "severity": "critical",
        "instance": "mongodb-db-1"
      },
      "annotations": {
        "summary": "Primary MongoDB instance is down",
        "description": "mongodb-db-1 is unreachable"
      },
      "startsAt": "2026-04-22T18:22:00Z",
      "endsAt": "0001-01-01T00:00:00Z",
      "generatorURL": "https://alertmanager.example/#/alerts",
      "fingerprint": "8c1fa7db"
    }
  ],
  "groupLabels": {
    "alertname": "MongoPrimaryDown"
  },
  "commonLabels": {
    "severity": "critical",
    "service": "mongodb"
  },
  "commonAnnotations": {
    "summary": "Primary MongoDB instance is down"
  },
  "externalURL": "https://alertmanager.example",
  "version": "4",
  "groupKey": "{}:{alertname=\"MongoPrimaryDown\"}",
  "truncatedAlerts": 0
}`
