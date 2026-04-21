package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/anton-bystrov/webhook-telegram-proxy/internal/metrics"
)

const (
	StatusReceived       = "received"
	StatusQueued         = "queued"
	StatusSending        = "sending"
	StatusDelivered      = "delivered"
	StatusRetryScheduled = "retry_scheduled"
	StatusFailed         = "failed"
	StatusDeadLetter     = "dead_letter"
)

var (
	ErrAlreadyExists = errors.New("delivery already exists")
	ErrNotFound      = errors.New("delivery not found")
)

type Record struct {
	EventID           string
	IdempotencyKey    string
	Status            string
	OriginalPayload   []byte
	RenderedMessage   string
	RenderedParts     []string
	SplitCount        int
	SentParts         int
	AttemptCount      int
	NextRetryAt       *time.Time
	LastError         string
	TelegramChatID    string
	TelegramMessageID *int64
	CreatedAt         time.Time
	UpdatedAt         time.Time
	DeliveredAt       *time.Time
	LastAttemptAt     *time.Time
}

type RotationResult struct {
	DeletedByState map[string]int
	DeletedTotal   int
	StoreSizeBytes int64
}

type Store struct {
	db      *sql.DB
	path    string
	metrics *metrics.Metrics
	logger  *slog.Logger
}

func New(ctx context.Context, path string, m *metrics.Metrics, logger *slog.Logger) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create store directory: %w", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	st := &Store{
		db:      db,
		path:    path,
		metrics: m,
		logger:  logger,
	}

	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("apply sqlite pragma: %w", err)
		}
	}

	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply sqlite schema: %w", err)
	}

	return st, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *Store) GetByIdempotencyKey(ctx context.Context, key string) (Record, bool, error) {
	start := time.Now()
	row := s.db.QueryRowContext(ctx, `
		SELECT event_id, idempotency_key, status, original_payload, rendered_message, rendered_parts, split_count, sent_parts,
		       attempt_count, next_retry_at, last_error, telegram_chat_id, telegram_message_id, created_at, updated_at,
		       delivered_at, last_attempt_at
		FROM deliveries
		WHERE idempotency_key = ?`,
		key,
	)

	record, found, err := scanRecord(row)
	s.observe("get_by_idempotency_key", start, err)
	return record, found, err
}

func (s *Store) GetByEventID(ctx context.Context, eventID string) (Record, bool, error) {
	start := time.Now()
	row := s.db.QueryRowContext(ctx, `
		SELECT event_id, idempotency_key, status, original_payload, rendered_message, rendered_parts, split_count, sent_parts,
		       attempt_count, next_retry_at, last_error, telegram_chat_id, telegram_message_id, created_at, updated_at,
		       delivered_at, last_attempt_at
		FROM deliveries
		WHERE event_id = ?`,
		eventID,
	)

	record, found, err := scanRecord(row)
	s.observe("get_by_event_id", start, err)
	return record, found, err
}

func (s *Store) CreateReceived(ctx context.Context, record Record) error {
	start := time.Now()
	parts, err := json.Marshal(record.RenderedParts)
	if err != nil {
		return fmt.Errorf("marshal rendered parts: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO deliveries (
			event_id, idempotency_key, status, original_payload, rendered_message, rendered_parts, split_count, sent_parts,
			attempt_count, next_retry_at, last_error, telegram_chat_id, telegram_message_id, created_at, updated_at,
			delivered_at, last_attempt_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.EventID,
		record.IdempotencyKey,
		record.Status,
		record.OriginalPayload,
		record.RenderedMessage,
		string(parts),
		record.SplitCount,
		record.SentParts,
		record.AttemptCount,
		nullTime(record.NextRetryAt),
		record.LastError,
		record.TelegramChatID,
		nullInt64(record.TelegramMessageID),
		record.CreatedAt.UTC(),
		record.UpdatedAt.UTC(),
		nullTime(record.DeliveredAt),
		nullTime(record.LastAttemptAt),
	)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			s.observe("create_received", start, ErrAlreadyExists)
			return ErrAlreadyExists
		}
		s.observe("create_received", start, err)
		return err
	}

	s.observe("create_received", start, nil)
	return nil
}

func (s *Store) MarkQueued(ctx context.Context, eventID string, nextRetryAt time.Time) error {
	return s.updateStatus(ctx, "mark_queued", eventID, StatusQueued, nextRetryAt, "")
}

