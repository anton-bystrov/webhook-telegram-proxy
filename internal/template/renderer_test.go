package alerttemplate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anton-bystrov/webhook-telegram-proxy/internal/models"
)

func TestLoadInvalidTemplate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid.tmpl")
	if err := os.WriteFile(path, []byte("{{ if }"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if _, err := Load(path, nil); err == nil {
		t.Fatal("expected template parsing error")
	}
}

func TestBuildDataPreservesTemplateFriendlyFields(t *testing.T) {
	start := time.Date(2026, 4, 28, 10, 0, 0, 0, time.UTC)
	end := start.Add(15 * time.Minute)

	renderer, err := Load(filepath.Join("..", "..", "templates", "telegram_alert.tmpl"), nil)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	renderer.SetDisplayLocation(time.FixedZone("MSK", 3*60*60))

	data := renderer.BuildData(models.WebhookPayload{
		Status:          "firing",
		Message:         "batch message",
		TruncatedAlerts: 2,
		CommonLabels: map[string]string{
			"environment": "prod",
			"service":     "api",
		},
		Alerts: []models.Alert{
			{
				Status:      "resolved",
				StartsAt:    start,
				EndsAt:      end,
				ValueString: "A=42",
				Labels: map[string]string{
					"alertname": "HighLatency",
					"instance":  "api-1",
					"severity":  "warning",
				},
				Annotations: map[string]string{
					"summary":     "Latency is high",
					"runbook_url": "https://runbooks.example/high-latency",
				},
			},
		},
	})

	if data.TotalAlerts != 3 {
		t.Fatalf("expected TotalAlerts to include truncated alerts, got %d", data.TotalAlerts)
	}
	if got := data.CommonLabels["environment"]; got != "prod" {
		t.Fatalf("expected common environment label, got %q", got)
	}
	if data.StatusIcon != "🔥" {
		t.Fatalf("expected firing status icon, got %q", data.StatusIcon)
	}
	if data.EnvironmentName != "PROD" {
		t.Fatalf("expected normalized environment name, got %q", data.EnvironmentName)
	}
	if data.GroupContext != "service=api, environment=prod" && data.GroupContext != "environment=prod, service=api" {
		t.Fatalf("expected group context, got %q", data.GroupContext)
	}
	if data.Alerts[0].StartsAt != start {
		t.Fatalf("expected StartsAt to remain time.Time, got %v", data.Alerts[0].StartsAt)
	}
	if got := data.Alerts[0].Annotations["runbook_url"]; got == "" {
		t.Fatal("expected annotations map to be available for template indexing")
	}
	if len(data.Alerts[0].WhereLines) == 0 {
		t.Fatal("expected where lines to be precomputed")
	}
	if data.Alerts[0].SinceUTC != "2026-04-28 10:00 UTC" {
		t.Fatalf("expected UTC timestamp, got %q", data.Alerts[0].SinceUTC)
	}
	if data.Alerts[0].SinceLocal != "13:00 MSK" {
		t.Fatalf("expected local timestamp, got %q", data.Alerts[0].SinceLocal)
	}
	if len(data.Alerts[0].ActionLinks) != 1 {
		t.Fatalf("expected one action link, got %d", len(data.Alerts[0].ActionLinks))
	}
}

func TestCloneWithAlertsPreservesTruncatedFooterSemantics(t *testing.T) {
	data := MessageData{
		TruncatedAlerts: 3,
		TotalAlerts:     5,
		Alerts: []AlertData{
			{Status: "firing"},
			{Status: "resolved"},
		},
	}

	clone := CloneWithAlerts(data, data.Alerts[:1], 1, 2)
	if clone.TotalAlerts != 4 {
		t.Fatalf("expected clone total to be current part + truncated alerts, got %d", clone.TotalAlerts)
	}
	if clone.FiringCount != 1 || clone.ResolvedCount != 0 {
		t.Fatalf("unexpected part counters: firing=%d resolved=%d", clone.FiringCount, clone.ResolvedCount)
	}
}

func TestFilterLabelsAndEnvironmentHelpers(t *testing.T) {
	filtered := filterLabels(map[string]string{
		"alertname":   "HighLatency",
		"severity":    "warning",
		"environment": "production",
		"service":     "api",
	})
	if _, exists := filtered["alertname"]; exists {
		t.Fatal("expected alertname to be filtered out")
	}
	if _, exists := filtered["severity"]; exists {
		t.Fatal("expected severity to be filtered out")
	}
	if got := filtered["service"]; got != "api" {
		t.Fatalf("expected service label to remain, got %q", got)
	}
	if got := environmentName(map[string]string{"cluster": "prod-eu1"}); got != "prod" {
		t.Fatalf("expected environment inference from cluster, got %q", got)
	}
}

func TestFilterLabelsDropsExporterPodButKeepsServiceContext(t *testing.T) {
	filtered := filterLabels(map[string]string{
		"pod":      "kube-prometheus-stack-prometheus-node-exporter-abc12",
		"service":  "api",
		"instance": "node-1",
	})
	if _, exists := filtered["pod"]; exists {
		t.Fatal("expected exporter pod label to be filtered out")
	}
	if got := filtered["service"]; got != "api" {
		t.Fatalf("expected service label to remain, got %q", got)
	}
	if got := filtered["instance"]; got != "node-1" {
		t.Fatalf("expected instance label to remain, got %q", got)
	}
}

func TestRenderEscapesAlertValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alert.tmpl")
	if err := os.WriteFile(path, []byte("{{ (index .Alerts 0).Summary }}"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	renderer, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	data := renderer.BuildData(models.WebhookPayload{
		Alerts: []models.Alert{
			{
				Status: "firing",
				Labels: map[string]string{"alertname": "demo"},
				Annotations: map[string]string{
					"summary": "<script>alert('x')</script>",
				},
			},
		},
	})
	rendered, err := renderer.Render(CloneWithAlerts(data, data.Alerts, 1, 1))
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if rendered == "" {
		t.Fatal("expected rendered message")
	}
	if !strings.Contains(rendered, "&lt;script&gt;") {
		t.Fatalf("expected rendered message to be escaped, got %q", rendered)
	}
}

func TestRenderDefaultTemplate(t *testing.T) {
	renderer, err := Load(filepath.Join("..", "..", "templates", "telegram_alert.tmpl"), nil)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	renderer.SetDisplayLocation(time.FixedZone("MSK", 3*60*60))

	start := time.Now().UTC().Add(-12 * time.Minute).Truncate(time.Minute)
	data := renderer.BuildData(models.WebhookPayload{
		Status:  "firing",
		Message: "Investigate immediately",
		CommonLabels: map[string]string{
			"environment": "prod",
			"service":     "payments",
			"alertname":   "DiskFull",
		},
		Alerts: []models.Alert{
			{
				Status:       "firing",
				StartsAt:     start,
				GeneratorURL: "https://grafana.example/alert/1",
				DashboardURL: "https://grafana.example/d/abc",
				PanelURL:     "https://grafana.example/d/abc?viewPanel=7",
				SilenceURL:   "https://alertmanager.example/#/silences/new",
				ValueString:  "A=94.1",
				Labels: map[string]string{
					"alertname": "DiskFull",
					"instance":  "node-1",
					"mount":     "/var",
					"severity":  "critical",
				},
				Annotations: map[string]string{
					"summary":     "Disk usage is above 94%",
					"description": "Filesystem /var is almost full",
					"runbook_url": "https://runbooks.example/disk-full",
				},
			},
		},
	})

	rendered, err := renderer.Render(data)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	expectedFragments := []string{
		"🔥 🟥 <b>PROD</b>",
		"service=payments",
		"Disk usage is above 94%",
		"<b>Where:</b>",
		"- instance: node-1",
		"- mount: /var",
		"<code>A=94.1</code>",
		"UTC (",
		"📕 runbook",
		"📊 graph",
		"📈 dashboard",
		"🔍 panel",
		"🔕 silence",
	}
	for _, fragment := range expectedFragments {
		if !strings.Contains(rendered, fragment) {
			t.Fatalf("expected rendered message to contain %q, got %q", fragment, rendered)
		}
	}
	if strings.Contains(rendered, "alertname=DiskFull") {
		t.Fatalf("expected noisy labels to be filtered out, got %q", rendered)
	}
}
