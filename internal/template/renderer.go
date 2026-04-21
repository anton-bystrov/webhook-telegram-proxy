package alerttemplate

import (
	"bytes"
	"fmt"
	"html"
	"os"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/anton-bystrov/webhook-telegram-proxy/internal/metrics"
	"github.com/anton-bystrov/webhook-telegram-proxy/internal/models"
)

const MaxTelegramMessageChars = 4096

type Pair struct {
	Key   string
	Value string
}

type AlertData struct {
	Status       string
	Name         string
	Severity     string
	Summary      string
	Description  string
	StartsAt     string
	EndsAt       string
	GeneratorURL string
	SilenceURL   string
	DashboardURL string
	PanelURL     string
	ValueString  string
	Labels       []Pair
	Annotations  []Pair
}

type MessageData struct {
	Receiver          string
	Status            string
	GroupKey          string
	ExternalURL       string
	Title             string
	Message           string
	FiringCount       int
	ResolvedCount     int
	TotalAlerts       int
	TruncatedAlerts   int
	CommonLabels      []Pair
	CommonAnnotations []Pair
	Alerts            []AlertData
	PartIndex         int
	PartCount         int
}

type Renderer struct {
	path    string
	tmpl    *template.Template
	metrics *metrics.Metrics
}

func Load(path string, m *metrics.Metrics) (*Renderer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read alert template: %w", err)
	}

	tmpl, err := template.New(filepathBase(path)).Funcs(template.FuncMap{
		"joinPairs": joinPairs,
		"orDash":    orDash,
	}).Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("parse alert template: %w", err)
	}

	return &Renderer{
		path:    path,
		tmpl:    tmpl,
		metrics: m,
	}, nil
}

func (r *Renderer) Render(data MessageData) (string, error) {
	start := time.Now()
	var buffer bytes.Buffer
	err := r.tmpl.Execute(&buffer, data)
	result := "success"
	if err != nil {
		result = "error"
	}
	if r.metrics != nil {
		r.metrics.ObserveTemplateRender(result, time.Since(start))
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(buffer.String()), nil
}

func (r *Renderer) BuildData(payload models.WebhookPayload) MessageData {
	alerts := make([]AlertData, 0, len(payload.Alerts))
	for _, alert := range payload.Alerts {
		alerts = append(alerts, AlertData{
			Status:       sanitize(alert.Status, 32),
			Name:         sanitize(firstNonEmpty(alert.Labels["alertname"], alert.Fingerprint, "unnamed-alert"), 256),
			Severity:     sanitize(firstNonEmpty(alert.Labels["severity"], "unknown"), 64),
			Summary:      sanitize(firstNonEmpty(alert.Annotations["summary"], alert.Annotations["message"]), 512),
			Description:  sanitize(alert.Annotations["description"], 1024),
			StartsAt:     formatTime(alert.StartsAt),
			EndsAt:       formatTime(alert.EndsAt),
			GeneratorURL: sanitize(alert.GeneratorURL, 512),
			SilenceURL:   sanitize(alert.SilenceURL, 512),
			DashboardURL: sanitize(alert.DashboardURL, 512),
			PanelURL:     sanitize(alert.PanelURL, 512),
			ValueString:  sanitize(alert.ValueString, 512),
			Labels:       sanitizePairs(alert.Labels),
			Annotations:  sanitizePairs(alert.Annotations),
		})
	}

	return MessageData{
		Receiver:          sanitize(payload.Receiver, 128),
		Status:            sanitize(payload.Status, 32),
		GroupKey:          sanitize(payload.GroupKey, 256),
		ExternalURL:       sanitize(payload.ExternalURL, 512),
		Title:             sanitize(payload.Title, 256),
		Message:           sanitize(payload.Message, 1024),
		FiringCount:       payload.FiringCount(),
		ResolvedCount:     payload.ResolvedCount(),
		TotalAlerts:       len(payload.Alerts),
		TruncatedAlerts:   payload.TruncatedAlerts,
		CommonLabels:      sanitizePairs(payload.CommonLabels),
		CommonAnnotations: sanitizePairs(payload.CommonAnnotations),
		Alerts:            alerts,
		PartIndex:         1,
		PartCount:         1,
	}
}

func CloneWithAlerts(data MessageData, alerts []AlertData, partIndex, partCount int) MessageData {
	clone := data
	clone.Alerts = alerts
	clone.TotalAlerts = len(alerts)
	clone.FiringCount = 0
	clone.ResolvedCount = 0
	for _, alert := range alerts {
		if alert.Status == "firing" {
			clone.FiringCount++
			continue
		}
		if alert.Status == "resolved" {
			clone.ResolvedCount++
		}
	}
	clone.PartIndex = partIndex
	clone.PartCount = partCount
	return clone
}

func sanitizePairs(values map[string]string) []Pair {
	if len(values) == 0 {
		return nil
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	pairs := make([]Pair, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, Pair{
			Key:   sanitize(key, 128),
			Value: sanitize(values[key], 512),
		})
	}
	return pairs
}

func sanitize(value string, maxLen int) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	escaped := html.EscapeString(value)
	runes := []rune(escaped)
	if len(runes) <= maxLen {
		return escaped
	}
	if maxLen <= 3 {
		return string(runes[:maxLen])
	}
	return string(runes[:maxLen-3]) + "..."
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func filepathBase(path string) string {
	parts := strings.Split(path, "/")
	return parts[len(parts)-1]
}

func joinPairs(values []Pair) string {
	if len(values) == 0 {
		return ""
	}
	lines := make([]string, 0, len(values))
	for _, value := range values {
		lines = append(lines, fmt.Sprintf("%s=%s", value.Key, value.Value))
	}
	return strings.Join(lines, ", ")
}

func orDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
