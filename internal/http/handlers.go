package transporthttp

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"

	"github.com/anton-bystrov/webhook-telegram-proxy/internal/config"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/metrics"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/service"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/store"
)

type Handlers struct {
	cfg     config.Config
	alerts  *service.AlertService
	store   *store.Store
	metrics *metrics.Metrics
	logger  *slog.Logger
}

func (h *Handlers) webhookGrafana(w http.ResponseWriter, r *http.Request) {
	if h.cfg.WebhookSecret != "" && !secureCompare(r.Header.Get("X-Webhook-Secret"), h.cfg.WebhookSecret) {
		h.metrics.WebhookEventsReceivedTotal.WithLabelValues("unauthorized").Inc()
		writeError(w, r, http.StatusUnauthorized, "unauthorized")
		return
	}

	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, http.StatusUnsupportedMediaType, "content type must be application/json")
		return
	}

	reader := http.MaxBytesReader(w, r.Body, h.cfg.MaxRequestBodyBytes)
	defer reader.Close()

	body, err := io.ReadAll(reader)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			h.metrics.WebhookEventsReceivedTotal.WithLabelValues("payload_too_large").Inc()
			writeError(w, r, http.StatusRequestEntityTooLarge, "payload exceeds max request size")
			return
		}
		writeError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}
	h.metrics.WebhookPayloadSizeBytes.Observe(float64(len(body)))

	result, statusCode, err := h.alerts.AcceptWebhook(r.Context(), body)
	if err != nil {
		h.logger.Error("webhook processing failed", "error", err, "status_code", statusCode, "request_id", requestIDFromContext(r.Context()))
		writeError(w, r, statusCode, publicErrorMessage(statusCode))
		return
	}

	writeJSON(w, statusCode, result)
}

func (h *Handlers) health(w http.ResponseWriter, r *http.Request) {
	if err := h.store.Ping(r.Context()); err != nil {
		h.logger.Warn("health check failed", "request_id", requestIDFromContext(r.Context()), "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"status": "unhealthy",
		})
		return
	}

	size, err := h.store.SizeBytes()
	if err != nil {
		h.logger.Warn("health store size check failed", "request_id", requestIDFromContext(r.Context()), "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"status": "unhealthy",
		})
		return
	}

	status := http.StatusOK
	healthState := "ok"
	if size > h.cfg.StoreMaxSizeBytes {
		status = http.StatusServiceUnavailable
		healthState = "store_pressure"
	}

	writeJSON(w, status, map[string]interface{}{
		"status":           healthState,
		"store_size_bytes": size,
		"auth_enabled":     h.cfg.BasicAuthEnabled(),
	})
}

func writeJSON(w http.ResponseWriter, statusCode int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, r *http.Request, statusCode int, message string) {
	writeJSON(w, statusCode, map[string]string{
		"error":      message,
		"request_id": requestIDFromContext(r.Context()),
	})
}

func publicErrorMessage(statusCode int) string {
	switch statusCode {
	case http.StatusBadRequest:
		return "invalid webhook payload"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusRequestEntityTooLarge:
		return "payload exceeds max request size"
	case http.StatusUnsupportedMediaType:
		return "content type must be application/json"
	case http.StatusServiceUnavailable:
		return "service temporarily unavailable"
	case http.StatusBadGateway:
		return "delivery temporarily unavailable"
	default:
		return "internal server error"
	}
}

func requireJSONContentType(r *http.Request) error {
	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		return nil
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return err
	}
	if mediaType != "application/json" {
		return errors.New("unsupported content type")
	}
	return nil
}
