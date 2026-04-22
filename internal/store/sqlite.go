package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/anton-bystrov/webhook-telegram-proxy/internal/metrics"
)

// Delivery lifecycle states. Terminal states (delivered, failed, dead_letter)
// are the only ones rotation is allowed to touch.
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

// slowOpThreshold is the duration above which we log a warning for a store op.
// SQLite ops on a warm WAL + cached pages should be sub-millisecond; anything
// over 100ms is worth investigating (lock wait, large result set, disk pressure).
const slowOpThreshold = 100 * time.Millisecond

// deleteBatchSize bounds the parameter count in DELETE ... WHERE event_id IN (?, ...).
// SQLite's SQLITE_MAX_VARIABLE_NUMBER defaults to 32766 on 3.32+, but we stay well
// below that so that rotation never hits the limit on very old builds either.
const deleteBatchSize = 500

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

// New opens (or creates) the SQLite database at path, applies schema, and
// returns a Store ready for use.
//
// PRAGMAs are applied via the DSN so they bind to every connection the pool
// opens, not just the first one. MaxOpenConns is pinned to 1 to serialize
// writes and reduce SQLITE_BUSY under concurrent webhook bursts. This is the
// correct setting for a small-writer outbox; if you ever need read concurrency,
// open a second read-only *sql.DB alongside this one.
func New(ctx context.Context, path string, m *metrics.Metrics, logger *slog.Logger) (*Store, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create store directory: %w", err)
	}

	dsn := buildDSN(path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0) // SQLite connections don't benefit from cycling.

	// Fail fast if the DSN or pragmas are malformed rather than at first query.
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply sqlite schema: %w", err)
	}

	return &Store{
		db:      db,
		path:    path,
		metrics: m,
		logger:  logger,
	}, nil
}

