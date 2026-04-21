package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/anton-bystrov/webhook-telegram-proxy/internal/config"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/metrics"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/models"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/store"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/telegram"
	alerttemplate "github.com/anton-bystrov/webhook-telegram-proxy/internal/template"
)

type DeliveryService struct {
	cfg      config.Config
	store    *store.Store
	renderer *alerttemplate.Renderer
	client   telegram.Client
	metrics  *metrics.Metrics
	logger   *slog.Logger
	now      func() time.Time
}

func NewDeliveryService(cfg config.Config, st *store.Store, renderer *alerttemplate.Renderer, client telegram.Client, m *metrics.Metrics, logger *slog.Logger) *DeliveryService {
	return &DeliveryService{
		cfg:      cfg,
		store:    st,
		renderer: renderer,
		client:   client,
		metrics:  m,
		logger:   logger,
		now:      time.Now,
	}
}

func (s *DeliveryService) Run(ctx context.Context) {
	recoveryTicker := time.NewTicker(s.cfg.RecoveryInterval)
	defer recoveryTicker.Stop()

	rotationTicker := time.NewTicker(s.cfg.StoreRotationInterval)
	defer rotationTicker.Stop()

	s.refreshQueueMetrics(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-recoveryTicker.C:
			s.refreshQueueMetrics(ctx)
			if err := s.RecoverPending(ctx); err != nil {
				s.logger.Error("recovery cycle failed", "error", err)
			}
		case <-rotationTicker.C:
			if err := s.RotateStore(ctx); err != nil {
				s.logger.Error("store rotation failed", "error", err)
			}
		}
	}
}

func (s *DeliveryService) EnsureCapacity(ctx context.Context) error {
	size, err := s.store.SizeBytes()
	if err != nil {
		return fmt.Errorf("measure store size: %w", err)
	}
	s.metrics.StoreSizeBytes.Set(float64(size))
	if size < s.cfg.StoreRotationHighWatermark {
		s.metrics.StoreDiskPressure.Set(0)
		return nil
	}

	s.metrics.StoreDiskPressure.Set(1)
	if s.cfg.StoreRotationEnabled {
		if err := s.RotateStore(ctx); err != nil {
			return err
		}
	}

	size, err = s.store.SizeBytes()
	if err != nil {
		return fmt.Errorf("measure store size after rotation: %w", err)
	}
	s.metrics.StoreSizeBytes.Set(float64(size))
	if size > s.cfg.StoreMaxSizeBytes {
		s.metrics.StoreRejectionsTotal.WithLabelValues("disk_pressure").Inc()
		return fmt.Errorf("store size %d exceeds max %d", size, s.cfg.StoreMaxSizeBytes)
	}
	if size < s.cfg.StoreRotationHighWatermark {
		s.metrics.StoreDiskPressure.Set(0)
	}
	return nil
}

func (s *DeliveryService) RecoverPending(ctx context.Context) error {
	ids, err := s.store.ListDueEventIDs(ctx, s.now().UTC(), s.cfg.WorkerBatchSize)
	if err != nil {
		return err
	}
	for _, eventID := range ids {
		if err := s.ProcessEventByID(ctx, eventID); err != nil {
			s.logger.Warn("pending delivery remains scheduled", "event_id", eventID, "error", err)
		}
	}
	return nil
}

