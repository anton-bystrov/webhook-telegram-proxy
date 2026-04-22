package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/anton-bystrov/webhook-telegram-proxy/internal/config"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/metrics"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/models"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/store"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/telegram"
	alerttemplate "github.com/anton-bystrov/webhook-telegram-proxy/internal/template"
)

// retryReasonNetwork etc. are the label values for delivery_retries_total.
// Kept as constants so callers can't typo them.
const (
	retryReasonThrottled   = "throttled"
	retryReasonServerError = "server_error"
	retryReasonClientError = "client_error"
	retryReasonNetwork     = "network"
)

// storeWriteTimeout is used for best-effort persistence when the caller's
// context is already canceled (e.g. client disconnected during webhook
// handling). We still want to avoid stranding records in 'sending'.
const storeWriteTimeout = 2 * time.Second

type DeliveryService struct {
	cfg      config.Config
	store    *store.Store
	renderer *alerttemplate.Renderer
	client   telegram.Client
	metrics  *metrics.Metrics
	logger   *slog.Logger
	now      func() time.Time

	// rotateMu serializes rotation: both the ticker-driven RotateStore and
	// the HTTP-path EnsureCapacity can race otherwise, producing
	// double-counted runs and flapping disk_pressure.
	rotateMu sync.Mutex

	rngMu sync.Mutex
	rng   *rand.Rand
}

func NewDeliveryService(cfg config.Config, st *store.Store, renderer *alerttemplate.Renderer, client telegram.Client, m *metrics.Metrics, logger *slog.Logger) *DeliveryService {
	if logger == nil {
		logger = slog.Default()
	}
	return &DeliveryService{
		cfg:      cfg,
		store:    st,
		renderer: renderer,
		client:   client,
		metrics:  m,
		logger:   logger,
		now:      time.Now,
		rng:      rand.New(rand.NewSource(time.Now().UnixNano())),
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
			if err := s.RecoverPending(ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.logger.Error("recovery cycle failed", "error", err)
			}
		case <-rotationTicker.C:
			if err := s.RotateStore(ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.logger.Error("store rotation failed", "error", err)
			}
		}
	}
}

// EnsureCapacity is called from the webhook ingress path before accepting a
// new record. If the store is over the high watermark, it triggers a synchronous
// rotation. Returns an error (which the caller maps to 503) only when the store
// genuinely can't fit more work — i.e., file size exceeds the hard max.
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

	// disk_pressure=1 means "we cannot accept new writes safely". That's only
	// true above the hard max. Above the high watermark but below max, rotation
	// is keeping up — pressure is 0.
	if size > s.cfg.StoreMaxSizeBytes {
		s.metrics.StoreDiskPressure.Set(1)
		s.metrics.StoreRejectionsTotal.WithLabelValues("disk_pressure").Inc()
		return fmt.Errorf("store size %d exceeds max %d", size, s.cfg.StoreMaxSizeBytes)
	}

	s.metrics.StoreDiskPressure.Set(0)
	return nil
}

