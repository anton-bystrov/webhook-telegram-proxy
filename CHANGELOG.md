# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and the project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

- Release automation, packaging, and multi-platform distribution pipeline.

## [0.1.0] - 2026-04-22

### Added

- SQLite-backed outbox with retry, restart recovery, and terminal-state rotation.
- Telegram delivery client with retry classification, redacted errors, and Prometheus metrics.
- HTTP ingress with webhook secret support, Basic Auth fallback, `/health`, `/readyz`, `/livez`, and `/metrics`.
- Configurable external alert template and Docker/systemd deployment assets.

[Unreleased]: https://github.com/anton-bystrov/webhook-telegram-proxy/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/anton-bystrov/webhook-telegram-proxy/releases/tag/v0.1.0
