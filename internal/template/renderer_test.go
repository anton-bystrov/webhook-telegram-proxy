package alerttemplate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