func (s *DeliveryService) RecoverPending(ctx context.Context) error {
	ids, err := s.store.ListDueEventIDs(ctx, s.now().UTC(), s.cfg.WorkerBatchSize)
	if err != nil {
		return fmt.Errorf("list due event ids: %w", err)
	}
	for _, eventID := range ids {
		// Stop mid-batch on shutdown rather than forcing every queued send to
		// time out against a closing ctx.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.ProcessEventByID(ctx, eventID); err != nil {
			// Log at Warn, not Error: retries are normal. Error-level logs
			// should page someone; this shouldn't.
			s.logger.Warn("delivery attempt did not complete", "event_id", eventID, "error", err)
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

	logger := s.logger.With(
		"event_id", record.EventID,
		"attempt", record.AttemptCount,
	)

	start := time.Now()
	resultLabel := "success"
	defer func() {
		s.metrics.ObserveDeliveryAttempt(resultLabel, time.Since(start))
	}()

	parts, err := s.ensureRenderedParts(ctx, record, logger)
	if err != nil {
		// ensureRenderedParts has already chosen the right terminal/retry
		// transition; we just bubble the label up for metrics.
		resultLabel = "retry"
		if errors.Is(err, errRenderFatal) {
			resultLabel = "failed"
		}
		return err
	}

	for index := record.SentParts; index < len(parts); index++ {
		// Dedicated timeout for the Telegram call. HTTPWriteTimeout is
		// misnamed for this purpose — it's the inbound response timeout.
		// TODO: split into a separate cfg.TelegramHTTPTimeout and fall back
		// to HTTPWriteTimeout when zero.
		sendCtx, cancel := context.WithTimeout(ctx, s.cfg.HTTPWriteTimeout)
		sent, err := s.client.SendMessage(sendCtx, s.cfg.TelegramChatID, parts[index], s.cfg.MessageParseMode)
		cancel()
		if err != nil {
			resultLabel = "retry"
			return s.handleSendError(ctx, record, parts, err, logger)
		}

		var messageID *int64
		if index == 0 {
			messageID = &sent.MessageID
		}
		if err := s.store.MarkChunkSent(ctx, record.EventID, index+1, messageID); err != nil {
			// The chunk was already accepted by Telegram. If we can't persist
			// that, a later retry will re-send and produce a duplicate. Log
			// loudly so the operator can correlate.
			logger.Error("telegram accepted chunk but store write failed; duplicate possible on retry",
				"chunk", index+1, "error", err,
			)

			// Best-effort: try not to strand the record in 'sending'.
			persistCtx, cancel := persistCtx(ctx, storeWriteTimeout)
			defer cancel()
			nextRetry := s.now().UTC().Add(s.computeBackoff(record.AttemptCount))
			_ = s.store.ScheduleRetry(persistCtx, record.EventID, joinRendered(parts), err.Error(), nextRetry)

			resultLabel = "failed"
			return fmt.Errorf("mark chunk sent: %w", err)
		}
	}

	if err := s.store.MarkDelivered(ctx, record.EventID, joinRendered(parts), s.cfg.TelegramChatID, len(parts), s.now().UTC()); err != nil {
		logger.Error("delivered to Telegram but store write failed; duplicate possible on retry", "error", err)

		// Best-effort: try not to strand the record in 'sending'.
		persistCtx, cancel := persistCtx(ctx, storeWriteTimeout)
		defer cancel()
		nextRetry := s.now().UTC().Add(s.computeBackoff(record.AttemptCount))
		_ = s.store.ScheduleRetry(persistCtx, record.EventID, joinRendered(parts), err.Error(), nextRetry)

		resultLabel = "failed"
		return fmt.Errorf("mark delivered: %w", err)
	}

	logger.Debug("delivery complete", "parts", len(parts))
	return nil
}

// errRenderFatal signals that the stored payload or template produced a
// message that will never succeed — fail hard rather than retry forever.
var errRenderFatal = errors.New("render fatal")

// ensureRenderedParts loads parts from the record if already rendered, or
// renders them now and persists. On failure it picks the correct state
// transition itself, so the caller just returns the error.
func (s *DeliveryService) ensureRenderedParts(ctx context.Context, record store.Record, logger *slog.Logger) ([]string, error) {
	if len(record.RenderedParts) > 0 {
		return record.RenderedParts, nil
	}

	var payload models.WebhookPayload
	if err := json.Unmarshal(record.OriginalPayload, &payload); err != nil {
		// Stored JSON is malformed — no retry will fix that.
		persistCtx, cancel := persistCtx(ctx, storeWriteTimeout)
		defer cancel()
		_ = s.store.MarkFailed(persistCtx, record.EventID, record.RenderedMessage, err.Error())
		logger.Error("decode stored payload failed; marking failed", "error", err)
		return nil, fmt.Errorf("%w: decode stored payload: %v", errRenderFatal, err)
	}

	parts, err := s.buildMessages(payload)
	if err != nil {
		persistCtx, cancel := persistCtx(ctx, storeWriteTimeout)
		defer cancel()
		_ = s.store.MarkFailed(persistCtx, record.EventID, record.RenderedMessage, err.Error())
		logger.Error("render template failed; marking failed", "error", err)
		return nil, fmt.Errorf("%w: render template: %v", errRenderFatal, err)
	}

	rendered := joinRendered(parts)
	if err := s.store.SaveRenderedParts(ctx, record.EventID, rendered, parts); err != nil {
		// Storage transient — schedule a retry instead of stranding the record
		// in 'sending' until next restart's RequeueSending.
		nextRetry := s.now().UTC().Add(s.computeBackoff(record.AttemptCount))
		persistCtx, cancel := persistCtx(ctx, storeWriteTimeout)
		defer cancel()
		if scheduleErr := s.store.ScheduleRetry(persistCtx, record.EventID, rendered, err.Error(), nextRetry); scheduleErr != nil {
			logger.Error("store failed to schedule retry after persist error",
				"persist_error", err,
				"schedule_error", scheduleErr,
			)
		}
		return nil, fmt.Errorf("persist rendered messages: %w", err)
	}

	return parts, nil
}

// RotateStore purges terminal-state records whose retention has elapsed.
// Serialized via rotateMu so concurrent calls from the ticker and the HTTP
// path don't double-count.
func (s *DeliveryService) RotateStore(ctx context.Context) error {
	s.rotateMu.Lock()
	defer s.rotateMu.Unlock()

	size, err := s.store.SizeBytes()
	if err != nil {
		s.metrics.StoreRotationRunsTotal.WithLabelValues("error").Inc()
		return fmt.Errorf("measure store size: %w", err)
	}
	s.metrics.StoreSizeBytes.Set(float64(size))

	if !s.cfg.StoreRotationEnabled || size < s.cfg.StoreRotationHighWatermark {
		s.metrics.StoreRotationRunsTotal.WithLabelValues("skipped").Inc()
		return nil
	}

	deliveredBefore := s.now().UTC().Add(-s.cfg.StoreRetentionDelivered)
	terminalBefore := s.now().UTC().Add(-s.cfg.StoreRetentionDeadLetter)

	deleted := map[string]int{}
	for size > s.cfg.StoreRotationLowWatermark {
		if ctx.Err() != nil {
			s.metrics.StoreRotationRunsTotal.WithLabelValues("error").Inc()
			return ctx.Err()
		}

		// SelectRotatable lets us tag deletions by state for the metric.
		candidates, err := s.store.SelectRotatable(ctx, deliveredBefore, terminalBefore, 100)
		if err != nil {
			s.metrics.StoreRotationRunsTotal.WithLabelValues("error").Inc()
			return fmt.Errorf("select rotatable: %w", err)
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
			return fmt.Errorf("delete rotatable: %w", err)
		}

		size, err = s.store.SizeBytes()
		if err != nil {
			s.metrics.StoreRotationRunsTotal.WithLabelValues("error").Inc()
			return fmt.Errorf("measure store size after rotation: %w", err)
		}
		s.metrics.StoreSizeBytes.Set(float64(size))
	}

	// DELETE alone doesn't shrink the file without VACUUM or auto_vacuum.
	// Attempt VACUUM when enabled, then truncate the WAL at the end.
	if s.cfg.StoreVacuumAfterRotation {
		if err := s.store.Vacuum(ctx); err != nil {
			s.logger.Warn("sqlite vacuum failed", "error", err)
		}
	}
	if err := s.store.CheckpointWAL(ctx); err != nil {
		s.logger.Warn("wal checkpoint failed after rotation", "error", err)
	}

	size, err = s.store.SizeBytes()
	if err != nil {
		s.metrics.StoreRotationRunsTotal.WithLabelValues("error").Inc()
		return fmt.Errorf("measure store size after rotation: %w", err)
	}
	s.metrics.StoreSizeBytes.Set(float64(size))

	for status, count := range deleted {
		s.metrics.StoreRotatedRecordsTotal.WithLabelValues(status).Add(float64(count))
	}

	// disk_pressure only reflects "cannot accept writes". Rotation freed logical
	// space even when file size didn't shrink, so we clear pressure unless we're
	// above the hard max.
	result := "success"
	if size > s.cfg.StoreMaxSizeBytes {
		s.metrics.StoreDiskPressure.Set(1)
		result = "pressure_remaining"
	} else {
		s.metrics.StoreDiskPressure.Set(0)
		if size >= s.cfg.StoreRotationHighWatermark {
			// Rotation ran but the file is still above the high watermark (e.g. no
			// VACUUM). Not fatal, but worth surfacing.
			result = "pressure_remaining"
		}
	}

	s.metrics.StoreRotationRunsTotal.WithLabelValues(result).Inc()
	return nil
}

// buildMessages renders the alert batch into one or more Telegram-sized
// messages. Renders each alert once (O(n)) and packs greedily by byte length
// against the rendered header size, rather than re-rendering the whole
// template for every incremental alert (O(n²)).
func (s *DeliveryService) buildMessages(payload models.WebhookPayload) ([]string, error) {
	data := s.renderer.BuildData(payload)

	whole, err := s.renderer.Render(data)
	if err != nil {
		return nil, fmt.Errorf("render full batch: %w", err)
	}
	if telegramChars(whole) <= alerttemplate.MaxTelegramMessageChars {
		return []string{whole}, nil
	}

	// Measure per-alert overhead by rendering the empty-alerts template.
	// Anything left in the budget after the header is alert space. We
	// reserve a safety margin for the "Part X of Y" header growing as Y
	// exceeds single digits.
	const partCountReserve = 16 // "Part 1 of 999" + padding
	emptyShell, err := s.renderer.Render(alerttemplate.CloneWithAlerts(data, nil, 1, 1))
	if err != nil {
		return nil, fmt.Errorf("render empty shell: %w", err)
	}
	headerBudget := telegramChars(emptyShell) + partCountReserve
	alertBudget := alerttemplate.MaxTelegramMessageChars - headerBudget
	if alertBudget <= 0 {
		return nil, fmt.Errorf("template header alone exceeds Telegram limit")
	}

	// Render each alert once as a singleton to measure its size.
	type sizedAlert struct {
		alert alerttemplate.AlertData
		size  int
	}
	sized := make([]sizedAlert, 0, len(data.Alerts))
	for _, alert := range data.Alerts {
		rendered, err := s.renderer.Render(alerttemplate.CloneWithAlerts(data, []alerttemplate.AlertData{alert}, 1, 1))
		if err != nil {
			return nil, fmt.Errorf("render alert for sizing: %w", err)
		}
		measured := telegramChars(rendered) - telegramChars(emptyShell)
		if measured < 0 {
			measured = telegramChars(rendered)
		}
		if measured > alertBudget {
			// Single oversized alert: truncation would require template
			// cooperation we don't have here. Surface as a fatal render so
			// the caller marks failed rather than retrying forever.
			return nil, fmt.Errorf("single alert exceeds Telegram limit (%d > %d chars)", measured, alertBudget)
		}
		sized = append(sized, sizedAlert{alert: alert, size: measured})
	}

	// Greedy pack into chunks.
	var chunks [][]alerttemplate.AlertData
	var current []alerttemplate.AlertData
	var currentSize int
	for _, entry := range sized {
		if len(current) > 0 && currentSize+entry.size > alertBudget {
			chunks = append(chunks, current)
			current = nil
			currentSize = 0
		}
		current = append(current, entry.alert)
		currentSize += entry.size
	}
	if len(current) > 0 {
		chunks = append(chunks, current)
	}

	// Final render with correct PartIndex/PartCount.
	messages := make([]string, 0, len(chunks))
	for idx, chunk := range chunks {
		rendered, err := s.renderer.Render(alerttemplate.CloneWithAlerts(data, chunk, idx+1, len(chunks)))
		if err != nil {
			return nil, fmt.Errorf("render chunk %d: %w", idx+1, err)
		}
		if telegramChars(rendered) > alerttemplate.MaxTelegramMessageChars {
			// Safety net: our budget math should prevent this, but guard
			// against template quirks we didn't account for.
			return nil, fmt.Errorf("chunk %d exceeds Telegram limit after final render (%d chars)", idx+1, telegramChars(rendered))
		}
		messages = append(messages, rendered)
	}

	return messages, nil
}

func (s *DeliveryService) handleSendError(ctx context.Context, record store.Record, parts []string, sendErr error, logger *slog.Logger) error {
	renderedMessage := joinRendered(parts)

	// Shutdown / parent-ctx cancellation shouldn't count as a delivery failure.
	// The per-send timeout cancels its own ctx but doesn't cancel the parent;
	// checking ctx.Err() here distinguishes the two cases.
	if ctx.Err() != nil {
		logger.Info("send aborted by parent context; leaving record for next recovery tick", "error", sendErr)
		// Drop back to 'retry_scheduled' so the next claim doesn't burn another
		// attempt on the same in-flight send that never left the process.
		persistCtx, cancel := context.WithTimeout(context.Background(), storeWriteTimeout)
		defer cancel()
		if err := s.store.ScheduleRetry(persistCtx, record.EventID, renderedMessage, sendErr.Error(), s.now().UTC()); err != nil {
			logger.Warn("failed to reschedule aborted send", "error", err)
		}
		return sendErr
	}

	var apiErr *telegram.APIError
	if errors.As(sendErr, &apiErr) {
		if !apiErr.Retryable {
			if err := s.store.MarkFailed(ctx, record.EventID, renderedMessage, apiErr.Error()); err != nil {
				return fmt.Errorf("mark failed: %w", err)
			}
			logger.Warn("non-retryable telegram error; marked failed", "status", apiErr.StatusCode, "error", apiErr)
			return apiErr
		}
		if record.AttemptCount >= s.cfg.MaxDeliveryAttempts {
			s.metrics.DeliveryDeadLetterTotal.Inc()
			if err := s.store.MarkDeadLetter(ctx, record.EventID, renderedMessage, apiErr.Error()); err != nil {
				return fmt.Errorf("mark dead letter: %w", err)
			}
			logger.Warn("max attempts reached; moved to dead letter", "status", apiErr.StatusCode, "error", apiErr)
			return apiErr
		}

		wait := s.computeBackoff(record.AttemptCount)
		// Respect Telegram's retry_after when present (e.g. on 429).
		if apiErr.RetryAfter > wait {
			wait = apiErr.RetryAfter
		}
		nextRetry := s.now().UTC().Add(wait)
		s.metrics.DeliveryRetriesTotal.WithLabelValues(classifyRetryReason(apiErr.StatusCode)).Inc()
		if err := s.store.ScheduleRetry(ctx, record.EventID, renderedMessage, apiErr.Error(), nextRetry); err != nil {
			return fmt.Errorf("schedule retry: %w", err)
		}
		logger.Info("scheduled retry", "status", apiErr.StatusCode, "wait", wait)
		return apiErr
	}

	// Non-API error (DNS, TCP, TLS, context deadline on the per-send ctx).
	if record.AttemptCount >= s.cfg.MaxDeliveryAttempts {
		s.metrics.DeliveryDeadLetterTotal.Inc()
		if err := s.store.MarkDeadLetter(ctx, record.EventID, renderedMessage, sendErr.Error()); err != nil {
			return fmt.Errorf("mark dead letter: %w", err)
		}
		logger.Warn("max attempts reached on network error; moved to dead letter", "error", sendErr)
		return sendErr
	}

	wait := s.computeBackoff(record.AttemptCount)
	nextRetry := s.now().UTC().Add(wait)
	s.metrics.DeliveryRetriesTotal.WithLabelValues(retryReasonNetwork).Inc()
	if err := s.store.ScheduleRetry(ctx, record.EventID, renderedMessage, sendErr.Error(), nextRetry); err != nil {
		return fmt.Errorf("schedule retry: %w", err)
	}
	logger.Info("scheduled retry on network error", "wait", wait, "error", sendErr)
	return sendErr
}

// computeBackoff wraps the pure backoff calculation with equal jitter
// (delay * [0.5, 1.0)) to avoid thundering-herd on recovery after an outage.
func (s *DeliveryService) computeBackoff(attempt int) time.Duration {
	base := backoff(attempt, s.cfg.RetryBaseDelay, s.cfg.RetryMaxDelay)
	s.rngMu.Lock()
	jitter := 0.5 + 0.5*s.rng.Float64() //nolint:gosec // non-cryptographic jitter
	s.rngMu.Unlock()
	return time.Duration(float64(base) * jitter)
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

// backoff returns the base exponential delay for the Nth attempt, capped at max.
// Callers should wrap this with jitter via computeBackoff.
func backoff(attempt int, base, max time.Duration) time.Duration {
	if attempt < 1 {
		return base
	}
	// Guard against overflow on large attempt counts.
	delay := base
	for step := 1; step < attempt; step++ {
		delay *= 2
		if delay >= max || delay <= 0 {
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
		return retryReasonThrottled
	case statusCode >= 500:
		return retryReasonServerError
	case statusCode >= 400:
		return retryReasonClientError
	default:
		return retryReasonNetwork
	}
}

func joinRendered(parts []string) string {
	return strings.Join(parts, "\n\n---\n\n")
}

func telegramChars(message string) int {
	// Telegram's limit is expressed in "characters". Counting runes is a closer
	// approximation than byte length for non-ASCII alerts.
	return utf8.RuneCountInString(message)
}

func persistCtx(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return ctx, func() {}
	}
	return context.WithTimeout(context.Background(), timeout)
}
