#!/bin/sh
set -eu

if ! getent group webhook-telegram-proxy >/dev/null 2>&1; then
	groupadd --system webhook-telegram-proxy >/dev/null 2>&1 || true
fi

if ! id -u webhook-telegram-proxy >/dev/null 2>&1; then
	useradd \
		--system \
		--home-dir /var/lib/webhook-telegram-proxy \
		--shell /usr/sbin/nologin \
		--gid webhook-telegram-proxy \
		webhook-telegram-proxy >/dev/null 2>&1 || true
fi

mkdir -p /etc/webhook-telegram-proxy /var/lib/webhook-telegram-proxy
chown webhook-telegram-proxy:webhook-telegram-proxy /var/lib/webhook-telegram-proxy

if command -v systemctl >/dev/null 2>&1; then
	systemctl daemon-reload >/dev/null 2>&1 || true
fi
