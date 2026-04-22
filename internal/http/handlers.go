package transporthttp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"time"

	"github.com/anton-bystrov/webhook-telegram-proxy/internal/config"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/metrics"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/service"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/store"
)

// healthPingTimeout bounds how long /health and /readyz will block on SQLite.
// Kubelet liveness probes default to 1s; if we block longer than that behind a
// slow write, the probe fails and the pod gets killed even though the DB is
// merely busy, not unhealthy.
const healthPingTimeout = 500 * time.Millisecond

type Handlers struct {
	cfg     config.Config
	alerts  *service.AlertService
	store   *store.Store
	metrics *metrics.Metrics
	logger  *slog.Logger
}

// healthResponse and errorResponse are typed to avoid map[string]any and catch
// field-name typos at compile time.
type healthResponse struct {
	Status         string `json:"status"`
	StoreSizeBytes int64  `json:"store_size_bytes,omitempty"`
	AuthEnabled    bool   `json:"auth_enabled"`
}

type errorResponse struct {
	Error     string `json:"error"`
	RequestID string `json:"request_id,omitempty"`
}

func (h *Handlers) webhookGrafana(w http.ResponseWriter, r *http.Request) {
	// Webhook authentication:
	// 1. If WEBHOOK_SECRET is configured, the request must present it.
	// 2. If no webhook secret exists, fall back to Basic Auth when enabled.
	// 3. If neither auth layer is configured, the endpoint remains open for
	//    compatibility with the current service contract.
	switch {
	case h.cfg.WebhookSecret != "":
		if !secureCompare(r.Header.Get("X-Webhook-Secret"), h.cfg.WebhookSecret) {
			h.metrics.WebhookEventsReceivedTotal.WithLabelValues("unauthorized").Inc()
			writePublicError(w, r, http.StatusUnauthorized, "unauthorized")
			return
		}
	case h.cfg.BasicAuthEnabled():
		username, password, ok := r.BasicAuth()
		usernameOK := secureCompare(username, h.cfg.BasicAuthUsername)
		passwordOK := secureCompare(password, h.cfg.BasicAuthPassword)
		if !ok || !usernameOK || !passwordOK {
			h.metrics.WebhookEventsReceivedTotal.WithLabelValues("unauthorized").Inc()
			writePublicError(w, r, http.StatusUnauthorized, "unauthorized")
			return
		}
	}

	if err := requireJSONContentType(r); err != nil {
		h.metrics.WebhookEventsReceivedTotal.WithLabelValues("unsupported_media_type").Inc()
		writePublicError(w, r, http.StatusUnsupportedMediaType, "content type must be application/json")
		return
	}

	reader := http.MaxBytesReader(w, r.Body, h.cfg.MaxRequestBodyBytes)
	defer reader.Close()

	body, err := io.ReadAll(reader)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			h.metrics.WebhookEventsReceivedTotal.WithLabelValues("payload_too_large").Inc()
			writePublicError(w, r, http.StatusRequestEntityTooLarge, "payload exceeds max request size")
			return
		}
		h.logger.Debug("failed to read request body",
			"request_id", requestIDFromContext(r.Context()),
			"error", err,
		)
		writePublicError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}
	h.metrics.WebhookPayloadSizeBytes.Observe(float64(len(body)))

	result, statusCode, err := h.alerts.AcceptWebhook(r.Context(), body)
	if err != nil {
		h.logger.Error("webhook processing failed",
			"error", err,
			"status_code", statusCode,
			"request_id", requestIDFromContext(r.Context()),
		)
		writePublicError(w, r, statusCode, publicErrorMessage(statusCode))
		return
	}

	writeJSON(w, r, statusCode, result)
}

func (h *Handlers) health(w http.ResponseWriter, r *http.Request) {
	// Short-deadline ctx so /health can't stall behind a slow DB write.
	ctx, cancel := context.WithTimeout(r.Context(), healthPingTimeout)
	defer cancel()

	if err := h.store.Ping(ctx); err != nil {
		h.logger.Warn("health check ping failed",
			"request_id", requestIDFromContext(r.Context()),
			"error", err,
		)
		writeJSON(w, r, http.StatusServiceUnavailable, healthResponse{
			Status:      "unhealthy",
			AuthEnabled: h.cfg.BasicAuthEnabled(),
		})
		return
	}

	size, err := h.store.SizeBytes()
	if err != nil {
		h.logger.Warn("health store size check failed",
			"request_id", requestIDFromContext(r.Context()),
			"error", err,
		)
		writeJSON(w, r, http.StatusServiceUnavailable, healthResponse{
			Status:      "unhealthy",
			AuthEnabled: h.cfg.BasicAuthEnabled(),
		})
		return
	}

	status := http.StatusOK
	healthState := "ok"
	if size > h.cfg.StoreMaxSizeBytes {
		status = http.StatusServiceUnavailable
		healthState = "store_pressure"
	}

	writeJSON(w, r, status, healthResponse{
		Status:         healthState,
		StoreSizeBytes: size,
		AuthEnabled:    h.cfg.BasicAuthEnabled(),
	})
}

// writeJSON marshals and writes a JSON response.
func writeJSON(w http.ResponseWriter, r *http.Request, statusCode int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		_ = err
		_ = r
	}
}

// writePublicError writes an error response with a pre-sanitized message.
func writePublicError(w http.ResponseWriter, r *http.Request, statusCode int, message string) {
	writeJSON(w, r, statusCode, errorResponse{
		Error:     message,
		RequestID: requestIDFromContext(r.Context()),
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
		return errors.New("missing content-type header")
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
