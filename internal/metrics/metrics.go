package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const namespace = "grafana_telegram_proxy"

type Metrics struct {
	registry *prometheus.Registry

	BuildInfo                      *prometheus.GaugeVec
	HTTPRequestsTotal              *prometheus.CounterVec
	HTTPRequestDuration            *prometheus.HistogramVec
	WebhookEventsReceivedTotal     *prometheus.CounterVec
	WebhookPayloadSizeBytes        prometheus.Histogram
	DeliveryAttemptsTotal          *prometheus.CounterVec
	DeliveryAttemptDuration        *prometheus.HistogramVec
	DeliveryQueueMessages          prometheus.Gauge
	DeliveryOldestQueuedMessageAge prometheus.Gauge
	DeliveryRetriesTotal           *prometheus.CounterVec
	DeliveryDeadLetterTotal        prometheus.Counter
	TemplateRendersTotal           *prometheus.CounterVec
	TemplateRenderDuration         prometheus.Histogram
	StoreOperationsTotal           *prometheus.CounterVec
	StoreOperationDuration         *prometheus.HistogramVec
	TelegramAPIRequestsTotal       *prometheus.CounterVec
	TelegramAPIRequestDuration     *prometheus.HistogramVec
	StoreSizeBytes                 prometheus.Gauge
	StoreRotationRunsTotal         *prometheus.CounterVec
	StoreRotatedRecordsTotal       *prometheus.CounterVec
	StoreDiskPressure              prometheus.Gauge
	StoreRejectionsTotal           *prometheus.CounterVec
}

func New(version, revision string) (*Metrics, error) {
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	m := &Metrics{
		registry: registry,
		BuildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "build_info",
			Help:      "Static build information.",
		}, []string{"version", "revision"}),
		HTTPRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "http_requests_total",
			Help:      "Total number of HTTP requests.",
		}, []string{"route", "method", "status_class"}),
		HTTPRequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "http_request_duration_seconds",
			Help:      "Duration of HTTP requests.",
			Buckets:   []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5},
		}, []string{"route", "method"}),
		WebhookEventsReceivedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "webhook_events_received_total",
			Help:      "Number of received webhook events.",
		}, []string{"result"}),
		WebhookPayloadSizeBytes: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "webhook_payload_size_bytes",
			Help:      "Size of incoming webhook payloads in bytes.",
			Buckets:   []float64{256, 512, 1024, 4096, 8192, 16384, 65536, 262144, 1048576},
		}),
		DeliveryAttemptsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "delivery_attempts_total",
			Help:      "Number of delivery attempts to Telegram.",
		}, []string{"result"}),
		DeliveryAttemptDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "delivery_attempt_duration_seconds",
			Help:      "Duration of delivery attempts.",
			Buckets:   []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10},
		}, []string{"result"}),
		DeliveryQueueMessages: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "delivery_queue_messages",
			Help:      "Current number of queued or retryable delivery messages.",
		}),
		DeliveryOldestQueuedMessageAge: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "delivery_oldest_queued_message_age_seconds",
			Help:      "Age of the oldest queued or retryable message in seconds.",
		}),
		DeliveryRetriesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "delivery_retries_total",
			Help:      "Number of scheduled delivery retries.",
		}, []string{"reason_class"}),
		DeliveryDeadLetterTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "delivery_dead_letter_total",
			Help:      "Number of messages moved to dead-letter.",
		}),
		TemplateRendersTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "template_renders_total",
			Help:      "Total number of alert template renders.",
		}, []string{"result"}),
		TemplateRenderDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "template_render_duration_seconds",
			Help:      "Duration of alert template rendering.",
			Buckets:   []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1},
		}),
		StoreOperationsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "store_operations_total",
			Help:      "Total number of persistent store operations.",
		}, []string{"operation", "result"}),
		StoreOperationDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "store_operation_duration_seconds",
			Help:      "Duration of persistent store operations.",
			Buckets:   []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5},
		}, []string{"operation", "result"}),
		TelegramAPIRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "telegram_api_requests_total",
			Help:      "Total number of Telegram API requests.",
		}, []string{"operation", "result"}),
		TelegramAPIRequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "telegram_api_request_duration_seconds",
			Help:      "Duration of Telegram API requests.",
			Buckets:   []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10},
		}, []string{"operation", "result"}),
		StoreSizeBytes: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "store_size_bytes",
			Help:      "Current persistent store size in bytes.",
		}),
		StoreRotationRunsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "store_rotation_runs_total",
			Help:      "Number of store rotation runs.",
		}, []string{"result"}),
		StoreRotatedRecordsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "store_rotated_records_total",
			Help:      "Number of rotated records by terminal state.",
		}, []string{"state"}),
		StoreDiskPressure: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "store_disk_pressure",
			Help:      "Whether the store is under disk pressure.",
		}),
		StoreRejectionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "store_rejections_total",
			Help:      "Number of webhook requests rejected because of store pressure.",
		}, []string{"reason"}),
	}

	for _, collector := range []prometheus.Collector{
		m.BuildInfo,
		m.HTTPRequestsTotal,
		m.HTTPRequestDuration,
		m.WebhookEventsReceivedTotal,
		m.WebhookPayloadSizeBytes,
		m.DeliveryAttemptsTotal,
		m.DeliveryAttemptDuration,
		m.DeliveryQueueMessages,
		m.DeliveryOldestQueuedMessageAge,
		m.DeliveryRetriesTotal,
		m.DeliveryDeadLetterTotal,
		m.TemplateRendersTotal,
		m.TemplateRenderDuration,
		m.StoreOperationsTotal,
		m.StoreOperationDuration,
		m.TelegramAPIRequestsTotal,
		m.TelegramAPIRequestDuration,
		m.StoreSizeBytes,
		m.StoreRotationRunsTotal,
		m.StoreRotatedRecordsTotal,
		m.StoreDiskPressure,
		m.StoreRejectionsTotal,
	} {
		if err := registry.Register(collector); err != nil {
			return nil, err
		}
	}

	m.BuildInfo.WithLabelValues(version, revision).Set(1)
	return m, nil
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{Registry: m.registry})
}

func (m *Metrics) ObserveHTTPRequest(route, method string, statusCode int, duration time.Duration) {
	statusClass := strconv.Itoa(statusCode / 100)
	m.HTTPRequestsTotal.WithLabelValues(route, method, statusClass+"xx").Inc()
	m.HTTPRequestDuration.WithLabelValues(route, method).Observe(duration.Seconds())
}

func (m *Metrics) ObserveStoreOperation(operation, result string, duration time.Duration) {
	m.StoreOperationsTotal.WithLabelValues(operation, result).Inc()
	m.StoreOperationDuration.WithLabelValues(operation, result).Observe(duration.Seconds())
}

func (m *Metrics) ObserveTelegramRequest(operation, result string, duration time.Duration) {
	m.TelegramAPIRequestsTotal.WithLabelValues(operation, result).Inc()
	m.TelegramAPIRequestDuration.WithLabelValues(operation, result).Observe(duration.Seconds())
}

func (m *Metrics) ObserveDeliveryAttempt(result string, duration time.Duration) {
	m.DeliveryAttemptsTotal.WithLabelValues(result).Inc()
	m.DeliveryAttemptDuration.WithLabelValues(result).Observe(duration.Seconds())
}

func (m *Metrics) ObserveTemplateRender(result string, duration time.Duration) {
	m.TemplateRendersTotal.WithLabelValues(result).Inc()
	m.TemplateRenderDuration.Observe(duration.Seconds())
}