func (s *Store) Requeue(ctx context.Context, eventID string, nextRetryAt time.Time) error {
	start := time.Now()
	result, err := s.db.ExecContext(ctx, `
		UPDATE deliveries
		SET status = ?, next_retry_at = ?, last_error = '', updated_at = ?
		WHERE event_id = ? AND status IN (?, ?)`,
		StatusQueued,
		nextRetryAt.UTC(),
		time.Now().UTC(),
		eventID,
		StatusFailed,
		StatusDeadLetter,
	)
	if err != nil {
		s.observe("requeue", start, err)
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		s.observe("requeue", start, err)
		return err
	}
	if affected == 0 {
		s.observe("requeue", start, ErrNotFound)
		return ErrNotFound
	}

	s.observe("requeue", start, nil)
	return nil
}

func (s *Store) ClaimForSend(ctx context.Context, eventID string, now time.Time) (Record, bool, error) {
	start := time.Now()
	result, err := s.db.ExecContext(ctx, `
		UPDATE deliveries
		SET status = ?, attempt_count = attempt_count + 1, updated_at = ?, last_attempt_at = ?, next_retry_at = NULL
		WHERE event_id = ? AND status IN (?, ?, ?)`,
		StatusSending,
		now.UTC(),
		now.UTC(),
		eventID,
		StatusReceived,
		StatusQueued,
		StatusRetryScheduled,
	)
	if err != nil {
		s.observe("claim_for_send", start, err)
		return Record{}, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		s.observe("claim_for_send", start, err)
		return Record{}, false, err
	}
	if affected == 0 {
		s.observe("claim_for_send", start, nil)
		return Record{}, false, nil
	}
	record, found, err := s.GetByEventID(ctx, eventID)
	s.observe("claim_for_send", start, err)
	return record, found, err
}

func (s *Store) SaveRenderedParts(ctx context.Context, eventID, renderedMessage string, parts []string) error {
	start := time.Now()
	payload, err := json.Marshal(parts)
	if err != nil {
		s.observe("save_rendered_parts", start, err)
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE deliveries
		SET rendered_message = ?, rendered_parts = ?, split_count = ?, updated_at = ?
		WHERE event_id = ?`,
		renderedMessage,
		string(payload),
		len(parts),
		time.Now().UTC(),
		eventID,
	)
	s.observe("save_rendered_parts", start, err)
	return err
}

func (s *Store) MarkChunkSent(ctx context.Context, eventID string, sentParts int, firstMessageID *int64) error {
	start := time.Now()
	if firstMessageID != nil {
		_, err := s.db.ExecContext(ctx, `
			UPDATE deliveries
			SET sent_parts = ?, telegram_message_id = COALESCE(telegram_message_id, ?), updated_at = ?
			WHERE event_id = ?`,
			sentParts,
			*firstMessageID,
			time.Now().UTC(),
			eventID,
		)
		s.observe("mark_chunk_sent", start, err)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE deliveries
		SET sent_parts = ?, updated_at = ?
		WHERE event_id = ?`,
		sentParts,
		time.Now().UTC(),
		eventID,
	)
	s.observe("mark_chunk_sent", start, err)
	return err
}

func (s *Store) MarkDelivered(ctx context.Context, eventID, renderedMessage, chatID string, splitCount int, deliveredAt time.Time) error {
	start := time.Now()
	_, err := s.db.ExecContext(ctx, `
		UPDATE deliveries
		SET status = ?, rendered_message = ?, telegram_chat_id = ?, split_count = ?, sent_parts = ?, delivered_at = ?, updated_at = ?, next_retry_at = NULL, last_error = ''
		WHERE event_id = ?`,
		StatusDelivered,
		renderedMessage,
		chatID,
		splitCount,
		splitCount,
		deliveredAt.UTC(),
		deliveredAt.UTC(),
		eventID,
	)
	s.observe("mark_delivered", start, err)
	return err
}

func (s *Store) ScheduleRetry(ctx context.Context, eventID, renderedMessage, lastError string, nextRetryAt time.Time) error {
	start := time.Now()
	_, err := s.db.ExecContext(ctx, `
		UPDATE deliveries
		SET status = ?, rendered_message = ?, last_error = ?, next_retry_at = ?, updated_at = ?
		WHERE event_id = ?`,
		StatusRetryScheduled,
		renderedMessage,
		lastError,
		nextRetryAt.UTC(),
		time.Now().UTC(),
		eventID,
	)
	s.observe("schedule_retry", start, err)
	return err
}

