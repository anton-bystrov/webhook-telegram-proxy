package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/anton-bystrov/webhook-telegram-proxy/internal/metrics"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/models"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/store"
)

type AcceptResult struct {
	EventID        string `json:"event_id"`
	IdempotencyKey string `json:"idempotency_key"`
	Status         string `json:"status"`
	Duplicate      bool   `json:"duplicate"`
}

type AlertService struct {
	store    *store.Store
	delivery *DeliveryService
	metrics  *metrics.Metrics
	logger   *slog.Logger
	now      func() time.Time
}

func NewAlertService(st *store.Store, delivery *DeliveryService, m *metrics.Metrics, logger *slog.Logger) *AlertService {
	return &AlertService{
		store:    st,
		delivery: delivery,
		metrics:  m,
		logger:   logger,
		now:      time.Now,
	}
}

func (s *AlertService) AcceptWebhook(ctx context.Context, raw []byte) (AcceptResult, int, error) {
	var payload models.WebhookPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		s.metrics.WebhookEventsReceivedTotal.WithLabelValues("invalid_json").Inc()
		return AcceptResult{}, 400, fmt.Errorf("decode webhook payload: %w", err)
	}

	if err := s.delivery.EnsureCapacity(ctx); err != nil {
		s.metrics.WebhookEventsReceivedTotal.WithLabelValues("store_rejected").Inc()
		return AcceptResult{}, 503, err
	}

	idempotencyKey := ComputeFingerprint(payload)
	existing, found, err := s.store.GetByIdempotencyKey(ctx, idempotencyKey)
	if err != nil {
		s.metrics.WebhookEventsReceivedTotal.WithLabelValues("store_error").Inc()
		return AcceptResult{}, 500, fmt.Errorf("lookup existing delivery: %w", err)
	}
	if found {
		if existing.Status == store.StatusDelivered {
			s.metrics.WebhookEventsReceivedTotal.WithLabelValues("duplicate_delivered").Inc()
			return AcceptResult{
				EventID:        existing.EventID,
				IdempotencyKey: existing.IdempotencyKey,
				Status:         existing.Status,
				Duplicate:      true,
			}, 200, nil
		}
		if existing.Status == store.StatusFailed || existing.Status == store.StatusDeadLetter {
			s.metrics.WebhookEventsReceivedTotal.WithLabelValues("duplicate_terminal").Inc()
			return AcceptResult{
				EventID:        existing.EventID,
				IdempotencyKey: existing.IdempotencyKey,
				Status:         existing.Status,
				Duplicate:      true,
			}, http.StatusOK, nil
		}
		if err := s.delivery.ProcessEventByID(ctx, existing.EventID); err != nil {
			s.metrics.WebhookEventsReceivedTotal.WithLabelValues("duplicate_pending").Inc()
			return s.loadAcceptResult(ctx, existing.EventID, existing.IdempotencyKey, true)
		}
		s.metrics.WebhookEventsReceivedTotal.WithLabelValues("duplicate_pending").Inc()
		return s.loadAcceptResult(ctx, existing.EventID, existing.IdempotencyKey, true)
	}

	now := s.now().UTC()
	eventID, err := randomID()
	if err != nil {
		return AcceptResult{}, 500, fmt.Errorf("generate event id: %w", err)
	}

	record := store.Record{
		EventID:         eventID,
		IdempotencyKey:  idempotencyKey,
		Status:          store.StatusReceived,
		OriginalPayload: raw,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := s.store.CreateReceived(ctx, record); err != nil {
		if err == store.ErrAlreadyExists {
			return s.AcceptWebhook(ctx, raw)
		}
		s.metrics.WebhookEventsReceivedTotal.WithLabelValues("store_error").Inc()
		return AcceptResult{}, 500, fmt.Errorf("persist webhook event: %w", err)
	}
	if err := s.store.MarkQueued(ctx, eventID, now); err != nil {
		return AcceptResult{}, 500, fmt.Errorf("queue delivery: %w", err)
	}

	if err := s.delivery.ProcessEventByID(ctx, eventID); err != nil {
		result, statusCode, loadErr := s.loadAcceptResult(ctx, eventID, idempotencyKey, false)
		if loadErr != nil {
			return AcceptResult{}, http.StatusInternalServerError, loadErr
		}
		switch result.Status {
		case store.StatusRetryScheduled:
			s.metrics.WebhookEventsReceivedTotal.WithLabelValues("queued_retry").Inc()
			return result, http.StatusBadGateway, err
		case store.StatusFailed, store.StatusDeadLetter:
			s.metrics.WebhookEventsReceivedTotal.WithLabelValues("terminal_failure").Inc()
			return result, http.StatusOK, nil
		default:
			s.metrics.WebhookEventsReceivedTotal.WithLabelValues("queued_pending").Inc()
			return result, statusCode, nil
		}
	}

	s.metrics.WebhookEventsReceivedTotal.WithLabelValues("success").Inc()
	return s.loadAcceptResult(ctx, eventID, idempotencyKey, false)
}

