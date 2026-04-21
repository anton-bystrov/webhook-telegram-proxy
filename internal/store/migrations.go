package store

const schema = `
CREATE TABLE IF NOT EXISTS deliveries (
	event_id TEXT PRIMARY KEY,
	idempotency_key TEXT NOT NULL UNIQUE,
	status TEXT NOT NULL,
	original_payload BLOB NOT NULL,
	rendered_message TEXT NOT NULL DEFAULT '',
	rendered_parts TEXT NOT NULL DEFAULT '[]',
	split_count INTEGER NOT NULL DEFAULT 0,
	sent_parts INTEGER NOT NULL DEFAULT 0,
	attempt_count INTEGER NOT NULL DEFAULT 0,
	next_retry_at TIMESTAMP NULL,
	last_error TEXT NOT NULL DEFAULT '',
	telegram_chat_id TEXT NOT NULL DEFAULT '',
	telegram_message_id INTEGER NULL,
	created_at TIMESTAMP NOT NULL,
	updated_at TIMESTAMP NOT NULL,
	delivered_at TIMESTAMP NULL,
	last_attempt_at TIMESTAMP NULL
);

CREATE INDEX IF NOT EXISTS idx_deliveries_status_retry
	ON deliveries(status, next_retry_at, created_at);

CREATE INDEX IF NOT EXISTS idx_deliveries_updated_at
	ON deliveries(updated_at);
`