// buildDSN composes a modernc.org/sqlite DSN that sets required pragmas on
// every connection the pool opens.
func buildDSN(path string) string {
	q := url.Values{}
	pragmas := []string{
		"busy_timeout(5000)",
		"journal_mode(WAL)",
		"synchronous(NORMAL)",
		"foreign_keys(ON)",
		"temp_store(MEMORY)",
	}
	for _, pragma := range pragmas {
		q.Add("_pragma", pragma)
	}

	u := url.URL{
		Scheme:   "file",
		Path:     path,
		RawQuery: q.Encode(),
	}
	return u.String()
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// CheckpointWAL truncates the WAL file. Call this at the end of each rotation
// run so a long-running reader earlier in the process doesn't leave the WAL
// growing unbounded and triggering a false store_pressure signal.
func (s *Store) CheckpointWAL(ctx context.Context) error {
	start := time.Now()
	_, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	s.observe("checkpoint_wal", start, err)
	return err
}

func (s *Store) GetByIdempotencyKey(ctx context.Context, key string) (Record, bool, error) {
	start := time.Now()
	row := s.db.QueryRowContext(ctx, selectRecordSQL+` WHERE idempotency_key = ?`, key)
	record, found, err := scanRecord(row)
	s.observe("get_by_idempotency_key", start, err)
	return record, found, err
}

func (s *Store) GetByEventID(ctx context.Context, eventID string) (Record, bool, error) {
	start := time.Now()
	row := s.db.QueryRowContext(ctx, selectRecordSQL+` WHERE event_id = ?`, eventID)
	record, found, err := scanRecord(row)
	s.observe("get_by_event_id", start, err)
	return record, found, err
}

func (s *Store) CreateReceived(ctx context.Context, record Record) error {
	start := time.Now()
	parts, err := json.Marshal(record.RenderedParts)
	if err != nil {
		s.observe("create_received", start, err)
		return fmt.Errorf("marshal rendered parts: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO deliveries (
			event_id, idempotency_key, status, original_payload, rendered_message, rendered_parts,
			split_count, sent_parts, attempt_count, next_retry_at, last_error, telegram_chat_id,
			telegram_message_id, created_at, updated_at, delivered_at, last_attempt_at
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
		if isUniqueConstraint(err) {
			s.observe("create_received", start, ErrAlreadyExists)
			return ErrAlreadyExists
		}
		s.observe("create_received", start, err)
		return fmt.Errorf("insert delivery: %w", err)
	}

	s.observe("create_received", start, nil)
	return nil
}

func (s *Store) MarkQueued(ctx context.Context, eventID string, nextRetryAt time.Time) error {
	return s.updateStatus(ctx, "mark_queued", eventID, StatusQueued, nextRetryAt, "")
}

// Requeue moves a record out of a terminal failure state back into the queue.
// attempt_count is reset to 0 so the record gets the full retry budget again;
// without this reset, a record that dead-lettered at MAX_DELIVERY_ATTEMPTS
// would dead-letter again on its first post-replay attempt.
func (s *Store) Requeue(ctx context.Context, eventID string, nextRetryAt time.Time) error {
	start := time.Now()
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `
		UPDATE deliveries
		SET status = ?, next_retry_at = ?, last_error = '', attempt_count = 0, updated_at = ?
		WHERE event_id = ? AND status IN (?, ?)`,
		StatusQueued,
		nextRetryAt.UTC(),
		now,
		eventID,
		StatusFailed,
		StatusDeadLetter,
	)
	if err != nil {
		s.observe("requeue", start, err)
		return fmt.Errorf("requeue: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		s.observe("requeue", start, err)
		return fmt.Errorf("requeue rows affected: %w", err)
	}
	if affected == 0 {
		s.observe("requeue", start, ErrNotFound)
		return ErrNotFound
	}

	s.observe("requeue", start, nil)
	return nil
}

// RequeueSending is a crash-recovery hook: on startup, any record stuck in
// 'sending' is assumed to be an in-flight delivery from the previous process
// incarnation and gets flipped back to 'queued'.
//
// This is where at-least-once semantics become visible: if the previous
// process actually succeeded at Telegram but crashed before calling
// MarkDelivered, the recovered record will be sent a second time.
func (s *Store) RequeueSending(ctx context.Context, nextRetryAt time.Time) (int64, error) {
	start := time.Now()
	result, err := s.db.ExecContext(ctx, `
		UPDATE deliveries
		SET status = ?, next_retry_at = ?, updated_at = ?
		WHERE status = ?`,
		StatusQueued,
		nextRetryAt.UTC(),
		time.Now().UTC(),
		StatusSending,
	)
	if err != nil {
		s.observe("requeue_sending", start, err)
		return 0, fmt.Errorf("requeue sending: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		s.observe("requeue_sending", start, err)
		return 0, fmt.Errorf("requeue sending rows affected: %w", err)
	}

	if affected > 0 && s.logger != nil {
		s.logger.Warn(
			"recovered in-flight deliveries; duplicate Telegram delivery possible",
			"count", affected,
		)
	}

	s.observe("requeue_sending", start, nil)
	return affected, nil
}

// ClaimForSend atomically transitions a record into 'sending' and returns its
// current contents. The status-gate predicate in WHERE ensures two workers
// cannot claim the same record.
func (s *Store) ClaimForSend(ctx context.Context, eventID string, now time.Time) (Record, bool, error) {
	start := time.Now()
	row := s.db.QueryRowContext(ctx, `
		UPDATE deliveries
		SET status = ?,
		    attempt_count = attempt_count + 1,
		    updated_at = ?,
		    last_attempt_at = ?,
		    next_retry_at = NULL
		WHERE event_id = ? AND status IN (?, ?, ?)
		RETURNING `+recordColumns,
		StatusSending,
		now.UTC(),
		now.UTC(),
		eventID,
		StatusReceived,
		StatusQueued,
		StatusRetryScheduled,
	)

	record, found, err := scanRecord(row)
	s.observe("claim_for_send", start, err)
	return record, found, err
}

func (s *Store) SaveRenderedParts(ctx context.Context, eventID, renderedMessage string, parts []string) error {
	start := time.Now()
	payload, err := json.Marshal(parts)
	if err != nil {
		s.observe("save_rendered_parts", start, err)
		return fmt.Errorf("marshal rendered parts: %w", err)
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
	if err != nil {
		return fmt.Errorf("save rendered parts: %w", err)
	}
	return nil
}

func (s *Store) MarkChunkSent(ctx context.Context, eventID string, sentParts int, firstMessageID *int64) error {
	start := time.Now()
	var err error
	if firstMessageID != nil {
		_, err = s.db.ExecContext(ctx, `
			UPDATE deliveries
			SET sent_parts = ?, telegram_message_id = COALESCE(telegram_message_id, ?), updated_at = ?
			WHERE event_id = ?`,
			sentParts,
			*firstMessageID,
			time.Now().UTC(),
			eventID,
		)
	} else {
		_, err = s.db.ExecContext(ctx, `
			UPDATE deliveries
			SET sent_parts = ?, updated_at = ?
			WHERE event_id = ?`,
			sentParts,
			time.Now().UTC(),
			eventID,
		)
	}
	s.observe("mark_chunk_sent", start, err)
	if err != nil {
		return fmt.Errorf("mark chunk sent: %w", err)
	}
	return nil
}

func (s *Store) MarkDelivered(ctx context.Context, eventID, renderedMessage, chatID string, splitCount int, deliveredAt time.Time) error {
	start := time.Now()
	_, err := s.db.ExecContext(ctx, `
		UPDATE deliveries
		SET status = ?, rendered_message = ?, telegram_chat_id = ?, split_count = ?, sent_parts = ?,
		    delivered_at = ?, updated_at = ?, next_retry_at = NULL, last_error = ''
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
	if err != nil {
		return fmt.Errorf("mark delivered: %w", err)
	}
	return nil
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
	if err != nil {
		return fmt.Errorf("schedule retry: %w", err)
	}
	return nil
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
		return nil, fmt.Errorf("list due event ids: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			s.observe("list_due_event_ids", start, err)
			return nil, fmt.Errorf("scan due event id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		s.observe("list_due_event_ids", start, err)
		return nil, fmt.Errorf("iterate due event ids: %w", err)
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
	if err != nil {
		return 0, fmt.Errorf("count queued: %w", err)
	}
	return count, nil
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
		return 0, fmt.Errorf("oldest queued age: %w", err)
	}
	s.observe("oldest_queued_age", start, nil)
	if !oldest.Valid {
		return 0, nil
	}
	return now.Sub(oldest.Time).Seconds(), nil
}

func (s *Store) SelectRotatable(ctx context.Context, deliveredBefore, terminalBefore time.Time, limit int) ([]Record, error) {
	start := time.Now()
	rows, err := s.db.QueryContext(ctx, selectRecordSQL+`
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
		return nil, fmt.Errorf("select rotatable: %w", err)
	}
	defer rows.Close()

	var records []Record
	for rows.Next() {
		record, err := scanRowsRecord(rows)
		if err != nil {
			s.observe("select_rotatable", start, err)
			return nil, fmt.Errorf("scan rotatable: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		s.observe("select_rotatable", start, err)
		return nil, fmt.Errorf("iterate rotatable: %w", err)
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

	for i := 0; i < len(eventIDs); i += deleteBatchSize {
		end := i + deleteBatchSize
		if end > len(eventIDs) {
			end = len(eventIDs)
		}
		batch := eventIDs[i:end]

		args := make([]any, len(batch))
		for j, id := range batch {
			args[j] = id
		}

		query := `DELETE FROM deliveries WHERE event_id IN (` + placeholders(len(batch)) + `)`
		if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
			s.observe("delete_by_event_ids", start, err)
			return fmt.Errorf("delete batch: %w", err)
		}
	}

	s.observe("delete_by_event_ids", start, nil)
	return nil
}

func (s *Store) DeleteTerminalBefore(ctx context.Context, deliveredBefore, terminalBefore time.Time, limit int) (int64, error) {
	start := time.Now()
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM deliveries
		WHERE event_id IN (
			SELECT event_id
			FROM deliveries
			WHERE (status = ? AND updated_at <= ?)
			   OR (status IN (?, ?) AND updated_at <= ?)
			ORDER BY updated_at ASC
			LIMIT ?
		)`,
		StatusDelivered,
		deliveredBefore.UTC(),
		StatusFailed,
		StatusDeadLetter,
		terminalBefore.UTC(),
		limit,
	)
	if err != nil {
		s.observe("delete_terminal_before", start, err)
		return 0, fmt.Errorf("delete terminal before: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		s.observe("delete_terminal_before", start, err)
		return 0, fmt.Errorf("delete terminal before rows affected: %w", err)
	}
	s.observe("delete_terminal_before", start, nil)
	return affected, nil
}

func (s *Store) Vacuum(ctx context.Context) error {
	start := time.Now()
	_, err := s.db.ExecContext(ctx, `VACUUM`)
	s.observe("vacuum", start, err)
	if err != nil {
		return fmt.Errorf("vacuum: %w", err)
	}
	return nil
}

// SizeBytes reports the total on-disk footprint: main DB + WAL + SHM. Ignoring
// WAL/SHM here would underreport during write bursts and mislead disk_pressure.
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
		return 0, fmt.Errorf("stat %s: %w", path, err)
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
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return nil
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
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return nil
}

// observe records store op duration to Prometheus and warns on slow ops.
// ErrAlreadyExists is a normal duplicate-webhook signal, not an error.
func (s *Store) observe(operation string, start time.Time, err error) {
	elapsed := time.Since(start)

	result := "success"
	switch {
	case err == nil || errors.Is(err, sql.ErrNoRows):
		// success
	case errors.Is(err, ErrAlreadyExists):
		result = "duplicate"
	case errors.Is(err, ErrNotFound):
		result = "not_found"
	default:
		result = "error"
	}

	if s.metrics != nil {
		s.metrics.ObserveStoreOperation(operation, result, elapsed)
	}

	if elapsed > slowOpThreshold && s.logger != nil {
		s.logger.Warn(
			"slow store op",
			"operation", operation,
			"duration", elapsed,
			"result", result,
		)
	}
}

func isUniqueConstraint(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}

	code := sqliteErr.Code()
	switch code {
	case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
		return true
	}

	// Fallback for drivers/builds that return only the primary code.
	return code&0xff == sqlite3.SQLITE_CONSTRAINT &&
		strings.Contains(strings.ToLower(sqliteErr.Error()), "unique")
}

// recordColumns is the column list used by every SELECT and the RETURNING
// clause of ClaimForSend. Keeping it in one place prevents drift between
// scan order and SQL column order.
const recordColumns = `event_id, idempotency_key, status, original_payload, rendered_message,
	rendered_parts, split_count, sent_parts, attempt_count, next_retry_at, last_error,
	telegram_chat_id, telegram_message_id, created_at, updated_at, delivered_at, last_attempt_at`

const selectRecordSQL = `SELECT ` + recordColumns + ` FROM deliveries`

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

func scanRecordValues(scan func(dest ...any) error) (Record, error) {
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
	if size <= 0 {
		return ""
	}
	return strings.Repeat("?, ", size-1) + "?"
}

func nullTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC()
}

func nullInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}
