# webhook-telegram-proxy

`webhook-telegram-proxy` receives Grafana Unified Alerting webhooks, stores them in a local SQLite database, renders a message from an external template, and sends that message to a Telegram channel or chat.

The project is designed not only to "forward an alert quickly", but to behave predictably in production:

- it persists each event before sending it to Telegram;
- it retries delivery on temporary failures;
- it recovers unfinished deliveries after restart;
- it avoids resending identical webhooks;
- it publishes Prometheus metrics;
- it limits local store growth and safely cleans up old terminal-state records;
- it can run both as a regular service and as a Docker container.

## What This Service Is For

Grafana can send alerts to an arbitrary HTTP endpoint via webhook. This project acts as a reliable bridge between Grafana and Telegram:

1. Grafana sends a webhook to `POST /webhook/grafana`.
2. The service validates authentication and request shape.
3. The event is written to SQLite.
4. A Telegram message is rendered from the webhook payload using a template.
5. The message is sent through the Telegram Bot API.
6. If Telegram is temporarily unavailable, the event stays in the local queue and is retried later.

This is useful when you need more than a simple "receive JSON and call Telegram" handler. The service adds observability, retries, duplicate protection, and recovery after process restarts.

## Features

- `POST /webhook/grafana` for Grafana webhooks
- `GET /health` and `GET /readyz` for service and store readiness checks
- `GET /livez` for process liveness checks
- `GET /metrics` for Prometheus scraping
- persistent SQLite-backed outbox
- `at-least-once` delivery with retry and restart recovery
- deduplication of identical webhooks via fingerprinting
- external Telegram message template via `templates/telegram_alert.tmpl`
- automatic splitting of oversized Telegram messages
- optional Basic Auth for admin endpoints and as a fallback for the webhook
- dedicated webhook protection through `X-Webhook-Secret`
- safer HTTP defaults: request body limit, header size limit, security headers, constant-time secret comparison
- bounded local store with watermark-based safe cleanup
- versioned release pipeline for binaries, `.deb`, `.rpm`, and Docker images
- `CHANGELOG.md` and tag-driven releases based on SemVer

## Delivery Model Limitations

The service implements a practical reliability model, not a magical exactly-once transport.

What it does guarantee:

- a webhook is persisted before delivery is attempted;
- temporary delivery errors are retried;
- pending records are recovered after restart;
- already delivered identical webhooks are not sent again.

What it does not guarantee:

- it cannot confirm that Telegram users actually read a message;
- it cannot honestly provide strict `exactly-once` semantics over the external Telegram Bot API.

For `v1`, a delivery is considered successful when Telegram Bot API returns `ok=true` with a valid `message_id`.

## Requirements

- Go `1.22+` if you want to run the service as a binary
- Docker if you want to run the container image
- Docker Buildx if you want to build multi-platform images locally
- GoReleaser if you want local snapshot packaging without GitHub Actions
- access to the Telegram Bot API
- Grafana with Unified Alerting webhook contact points
- persistent storage for SQLite if you run the service in a container

## Quick Start

### 1. Create a Telegram bot and prepare the target channel

1. Create a bot through `@BotFather` and get `TELEGRAM_BOT_TOKEN`.
2. Add the bot to the target channel or group.
3. Grant the bot permission to post messages.
4. Find the target `TELEGRAM_CHAT_ID`.

For channels this usually looks like `-1001234567890`.

### 2. Prepare configuration

The service does not auto-load `.env`. For local development, export variables into the shell manually or provide them in another way.

Create `.env` from the example:

```bash
cp .env.example .env
```

Minimum configuration to start:

```dotenv
TELEGRAM_BOT_TOKEN=replace-me
TELEGRAM_CHAT_ID=-1001234567890
WEBHOOK_SECRET=change-me
```

Optional Telegram egress overrides:

```dotenv
TELEGRAM_PROXY_URL=socks5://user:pass@proxy.internal.example:1080
TELEGRAM_BASE_URL=https://botapi.internal.example
```

If the service is reachable outside a trusted internal network, enable Basic Auth:

```dotenv
BASIC_AUTH_USERNAME=admin
BASIC_AUTH_PASSWORD=very-strong-password
```

### 3. Start the service

