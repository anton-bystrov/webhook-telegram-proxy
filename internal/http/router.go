package transporthttp

import (
	"log/slog"
	"net/http"

	"github.com/anton-bystrov/webhook-telegram-proxy/internal/config"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/metrics"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/service"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/store"
)

// NewRouter builds the HTTP handler tree.
//
// Auth topology:
//
//   - /webhook/grafana authenticates via X-Webhook-Secret when configured.
//     If no webhook secret is set, it falls back to Basic Auth when enabled.
//     This keeps the endpoint from becoming accidentally unauthenticated
//     while still allowing Grafana to use the shared secret alone.
//
//   - /health, /readyz, /metrics form the admin surface. Basic Auth guards
//     them when configured. /livez is never behind auth so kubelet liveness
//     probes can always reach it.
//
// Middleware order (outer to inner):
//
//	recover -> requestID -> log -> securityHeaders -> route
func NewRouter(
	cfg config.Config,
	alerts *service.AlertService,
	st *store.Store,
	m *metrics.Metrics,
	logger *slog.Logger,
) http.Handler {
	handlers := &Handlers{
		cfg:     cfg,
		alerts:  alerts,
		store:   st,
		metrics: m,
		logger:  logger,
	}

	mux := http.NewServeMux()

	adminAuth := func(h http.Handler) http.Handler {
		return basicAuthMiddleware(cfg, h)
	}

	mux.Handle("POST /webhook/grafana",
		handlers.withRoute("webhook_grafana",
			http.HandlerFunc(handlers.webhookGrafana)))

	mux.Handle("GET /health",
		handlers.withRoute("health",
			adminAuth(http.HandlerFunc(handlers.health))))

	mux.Handle("GET /readyz",
		handlers.withRoute("readyz",
			adminAuth(http.HandlerFunc(handlers.health))))

	mux.Handle("GET /livez",
		handlers.withRoute("livez",
			http.HandlerFunc(livenessHandler)))

	mux.Handle("GET /metrics",
		handlers.withRoute("metrics",
			adminAuth(m.Handler())))

	return recoverMiddleware(
		requestIDMiddleware(
			logMiddleware(logger,
				securityHeadersMiddleware(mux),
			),
		),
		logger,
	)
}

// livenessHandler answers /livez. Intentionally minimal: the kubelet liveness
// probe should only restart the process when it is wedged, not when SQLite is
// slow or Telegram is down.
func livenessHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}