func (s *DeliveryService) ProcessEventByID(ctx context.Context, eventID string) error {
	record, claimed, err := s.store.ClaimForSend(ctx, eventID, s.now().UTC())
	if err != nil {
		return fmt.Errorf("claim delivery: %w", err)
	}
	if !claimed {
		return nil
	}

	start := time.Now()
	resultLabel := "success"
	defer func() {
		s.metrics.ObserveDeliveryAttempt(resultLabel, time.Since(start))
		s.refreshQueueMetrics(ctx)
	}()

	var parts []string
	if len(record.RenderedParts) > 0 {
		parts = record.RenderedParts
	} else {
		var payload models.WebhookPayload
		if err := json.Unmarshal(record.OriginalPayload, &payload); err != nil {
			resultLabel = "failed"
			_ = s.store.MarkFailed(ctx, record.EventID, record.RenderedMessage, err.Error())
			return fmt.Errorf("decode stored payload: %w", err)
		}

		built, err := s.buildMessages(payload)
		if err != nil {
			resultLabel = "failed"
			_ = s.store.MarkFailed(ctx, record.EventID, record.RenderedMessage, err.Error())
			return fmt.Errorf("render alert template: %w", err)
		}
		parts = built
		if err := s.store.SaveRenderedParts(ctx, record.EventID, joinRendered(parts), parts); err != nil {
			resultLabel = "failed"
			return fmt.Errorf("persist rendered messages: %w", err)
		}
	}

	for index := record.SentParts; index < len(parts); index++ {
		sendCtx, cancel := context.WithTimeout(ctx, s.cfg.HTTPWriteTimeout)
		sent, err := s.client.SendMessage(sendCtx, s.cfg.TelegramChatID, parts[index], s.cfg.MessageParseMode)
		cancel()
		if err != nil {
			resultLabel = "retry"
			return s.handleSendError(ctx, record, parts, err)
		}

		var messageID *int64
		if index == 0 {
			messageID = &sent.MessageID
		}
		if err := s.store.MarkChunkSent(ctx, record.EventID, index+1, messageID); err != nil {
			resultLabel = "failed"
			return fmt.Errorf("mark chunk sent: %w", err)
		}
	}

	if err := s.store.MarkDelivered(ctx, record.EventID, joinRendered(parts), s.cfg.TelegramChatID, len(parts), s.now().UTC()); err != nil {
		resultLabel = "failed"
		return fmt.Errorf("mark delivered: %w", err)
	}
	resultLabel = "success"
	return nil
}

func (s *DeliveryService) RotateStore(ctx context.Context) error {
	size, err := s.store.SizeBytes()
	if err != nil {
		s.metrics.StoreRotationRunsTotal.WithLabelValues("error").Inc()
		return err
	}
	s.metrics.StoreSizeBytes.Set(float64(size))
	if !s.cfg.StoreRotationEnabled || size < s.cfg.StoreRotationHighWatermark {
		s.metrics.StoreRotationRunsTotal.WithLabelValues("skipped").Inc()
		return nil
	}

	deleted := map[string]int{}
	for size > s.cfg.StoreRotationLowWatermark {
		candidates, err := s.store.SelectRotatable(
			ctx,
			s.now().UTC().Add(-s.cfg.StoreRetentionDelivered),
			s.now().UTC().Add(-s.cfg.StoreRetentionDeadLetter),
			100,
		)
		if err != nil {
			s.metrics.StoreRotationRunsTotal.WithLabelValues("error").Inc()
			return err
		}
		if len(candidates) == 0 {
			break
		}

		eventIDs := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			eventIDs = append(eventIDs, candidate.EventID)
			deleted[candidate.Status]++
		}
		if err := s.store.DeleteByEventIDs(ctx, eventIDs); err != nil {
			s.metrics.StoreRotationRunsTotal.WithLabelValues("error").Inc()
			return err
		}

		size, err = s.store.SizeBytes()
		if err != nil {
			s.metrics.StoreRotationRunsTotal.WithLabelValues("error").Inc()
			return err
		}
		s.metrics.StoreSizeBytes.Set(float64(size))
	}

	if s.cfg.StoreVacuumAfterRotation {
		if err := s.store.Vacuum(ctx); err != nil {
			s.logger.Warn("sqlite vacuum failed", "error", err)
		}
		size, err = s.store.SizeBytes()
		if err == nil {
			s.metrics.StoreSizeBytes.Set(float64(size))
		}
	}

	for status, count := range deleted {
		s.metrics.StoreRotatedRecordsTotal.WithLabelValues(status).Add(float64(count))
	}

	result := "success"
	if size > s.cfg.StoreRotationLowWatermark {
		result = "pressure_remaining"
		s.metrics.StoreDiskPressure.Set(1)
	} else {
		s.metrics.StoreDiskPressure.Set(0)
	}
	s.metrics.StoreRotationRunsTotal.WithLabelValues(result).Inc()
	return nil
}