func (s *Store) MarkFailed(ctx context.Context, eventID, renderedMessage, lastError string) error {
	return s.markTerminal(ctx, "mark_failed", eventID, StatusFailed, renderedMessage, lastError)
}

func (s *Store) MarkDeadLetter(ctx context.Context, eventID, renderedMessage, lastError string) error {
	return s.markTerminal(ctx, "mark_dead_letter", eventID, StatusDeadLetter, renderedMessage, lastError)
}

func (s *Store) ListDueEventIDs(ctx context.Context, now time.Time, limit int) ([]string, error) {
	start := time.Now()
	rows, err := s.db.QueryContext(ctx, `
		SELECT event_id
		FROM deliveries
		WHERE status IN (?, ?, ?)
		  AND (next_retry_at IS NULL OR next_retry_at <= ?)
		ORDER BY created_at ASC
		LIMIT ?`,
		StatusReceived,
		StatusQueued,
		StatusRetryScheduled,
		now.UTC(),
		limit,
	)
	if err != nil {
		s.observe("list_due_event_ids", start, err)
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			s.observe("list_due_event_ids", start, err)
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		s.observe("list_due_event_ids", start, err)
		return nil, err
	}

	s.observe("list_due_event_ids", start, nil)
	return ids, nil
}

func (s *Store) CountQueued(ctx context.Context) (int, error) {
	start := time.Now()
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM deliveries
		WHERE status IN (?, ?, ?, ?)`,
		StatusReceived,
		StatusQueued,
		StatusSending,
		StatusRetryScheduled,
	).Scan(&count)
	s.observe("count_queued", start, err)
	return count, err
}

func (s *Store) OldestQueuedAge(ctx context.Context, now time.Time) (float64, error) {
	start := time.Now()
	var oldest sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT MIN(created_at)
		FROM deliveries
		WHERE status IN (?, ?, ?, ?)`,
		StatusReceived,
		StatusQueued,
		StatusSending,
		StatusRetryScheduled,
	).Scan(&oldest)
	if err != nil {
		s.observe("oldest_queued_age", start, err)
		return 0, err
	}
	s.observe("oldest_queued_age", start, nil)
	if !oldest.Valid {
		return 0, nil
	}
	return now.Sub(oldest.Time).Seconds(), nil
}

func (s *Store) SelectRotatable(ctx context.Context, deliveredBefore, terminalBefore time.Time, limit int) ([]Record, error) {
	start := time.Now()
	rows, err := s.db.QueryContext(ctx, `
		SELECT event_id, idempotency_key, status, original_payload, rendered_message, rendered_parts, split_count, sent_parts,
		       attempt_count, next_retry_at, last_error, telegram_chat_id, telegram_message_id, created_at, updated_at,
		       delivered_at, last_attempt_at
		FROM deliveries
		WHERE (status = ? AND updated_at <= ?)
		   OR (status IN (?, ?) AND updated_at <= ?)
		ORDER BY updated_at ASC
		LIMIT ?`,
		StatusDelivered,
		deliveredBefore.UTC(),
		StatusFailed,
		StatusDeadLetter,
		terminalBefore.UTC(),
		limit,
	)
	if err != nil {
		s.observe("select_rotatable", start, err)
		return nil, err
	}
	defer rows.Close()

	var records []Record
	for rows.Next() {
		record, err := scanRowsRecord(rows)
		if err != nil {
			s.observe("select_rotatable", start, err)
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		s.observe("select_rotatable", start, err)
		return nil, err
	}

	s.observe("select_rotatable", start, nil)
	return records, nil
}

func (s *Store) DeleteByEventIDs(ctx context.Context, eventIDs []string) error {
	start := time.Now()
	if len(eventIDs) == 0 {
		s.observe("delete_by_event_ids", start, nil)
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.observe("delete_by_event_ids", start, err)
		return err
	}

	query := `DELETE FROM deliveries WHERE event_id IN (` + placeholders(len(eventIDs)) + `)`
	args := make([]interface{}, 0, len(eventIDs))
	for _, id := range eventIDs {
		args = append(args, id)
	}

	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		_ = tx.Rollback()
		s.observe("delete_by_event_ids", start, err)
		return err
	}
	if err := tx.Commit(); err != nil {
		s.observe("delete_by_event_ids", start, err)
		return err
	}

	s.observe("delete_by_event_ids", start, nil)
	return nil
}

