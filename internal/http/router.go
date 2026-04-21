package transporthttp

import (
	"log/slog"
	"net/http"

	"github.com/anton-bystrov/webhook-telegram-proxy/internal/config"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/metrics"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/service"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/store"
)

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
	mux.Handle("GET /health", handlers.withRoute("health", basicAuthMiddleware(cfg, http.HandlerFunc(handlers.health))))
	mux.Handle("GET /metrics", handlers.withRoute("metrics", basicAuthMiddleware(cfg, m.Handler())))
	mux.Handle("POST /webhook/grafana", handlers.withRoute("webhook_grafana", basicAuthMiddleware(cfg, http.HandlerFunc(handlers.webhookGrafana))))

	return requestIDMiddleware(
		securityHeadersMiddleware(
			recoverMiddleware(
				logMiddleware(logger,
					mux,
				),
				logger,
			),
		),
	)
}
