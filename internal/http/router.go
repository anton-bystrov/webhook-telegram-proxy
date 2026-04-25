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
//   - /webhook/grafana and /webhook/alertmanager accept either
//     X-Webhook-Secret or Basic Auth when those mechanisms are configured.
//     This keeps Grafana compatible with the shared-secret flow while also
//     giving Alertmanager a practical in-cluster auth option.
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
			http.HandlerFunc(handlers.webhookReceiver)))

	mux.Handle("POST /webhook/alertmanager",
		handlers.withRoute("webhook_alertmanager",
			http.HandlerFunc(handlers.webhookReceiver)))

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