Run locally:

```bash
set -a
source .env
set +a
go run ./cmd/server
```

Or build a binary:

```bash
go build -o bin/webhook-telegram-proxy ./cmd/server
./bin/webhook-telegram-proxy
```

By default the service listens on `0.0.0.0:8080`.

### 4. Connect Grafana

In Grafana:

1. Open `Alerting` -> `Contact points`.
2. Create a new contact point of type `Webhook`.
3. Set the URL:

```text
https://your-host.example/webhook/grafana
```

4. Add a header:

```text
X-Webhook-Secret: change-me
```

5. If `WEBHOOK_SECRET` is configured, Grafana only needs the `X-Webhook-Secret` header.
6. If `WEBHOOK_SECRET` is not configured and Basic Auth is enabled, configure HTTP Basic Auth in Grafana.
7. Keep the default webhook body unless you intentionally change the supported payload structure.

### 5. Verify end to end

Example request:

```bash
curl -X POST http://127.0.0.1:8080/webhook/grafana \
  -H "Content-Type: application/json" \
  -H "X-Webhook-Secret: change-me" \
  -u admin:very-strong-password \
  --data @sample-grafana-payload.json
```

If `WEBHOOK_SECRET` is configured, omit `-u`. If `WEBHOOK_SECRET` is not configured and Basic Auth is enabled, omit the header and keep `-u`.

## Architecture in Plain English

The service has a few simple layers:

- the HTTP layer accepts requests and exposes `health` and `metrics`;
- `AlertService` validates webhook input and handles deduplication;
- `DeliveryService` renders messages, sends them to Telegram, retries failures, and recovers pending work;
- the SQLite store keeps payloads, statuses, errors, message IDs, and delivery metadata;
- the template renderer loads the external template and injects alert data;
- Prometheus metrics expose queue, retry, store, and Telegram API behavior.

## Distribution and Release Assets

Tagged releases publish versioned artifacts for:

- Linux binaries: `amd64`, `arm64`, `armv7`
- macOS binaries: `amd64`, `arm64`
- Windows binary: `amd64`
- Linux packages: `.deb` and `.rpm` for `amd64`, `arm64`, and `armv7`
- Multi-platform Docker image: `linux/amd64`, `linux/arm64`, `linux/arm/v7`

Binary archives, packages, and checksums are attached to the GitHub Release.
Container images are published to:

```text
ghcr.io/anton-bystrov/webhook-telegram-proxy
```

Version tags use Semantic Versioning:

```text
vX.Y.Z
```

Release notes come from GoReleaser git changelog generation, while `CHANGELOG.md`
remains the human-readable project history.

## Running the Service

### Local developer commands

The repository includes a `Makefile` for the most common release and validation
tasks:

```bash
make fmt
make test
make vet
make build
make changelog
make release-check
make snapshot
make docker-build
make docker-buildx
```

What these commands do:

- `make build`: build the current-platform binary into `dist/`
- `make snapshot`: build versioned archives plus `.deb` and `.rpm` packages into `dist/`
- `make release-check`: validate `.goreleaser.yaml`
- `make changelog`: generate a draft changelog file at `dist/CHANGELOG.next.md`
- `make docker-build`: build a local Docker image with the standard Docker builder
- `make docker-buildx`: build a multi-platform OCI image archive at `dist/`

If `goreleaser` is not installed locally, the helper script falls back to
running GoReleaser through Docker.

### Run as a regular binary

Build:

```bash
go build -o bin/webhook-telegram-proxy ./cmd/server
```

Run with environment variables:

```bash
set -a
source .env
set +a
./bin/webhook-telegram-proxy
```

Or use CLI flags:

```bash
./bin/webhook-telegram-proxy \
  --app-host 0.0.0.0 \
  --app-port 8080 \
  --telegram-bot-token "$TELEGRAM_BOT_TOKEN" \
  --telegram-chat-id "$TELEGRAM_CHAT_ID" \
  --telegram-proxy-url "$TELEGRAM_PROXY_URL" \
  --telegram-base-url "$TELEGRAM_BASE_URL" \
  --webhook-secret "$WEBHOOK_SECRET" \
  --alert-template-path templates/telegram_alert.tmpl \
  --store-path data/webhook-telegram-proxy.db
```