func (s *DeliveryService) buildMessages(payload models.WebhookPayload) ([]string, error) {
	data := s.renderer.BuildData(payload)
	whole, err := s.renderer.Render(data)
	if err != nil {
		return nil, err
	}
	if len(whole) <= alerttemplate.MaxTelegramMessageChars {
		return []string{whole}, nil
	}

	chunks := make([][]alerttemplate.AlertData, 0)
	current := make([]alerttemplate.AlertData, 0)
	for _, alert := range data.Alerts {
		candidate := append(append([]alerttemplate.AlertData{}, current...), alert)
		rendered, err := s.renderer.Render(alerttemplate.CloneWithAlerts(data, candidate, 1, 1))
		if err != nil {
			return nil, err
		}
		if len(rendered) <= alerttemplate.MaxTelegramMessageChars {
			current = candidate
			continue
		}
		if len(current) == 0 {
			rendered, err = s.renderer.Render(alerttemplate.CloneWithAlerts(data, []alerttemplate.AlertData{alert}, 1, 1))
			if err != nil {
				return nil, err
			}
			if len(rendered) > alerttemplate.MaxTelegramMessageChars {
				return nil, fmt.Errorf("single rendered alert exceeds Telegram message limit")
			}
			chunks = append(chunks, []alerttemplate.AlertData{alert})
			current = nil
			continue
		}
		chunks = append(chunks, current)
		current = []alerttemplate.AlertData{alert}
	}
	if len(current) > 0 {
		chunks = append(chunks, current)
	}

	messages := make([]string, 0, len(chunks))
	for idx, chunk := range chunks {
		rendered, err := s.renderer.Render(alerttemplate.CloneWithAlerts(data, chunk, idx+1, len(chunks)))
		if err != nil {
			return nil, err
		}
		if len(rendered) > alerttemplate.MaxTelegramMessageChars {
			return nil, fmt.Errorf("rendered alert chunk exceeds Telegram message limit")
		}
		messages = append(messages, rendered)
	}

	return messages, nil
}

func (s *DeliveryService) handleSendError(ctx context.Context, record store.Record, parts []string, sendErr error) error {
	var apiErr *telegram.APIError
	renderedMessage := joinRendered(parts)
	if errors.As(sendErr, &apiErr) {
		if apiErr.Retryable {
			if record.AttemptCount >= s.cfg.MaxDeliveryAttempts {
				s.metrics.DeliveryDeadLetterTotal.Inc()
				if err := s.store.MarkDeadLetter(ctx, record.EventID, renderedMessage, apiErr.Error()); err != nil {
					return err
				}
				return apiErr
			}

			nextRetry := s.now().UTC().Add(backoff(record.AttemptCount, s.cfg.RetryBaseDelay, s.cfg.RetryMaxDelay))
			s.metrics.DeliveryRetriesTotal.WithLabelValues(classifyRetryReason(apiErr.StatusCode)).Inc()
			if err := s.store.ScheduleRetry(ctx, record.EventID, renderedMessage, apiErr.Error(), nextRetry); err != nil {
				return err
			}
			return apiErr
		}

		if err := s.store.MarkFailed(ctx, record.EventID, renderedMessage, apiErr.Error()); err != nil {
			return err
		}
		return apiErr
	}

	if record.AttemptCount >= s.cfg.MaxDeliveryAttempts {
		s.metrics.DeliveryDeadLetterTotal.Inc()
		if err := s.store.MarkDeadLetter(ctx, record.EventID, renderedMessage, sendErr.Error()); err != nil {
			return err
		}
		return sendErr
	}

	nextRetry := s.now().UTC().Add(backoff(record.AttemptCount, s.cfg.RetryBaseDelay, s.cfg.RetryMaxDelay))
	s.metrics.DeliveryRetriesTotal.WithLabelValues("network").Inc()
	if err := s.store.ScheduleRetry(ctx, record.EventID, renderedMessage, sendErr.Error(), nextRetry); err != nil {
		return err
	}
	return sendErr
}

func (s *DeliveryService) refreshQueueMetrics(ctx context.Context) {
	count, err := s.store.CountQueued(ctx)
	if err == nil {
		s.metrics.DeliveryQueueMessages.Set(float64(count))
	}
	age, err := s.store.OldestQueuedAge(ctx, s.now().UTC())
	if err == nil {
		s.metrics.DeliveryOldestQueuedMessageAge.Set(age)
	}
	size, err := s.store.SizeBytes()
	if err == nil {
		s.metrics.StoreSizeBytes.Set(float64(size))
	}
}

func backoff(attempt int, base, max time.Duration) time.Duration {
	if attempt < 1 {
		return base
	}
	delay := base
	for step := 1; step < attempt; step++ {
		delay *= 2
		if delay >= max {
			return max
		}
	}
	if delay > max {
		return max
	}
	return delay
}

func classifyRetryReason(statusCode int) string {
	switch {
	case statusCode == 429:
		return "throttled"
	case statusCode >= 500:
		return "server_error"
	default:
		return "network"
	}
}

func joinRendered(parts []string) string {
	return strings.Join(parts, "\n\n---\n\n")
}