func (s *AlertService) loadAcceptResult(ctx context.Context, eventID, idempotencyKey string, duplicate bool) (AcceptResult, int, error) {
	record, found, err := s.store.GetByEventID(ctx, eventID)
	if err != nil {
		return AcceptResult{}, http.StatusInternalServerError, fmt.Errorf("reload delivery: %w", err)
	}
	if !found {
		return AcceptResult{}, http.StatusInternalServerError, fmt.Errorf("delivery record %s not found after processing", eventID)
	}

	result := AcceptResult{
		EventID:        eventID,
		IdempotencyKey: idempotencyKey,
		Status:         record.Status,
		Duplicate:      duplicate,
	}

	switch record.Status {
	case store.StatusDelivered, store.StatusFailed, store.StatusDeadLetter:
		return result, http.StatusOK, nil
	case store.StatusReceived, store.StatusQueued, store.StatusSending, store.StatusRetryScheduled:
		return result, http.StatusAccepted, nil
	default:
		return AcceptResult{}, http.StatusInternalServerError, fmt.Errorf("unexpected delivery status %q", record.Status)
	}
}

func ComputeFingerprint(payload models.WebhookPayload) string {
	builder := strings.Builder{}
	builder.WriteString(payload.Receiver)
	builder.WriteString("|")
	builder.WriteString(payload.Status)
	builder.WriteString("|")
	builder.WriteString(payload.GroupKey)
	builder.WriteString("|")
	builder.WriteString(payload.ExternalURL)
	builder.WriteString("|")
	builder.WriteString(payload.Title)
	builder.WriteString("|")
	builder.WriteString(payload.Message)
	builder.WriteString("|")
	builder.WriteString(stableMapString(payload.GroupLabels))
	builder.WriteString("|")
	builder.WriteString(stableMapString(payload.CommonLabels))
	builder.WriteString("|")
	builder.WriteString(stableMapString(payload.CommonAnnotations))
	builder.WriteString("|")

	alerts := make([]string, 0, len(payload.Alerts))
	for _, alert := range payload.Alerts {
		alerts = append(alerts, strings.Join([]string{
			alert.Status,
			alert.Fingerprint,
			alert.StartsAt.UTC().Format(time.RFC3339Nano),
			alert.EndsAt.UTC().Format(time.RFC3339Nano),
			alert.GeneratorURL,
			stableMapString(alert.Labels),
			stableMapString(alert.Annotations),
			alert.ValueString,
		}, "|"))
	}
	sort.Strings(alerts)
	builder.WriteString(strings.Join(alerts, "||"))

	hash := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(hash[:])
}

func stableMapString(values map[string]string) string {
	if len(values) == 0 {
		return ""
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	pairs := make([]string, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, key+"="+values[key])
	}
	return strings.Join(pairs, ",")
}

func randomID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}