CLI flags override environment variables.

### Run with Docker

Build the image:

```bash
docker build -t webhook-telegram-proxy .
```

Run the container:

```bash
docker run --rm \
  -p 8080:8080 \
  --env-file .env \
  -v "$PWD/templates:/app/templates:ro" \
  -v "$PWD/data:/app/data" \
  webhook-telegram-proxy
```

Important notes:

- mount `templates` separately so you can change the template without rebuilding the image;
- mount `data` to persistent storage, otherwise the SQLite file is lost when the container is removed;
- if Basic Auth is enabled, it protects `GET /health`, `GET /readyz`, and `GET /metrics`;
- `GET /livez` is intentionally unauthenticated for liveness probes;
- `POST /webhook/grafana` uses `X-Webhook-Secret` when configured, and falls back to Basic Auth only when the webhook secret is not set.

### Run as a systemd service

The repository includes an example unit file: `packaging/systemd/webhook-telegram-proxy.service`.

One practical setup looks like this:

1. Create a dedicated system user:

```bash
sudo useradd --system --home /opt/webhook-telegram-proxy --shell /usr/sbin/nologin webhook-telegram-proxy
```

2. Place files at:

- binary: `/opt/webhook-telegram-proxy/webhook-telegram-proxy`
- template: `/opt/webhook-telegram-proxy/templates/telegram_alert.tmpl`
- SQLite store: `/var/lib/webhook-telegram-proxy/webhook-telegram-proxy.db`

3. Create an environment file:

```bash
sudo tee /etc/webhook-telegram-proxy.env >/dev/null <<'EOF'
TELEGRAM_BOT_TOKEN=replace-me
TELEGRAM_CHAT_ID=-1001234567890
TELEGRAM_PROXY_URL=socks5://user:pass@proxy.internal.example:1080
TELEGRAM_BASE_URL=
WEBHOOK_SECRET=change-me
BASIC_AUTH_USERNAME=admin
BASIC_AUTH_PASSWORD=very-strong-password
EOF
```

4. Install the unit file:

```bash
sudo cp packaging/systemd/webhook-telegram-proxy.service /etc/systemd/system/
```

5. Reload and start:

```bash
sudo systemctl daemon-reload
sudo systemctl enable webhook-telegram-proxy
sudo systemctl start webhook-telegram-proxy
```

6. Check status:

```bash
sudo systemctl status webhook-telegram-proxy
```

## Configuration

The main settings are listed below. See `.env.example` for a complete example.

### Required settings

| Variable | Flag | Default | Description |
| --- | --- | --- | --- |
| `TELEGRAM_BOT_TOKEN` | `--telegram-bot-token` | none | Telegram bot token |
| `TELEGRAM_CHAT_ID` | `--telegram-chat-id` | none | Target channel, chat, or group ID |
| `TELEGRAM_BASE_URL` | `--telegram-base-url` | empty | Override the Telegram Bot API base URL, for example for a self-hosted `telegram-bot-api` instance |
| `TELEGRAM_PROXY_URL` | `--telegram-proxy-url` | empty | Explicit proxy URL for Telegram Bot API egress. Supported schemes: `http`, `https`, `socks5` |

### Core server settings

| Variable | Flag | Default | Description |
| --- | --- | --- | --- |
| `APP_HOST` | `--app-host` | `0.0.0.0` | HTTP bind host |
| `APP_PORT` | `--app-port` | `8080` | HTTP bind port |
| `LOG_LEVEL` | `--log-level` | `INFO` | Log level |
| `ENVIRONMENT` | `--environment` | `production` | Environment name |
| `ALERT_TEMPLATE_PATH` | `--alert-template-path` | `templates/telegram_alert.tmpl` | Path to the message template |
| `MESSAGE_PARSE_MODE` | `--message-parse-mode` | `HTML` | Only `HTML` is supported in `v1` |

### Security settings

| Variable | Flag | Default | Description |
| --- | --- | --- | --- |
| `WEBHOOK_SECRET` | `--webhook-secret` | empty | Shared secret validated through `X-Webhook-Secret` |
| `BASIC_AUTH_USERNAME` | `--basic-auth-username` | empty | Basic Auth username |
| `BASIC_AUTH_PASSWORD` | `--basic-auth-password` | empty | Basic Auth password |
| `BASIC_AUTH_REALM` | `--basic-auth-realm` | `webhook-telegram-proxy` | Realm used in the Basic Auth challenge |