func (s *Store) Vacuum(ctx context.Context) error {
	start := time.Now()
	_, err := s.db.ExecContext(ctx, `VACUUM`)
	s.observe("vacuum", start, err)
	return err
}

func (s *Store) SizeBytes() (int64, error) {
	var total int64
	for _, path := range []string{s.path, s.path + "-wal", s.path + "-shm"} {
		info, err := os.Stat(path)
		if err == nil {
			total += info.Size()
			continue
		}
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		return 0, err
	}
	return total, nil
}

func (s *Store) updateStatus(ctx context.Context, operation, eventID, status string, nextRetryAt time.Time, lastError string) error {
	start := time.Now()
	_, err := s.db.ExecContext(ctx, `
		UPDATE deliveries
		SET status = ?, next_retry_at = ?, last_error = ?, updated_at = ?
		WHERE event_id = ?`,
		status,
		nextRetryAt.UTC(),
		lastError,
		time.Now().UTC(),
		eventID,
	)
	s.observe(operation, start, err)
	return err
}

func (s *Store) markTerminal(ctx context.Context, operation, eventID, status, renderedMessage, lastError string) error {
	start := time.Now()
	_, err := s.db.ExecContext(ctx, `
		UPDATE deliveries
		SET status = ?, rendered_message = ?, last_error = ?, next_retry_at = NULL, updated_at = ?
		WHERE event_id = ?`,
		status,
		renderedMessage,
		lastError,
		time.Now().UTC(),
		eventID,
	)
	s.observe(operation, start, err)
	return err
}

func (s *Store) observe(operation string, start time.Time, err error) {
	if s.metrics == nil {
		return
	}
	result := "success"
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		result = "error"
	}
	s.metrics.ObserveStoreOperation(operation, result, time.Since(start))
}

func scanRecord(row *sql.Row) (Record, bool, error) {
	record, err := scanRecordValues(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Record{}, false, nil
		}
		return Record{}, false, err
	}
	return record, true, nil
}

func scanRowsRecord(rows *sql.Rows) (Record, error) {
	return scanRecordValues(rows.Scan)
}

func scanRecordValues(scan func(dest ...interface{}) error) (Record, error) {
	var record Record
	var renderedParts string
	var nextRetryAt sql.NullTime
	var telegramMessageID sql.NullInt64
	var deliveredAt sql.NullTime
	var lastAttemptAt sql.NullTime

	err := scan(
		&record.EventID,
		&record.IdempotencyKey,
		&record.Status,
		&record.OriginalPayload,
		&record.RenderedMessage,
		&renderedParts,
		&record.SplitCount,
		&record.SentParts,
		&record.AttemptCount,
		&nextRetryAt,
		&record.LastError,
		&record.TelegramChatID,
		&telegramMessageID,
		&record.CreatedAt,
		&record.UpdatedAt,
		&deliveredAt,
		&lastAttemptAt,
	)
	if err != nil {
		return Record{}, err
	}

	if renderedParts != "" {
		if err := json.Unmarshal([]byte(renderedParts), &record.RenderedParts); err != nil {
			return Record{}, fmt.Errorf("unmarshal rendered parts: %w", err)
		}
	}
	if nextRetryAt.Valid {
		record.NextRetryAt = &nextRetryAt.Time
	}
	if telegramMessageID.Valid {
		value := telegramMessageID.Int64
		record.TelegramMessageID = &value
	}
	if deliveredAt.Valid {
		record.DeliveredAt = &deliveredAt.Time
	}
	if lastAttemptAt.Valid {
		record.LastAttemptAt = &lastAttemptAt.Time
	}

	return record, nil
}

func placeholders(size int) string {
	parts := make([]string, size)
	for idx := range parts {
		parts[idx] = "?"
	}
	return strings.Join(parts, ", ")
}

func nullTime(value *time.Time) interface{} {
	if value == nil {
		return nil
	}
	return value.UTC()
}

func nullInt64(value *int64) interface{} {
	if value == nil {
		return nil
	}
	return *value
}
