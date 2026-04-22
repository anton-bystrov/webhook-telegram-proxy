ARG GO_VERSION=1.22.12
ARG ALPINE_VERSION=3.20

FROM golang:${GO_VERSION}-alpine AS build

ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
ARG VERSION=dev
ARG REVISION=unknown
ARG BUILD_DATE=unknown

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY templates ./templates

RUN set -eux; \
    TARGET_OS="${TARGETOS:-linux}"; \
    TARGET_ARCH="${TARGETARCH:-amd64}"; \
    TARGET_VARIANT="${TARGETVARIANT:-}"; \
    GOARM=""; \
    if [ "${TARGET_ARCH}" = "arm" ] && [ -n "${TARGET_VARIANT}" ]; then GOARM="${TARGET_VARIANT#v}"; fi; \
    CGO_ENABLED=0 GOOS="${TARGET_OS}" GOARCH="${TARGET_ARCH}" GOARM="${GOARM}" \
      go build -trimpath \
        -ldflags="-s -w -X main.version=${VERSION} -X main.revision=${REVISION} -X main.buildDate=${BUILD_DATE}" \
        -o /out/webhook-telegram-proxy ./cmd/server

FROM alpine:${ALPINE_VERSION}

ARG VERSION=dev
ARG REVISION=unknown
ARG BUILD_DATE=unknown

LABEL org.opencontainers.image.title="webhook-telegram-proxy" \
      org.opencontainers.image.description="Reliable Grafana webhook to Telegram proxy with SQLite-backed delivery queue" \
      org.opencontainers.image.url="https://github.com/anton-bystrov/webhook-telegram-proxy" \
      org.opencontainers.image.source="https://github.com/anton-bystrov/webhook-telegram-proxy" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.created="${BUILD_DATE}"

RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S app && \
    adduser -S -G app -h /app app && \
    mkdir -p /app/templates /app/data && \
    chown -R app:app /app

WORKDIR /app

COPY --from=build /out/webhook-telegram-proxy /usr/local/bin/webhook-telegram-proxy
COPY templates /app/templates

ENV ALERT_TEMPLATE_PATH=/app/templates/telegram_alert.tmpl \
    STORE_PATH=/app/data/webhook-telegram-proxy.db

USER app

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/webhook-telegram-proxy"]