If only username or only password is set, the service will fail to start. Both values must be provided together.

### HTTP timeouts and limits

| Variable | Flag | Default | Description |
| --- | --- | --- | --- |
| `HTTP_READ_TIMEOUT` | `--http-read-timeout` | `5s` | Request read timeout |
| `HTTP_WRITE_TIMEOUT` | `--http-write-timeout` | `10s` | Response timeout and outbound Telegram timeout |
| `HTTP_SHUTDOWN_TIMEOUT` | `--http-shutdown-timeout` | `10s` | Graceful shutdown timeout |
| `HTTP_IDLE_TIMEOUT` | `--http-idle-timeout` | `60s` | Server idle timeout |
| `MAX_REQUEST_BODY_BYTES` | `--max-request-body-bytes` | `1048576` | Maximum webhook body size |
| `MAX_HEADER_BYTES` | `--max-header-bytes` | `1048576` | Maximum HTTP header size |

### Retry and recovery

| Variable | Flag | Default | Description |
| --- | --- | --- | --- |
| `MAX_DELIVERY_ATTEMPTS` | `--max-delivery-attempts` | `5` | Maximum delivery attempts |
| `RETRY_BASE_DELAY` | `--retry-base-delay` | `2s` | Base exponential backoff delay |
| `RETRY_MAX_DELAY` | `--retry-max-delay` | `2m` | Maximum retry delay |
| `RECOVERY_INTERVAL` | `--recovery-interval` | `15s` | How often pending work is picked up |
| `WORKER_BATCH_SIZE` | `--worker-batch-size` | `32` | Number of queued events processed per recovery tick |

### Local store and safe rotation

| Variable | Flag | Default | Description |
| --- | --- | --- | --- |
| `STORE_PATH` | `--store-path` | `data/webhook-telegram-proxy.db` | SQLite store path |
| `STORE_MAX_SIZE_BYTES` | `--store-max-size-bytes` | `104857600` | Hard store size limit |
| `STORE_ROTATION_ENABLED` | `--store-rotation-enabled` | `true` | Enable safe store cleanup |
| `STORE_ROTATION_HIGH_WATERMARK_BYTES` | `--store-rotation-high-watermark-bytes` | `83886080` | Cleanup starts above this size |
| `STORE_ROTATION_LOW_WATERMARK_BYTES` | `--store-rotation-low-watermark-bytes` | `62914560` | Cleanup target size |
| `STORE_RETENTION_DELIVERED_HOURS` | `--store-retention-delivered-hours` | `168` | Retention for delivered records |
| `STORE_RETENTION_DEAD_LETTER_HOURS` | `--store-retention-dead-letter-hours` | `720` | Retention for `failed` and `dead_letter` records |
| `STORE_ROTATION_INTERVAL` | `--store-rotation-interval` | `1m` | Background cleanup frequency |
| `STORE_VACUUM_AFTER_ROTATION` | `--store-vacuum-after-rotation` | `false` | Run `VACUUM` after cleanup |

## Security Recommendations

For production use, these are good defaults:

1. Always enable `WEBHOOK_SECRET`.
2. Enable `BASIC_AUTH_USERNAME` and `BASIC_AUTH_PASSWORD` if the service is reachable outside a trusted network.
3. Put the service behind HTTPS termination or a reverse proxy.
4. Restrict network access to `/metrics`, `/health`, and `/readyz` when possible, even if Basic Auth is already enabled.
5. Never commit `.env` files or bot tokens to source control.
6. Use a dedicated Telegram bot for this integration.

The application already includes:

- constant-time comparison for secrets;
- `Content-Type: application/json` validation;
- request body size limits;
- header size limits;
- defensive HTTP security headers;
- more careful public error responses with reduced internal leakage.

## Telegram Proxy and Restricted-Region Deployments

The Telegram client supports these outbound modes:

- direct connection with the standard Go transport;
- explicit `http://` proxy;
- explicit `https://` proxy;
- explicit `socks5://` proxy;
- custom `TELEGRAM_BASE_URL` for a self-hosted `telegram-bot-api` endpoint.

Examples:

```dotenv
TELEGRAM_PROXY_URL=http://proxy.internal.example:3128
TELEGRAM_PROXY_URL=https://user:pass@proxy.internal.example:8443
TELEGRAM_PROXY_URL=socks5://user:pass@proxy.internal.example:1080
TELEGRAM_BASE_URL=https://botapi.internal.example
```

Operational notes:

- `TELEGRAM_PROXY_URL` takes precedence over `HTTP_PROXY`, `HTTPS_PROXY`, and related environment variables for Telegram requests;
- proxy credentials are redacted from logs and returned errors;
- bot tokens remain redacted from client errors;
- invalid proxy schemes fail fast during startup instead of silently falling back.

### Why MTProto Is Not Exposed Here

This service sends alerts through the HTTP Telegram Bot API. `MTProto` proxies are designed for the Telegram client protocol and are not a drop-in transport for the Bot API HTTP endpoint used here. For that reason, `mtproto://...` is rejected during startup instead of being accepted as a misleading configuration.

### Recommended Alternatives for Restricted Regions

1. `SOCKS5` proxy.
   Usually the best first option when only Telegram egress needs help.

2. `HTTP` or `HTTPS` proxy.
   A good fit when your environment already standardizes on outbound web proxies.

3. Self-hosted `telegram-bot-api` plus `TELEGRAM_BASE_URL`.
   Useful when you want stronger control over the upstream hop and better local observability.

4. Host-level or network-level egress through `WireGuard` or another VPN.
   Often the cleanest option when multiple services in the same environment need reliable outbound Telegram access.

5. A relay service in another region.
   Good when the monitored environment can reach one controlled HTTPS endpoint but not Telegram directly.

Quick comparison:

- `SOCKS5 proxy`: low operational complexity, good reliability, recommended first option.
- `HTTP/HTTPS proxy`: low-to-medium complexity, strong fit for enterprise networks, but more dependent on proxy policy and TLS inspection behavior.
- `Self-hosted Bot API`: medium complexity, best control over egress and observability, requires extra infrastructure.
- `WireGuard/VPN egress`: medium complexity, best platform-wide reachability story, often cleaner than per-app proxy sprawl.
- `Regional relay`: medium-to-high complexity, flexible and resilient, but introduces another service to secure and operate.

## Grafana Configuration

The service expects the standard Grafana Unified Alerting webhook payload.

Recommended contact point settings:

- `Integration`: `Webhook`
- `URL`: `https://proxy.example/webhook/grafana`
- `HTTP Method`: `POST`
- `Extra Header`: `X-Webhook-Secret: <your-secret>`
- `Authentication`: only if you intentionally run without `WEBHOOK_SECRET` and rely on Basic Auth fallback
- `Custom Payload`: disabled unless you intentionally adapt the payload format

If you radically change the webhook body structure in Grafana, the service may stop extracting alert fields correctly.

## Example Webhook Payload

```json
{
  "receiver": "telegram-proxy",
  "status": "firing",
  "alerts": [
    {
      "status": "firing",
      "labels": {
        "alertname": "HighErrorRate",
        "severity": "critical",
        "service": "payments"
      },
      "annotations": {
        "summary": "Error rate is above threshold",
        "description": "5xx ratio is above 5% for 10m"
      },
      "startsAt": "2026-04-21T18:22:00Z",
      "endsAt": "0001-01-01T00:00:00Z",
      "generatorURL": "https://grafana.example/alerting/grafana/abc/view"
    }
  ],
  "groupLabels": {
    "alertname": "HighErrorRate"
  },
  "commonLabels": {
    "severity": "critical",
    "service": "payments"
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
}
```

## API Endpoints

### `POST /webhook/grafana`

Purpose: accept a Grafana webhook, persist it, try to deliver it to Telegram, and return the processing status.

Request requirements:

- method: `POST`
- `Content-Type: application/json`
- `X-Webhook-Secret` header if `WEBHOOK_SECRET` is configured
- Basic Auth only if `WEBHOOK_SECRET` is not configured and Basic Auth is enabled

Example successful response:

```json
{
  "event_id": "2bd96bb71db50b4d8b7d7f6db4b4100a",
  "idempotency_key": "6bb0d3fb0ba4a6fd0b3c5072ce5fb22b78c1d6d8d8d8f7c1d52e79d6f9dfad43",
  "status": "delivered",
  "duplicate": false
}
```

Possible response codes:

- `200 OK`: delivered successfully, duplicate of an already known event, or terminally finished with `failed` / `dead_letter`
- `202 Accepted`: duplicate of an event that is still being worked on or has been requeued
- `400 Bad Request`: invalid JSON
- `401 Unauthorized`: invalid `X-Webhook-Secret`, or invalid Basic Auth credentials when running in Basic Auth fallback mode
- `413 Payload Too Large`: request body exceeds the configured limit
- `415 Unsupported Media Type`: `Content-Type` is not `application/json`
- `502 Bad Gateway`: Telegram or the network path to Telegram is temporarily unavailable, delivery will be retried
- `503 Service Unavailable`: the local store is full or temporarily unavailable
- `500 Internal Server Error`: internal processing error

Example error response:

```json
{
  "error": "unauthorized",
  "request_id": "e7b3dfe4f9e547f5"
}
```

### `GET /health`

Purpose: confirm that the service is alive, SQLite is reachable, and the store is not under critical pressure.

Example response:

```json
{
  "status": "ok",
  "store_size_bytes": 262144,
  "auth_enabled": true
}
```

`status` meanings:

- `ok`: service is healthy and the store is reachable
- `store_pressure`: store size has crossed a critical threshold
- `unhealthy`: SQLite is unavailable or the health check itself failed

If Basic Auth is enabled, this endpoint is protected.

### `GET /readyz`

Purpose: readiness probe. It uses the same logic and response shape as `GET /health`.

If Basic Auth is enabled, this endpoint is protected.

### `GET /livez`

Purpose: liveness probe. This endpoint only confirms that the process is responsive and does not touch SQLite or Telegram.

Example response:

```json
{
  "status": "ok"
}
```

This endpoint is intentionally never protected by Basic Auth so kubelet-style liveness probes can always reach it.

### `GET /metrics`

Exposes Prometheus metrics. If Basic Auth is enabled, this endpoint is protected as well.

## Delivery Lifecycle and Recovery

The main pipeline looks like this:

1. the webhook enters `received`
2. then it moves to `queued`
3. when picked up, it is claimed as `sending`
4. on success it becomes `delivered`
5. on temporary failure it becomes `retry_scheduled`
6. after retries are exhausted it becomes `dead_letter`
7. on a non-retryable error it becomes `failed`

Delivery statuses used by the store:

- `received`
- `queued`
- `sending`
- `delivered`
- `retry_scheduled`
- `failed`
- `dead_letter`

When an identical webhook is received again, the service computes a fingerprint and avoids duplicate delivery whenever possible.

If a record has already reached `failed` or `dead_letter`, receiving the same webhook again does not automatically requeue it. This prevents an upstream retry loop from turning a terminal delivery outcome into repeated duplicate sends.

## Local Store and Safe Rotation

The service keeps delivery records in SQLite and monitors store size on disk.

The most important rule is that cleanup never removes records that may still be needed for delivery.

Only terminal-state records may be deleted:

- `delivered`
- `failed`
- `dead_letter`

Records in these states are never removed by rotation:

- `received`
- `queued`
- `sending`
- `retry_scheduled`

If the store grows beyond safe limits and cleanup cannot free enough space, the service starts rejecting new webhooks with `503 Service Unavailable`.

## Telegram Message Template

Message formatting lives in a separate file:

```text
templates/telegram_alert.tmpl
```

You can change the path through `ALERT_TEMPLATE_PATH` or `--alert-template-path`.

The template is loaded during startup. If the file is missing or contains invalid syntax, the service will fail to start.

### Template behavior

- uses `text/template`
- supports only `HTML` parse mode
- user-controlled values are escaped before insertion
- large alert batches are automatically split into multiple Telegram messages
- if a single rendered alert still exceeds the Telegram limit, delivery fails

### Top-level template fields

The template receives a `MessageData` structure with these fields:

| Field | Description |
| --- | --- |
| `Receiver` | Grafana receiver name |
| `Status` | overall batch status |
| `GroupKey` | Grafana group key |
| `ExternalURL` | Grafana base URL |
| `Title` | top-level notification title |
| `Message` | top-level notification message |
| `FiringCount` | number of firing alerts |
| `ResolvedCount` | number of resolved alerts |
| `TotalAlerts` | number of alerts in the current rendered part |
| `TruncatedAlerts` | truncated alert count from payload |
| `CommonLabels` | common labels |
| `CommonAnnotations` | common annotations |
| `Alerts` | list of individual alerts |
| `PartIndex` | current message part number |
| `PartCount` | total number of parts |

### Per-alert fields

| Field | Description |
| --- | --- |
| `Status` | alert status |
| `Name` | `alertname` or fallback |
| `Severity` | severity |
| `Summary` | summary |
| `Description` | description |
| `StartsAt` | start time |
| `EndsAt` | end time |
| `GeneratorURL` | alert URL |
| `SilenceURL` | silence URL |
| `DashboardURL` | dashboard URL |
| `PanelURL` | panel URL |
| `ValueString` | value string |
| `Labels` | labels as key/value pairs |
| `Annotations` | annotations as key/value pairs |

### Template helper functions

| Function | Description |
| --- | --- |
| `orDash` | replaces an empty value with `-` |
| `joinPairs` | turns key/value pairs into `key=value, key2=value2` |

### Example template fragment

```gotemplate
<b>Status:</b> {{ orDash .Status }}
<b>Receiver:</b> {{ orDash .Receiver }}
<b>Alerts:</b> {{ .TotalAlerts }} (firing={{ .FiringCount }}, resolved={{ .ResolvedCount }})
{{- range .Alerts }}
<b>Alert:</b> {{ orDash .Name }}
<b>Severity:</b> {{ orDash .Severity }}
{{- if .Summary }}
<b>Summary:</b> {{ .Summary }}
{{- end }}
{{- end }}
```

## Prometheus Metrics

The service exposes `GET /metrics` and publishes both standard Go runtime/process metrics and application metrics.

Main metric groups:

- HTTP:
  - `grafana_telegram_proxy_http_requests_total`
  - `grafana_telegram_proxy_http_request_duration_seconds`
- webhook ingest:
  - `grafana_telegram_proxy_webhook_events_received_total`
  - `grafana_telegram_proxy_webhook_payload_size_bytes`
- delivery:
  - `grafana_telegram_proxy_delivery_attempts_total`
  - `grafana_telegram_proxy_delivery_attempt_duration_seconds`
  - `grafana_telegram_proxy_delivery_queue_messages`
  - `grafana_telegram_proxy_delivery_oldest_queued_message_age_seconds`
  - `grafana_telegram_proxy_delivery_retries_total`
  - `grafana_telegram_proxy_delivery_dead_letter_total`
- template rendering:
  - `grafana_telegram_proxy_template_renders_total`
  - `grafana_telegram_proxy_template_render_duration_seconds`
- store:
  - `grafana_telegram_proxy_store_operations_total`
  - `grafana_telegram_proxy_store_operation_duration_seconds`
  - `grafana_telegram_proxy_store_size_bytes`
  - `grafana_telegram_proxy_store_rotation_runs_total`
  - `grafana_telegram_proxy_store_rotated_records_total`
  - `grafana_telegram_proxy_store_disk_pressure`
  - `grafana_telegram_proxy_store_rejections_total`
- Telegram API:
  - `grafana_telegram_proxy_telegram_api_requests_total`
  - `grafana_telegram_proxy_telegram_api_request_duration_seconds`

Operationally, the most useful things to watch are:

- whether `delivery_queue_messages` keeps growing;
- whether `delivery_oldest_queued_message_age_seconds` increases;
- whether `dead_letter` events start appearing;
- whether `store_disk_pressure=1` remains active.

## Operational Recommendations

1. Keep the SQLite file on persistent storage.
2. Do not use an ephemeral volume if restart recovery matters to you.
3. Do not disable `WEBHOOK_SECRET` in production.
4. Put the service behind HTTPS and ideally behind a reverse proxy or firewall.
5. Configure Prometheus alerting at least for:
   - growing delivery queue;
   - rising dead-letter count;
   - `store_disk_pressure=1`;
   - unhealthy health status.
6. Change message format through `ALERT_TEMPLATE_PATH` instead of modifying code.

## Troubleshooting

### Grafana gets `401 Unauthorized`

Check:

- whether `X-Webhook-Secret` matches;
- if `WEBHOOK_SECRET` is not configured, whether Basic Auth is enabled in the service;
- if running in Basic Auth fallback mode, whether Grafana is using the correct username and password.

### Grafana gets `415 Unsupported Media Type`

The service expects `Content-Type: application/json`. Make sure the webhook is actually sent as JSON.

### Grafana gets `503 Service Unavailable`

Possible reasons:

- the store is unavailable;
- the store is full and safe cleanup could not free enough space.

Check:

- `GET /health`;
- `GET /readyz`;
- the size of the `data/` directory;
- metrics `grafana_telegram_proxy_store_size_bytes` and `grafana_telegram_proxy_store_disk_pressure`.

### Webhooks are accepted but no Telegram messages arrive

Check:

- `TELEGRAM_BOT_TOKEN`;
- `TELEGRAM_CHAT_ID`;
- `TELEGRAM_PROXY_URL` or `TELEGRAM_BASE_URL` if you route Telegram traffic through a proxy or self-hosted Bot API;
- whether the bot was added to the channel;
- whether the bot has permission to post;
- metrics `telegram_api_requests_total`, `delivery_retries_total`, and `delivery_dead_letter_total`.

### The service does not start after editing the template

Check the template syntax. The template is validated during startup and the process exits immediately on parse errors.

### Metrics are unavailable

If Basic Auth is enabled, Prometheus must scrape with valid credentials too.

## Example Local Workflow

```bash
cp .env.example .env
```

Edit `.env`, then run:

```bash
set -a
source .env
set +a
go run ./cmd/server
```

Check health:

```bash
curl -u admin:very-strong-password http://127.0.0.1:8080/health
```

Check liveness:

```bash
curl http://127.0.0.1:8080/livez
```

Check webhook delivery:

```bash
curl -X POST http://127.0.0.1:8080/webhook/grafana \
  -H "Content-Type: application/json" \
  -H "X-Webhook-Secret: change-me" \
  -u admin:very-strong-password \
  --data @sample-grafana-payload.json
```

Check metrics:

```bash
curl -u admin:very-strong-password http://127.0.0.1:8080/metrics
```

## Project Layout

- `cmd/server/main.go` — entrypoint
- `internal/config` — configuration from env vars and CLI flags
- `internal/http` — router, handlers, middleware
- `internal/models` — Grafana webhook payload models
- `internal/service` — webhook acceptance and delivery logic
- `internal/store` — SQLite store and rotation helpers
- `internal/template` — template loading and rendering
- `internal/telegram` — Telegram Bot API client
- `internal/metrics` — Prometheus collectors
- `templates/telegram_alert.tmpl` — default message template
- `packaging/systemd/webhook-telegram-proxy.service` — example systemd unit

## What to Configure First

If this is your first deployment, start with these settings:

1. `TELEGRAM_BOT_TOKEN`
2. `TELEGRAM_CHAT_ID`
3. `WEBHOOK_SECRET`
4. `BASIC_AUTH_USERNAME` and `BASIC_AUTH_PASSWORD`
5. `STORE_PATH` on persistent storage
6. `ALERT_TEMPLATE_PATH` if you want to customize message formatting
7. `TELEGRAM_PROXY_URL` or `TELEGRAM_BASE_URL` if Telegram is restricted in your region or network

## Possible Future Improvements

- multiple Telegram targets with routing by labels
- additional delivery backends such as Slack or Discord
- replay tooling for `dead_letter`
- administrative endpoints
- OpenTelemetry traces
- a dedicated Grafana dashboard for proxy metrics

## License

This project is licensed under the `MIT License`.

MIT is one of the most widely used permissive open-source licenses:

- you can use the code commercially;
- you can modify it;
- you can distribute it;
- you can include it in proprietary software;
- you only need to keep the copyright notice and license text.

See the full license text in [`LICENSE`](LICENSE).
