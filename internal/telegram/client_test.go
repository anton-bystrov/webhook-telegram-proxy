package telegram

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSendMessageWithCustomBaseURL(t *testing.T) {
	t.Helper()

	var requestPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":42,"chat":{"id":-100123}}}`)
	}))
	defer server.Close()

	client, err := NewHTTPClient("token-123", time.Second, nil, WithBaseURL(server.URL))
	if err != nil {
		t.Fatalf("NewHTTPClient() error = %v", err)
	}

	sent, err := client.SendMessage(context.Background(), "-100123", "hello", "HTML")
	if err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}

	if requestPath != "/bottoken-123/sendMessage" {
		t.Fatalf("expected request path %q, got %q", "/bottoken-123/sendMessage", requestPath)
	}
	if sent.MessageID != 42 {
		t.Fatalf("expected message id 42, got %d", sent.MessageID)
	}
}

func TestNewHTTPClientDirectModeKeepsDefaultTransport(t *testing.T) {
	client, err := NewHTTPClient("token-123", time.Second, nil)
	if err != nil {
		t.Fatalf("NewHTTPClient() error = %v", err)
	}

	transport, ok := client.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", client.client.Transport)
	}
	if transport.Proxy == nil {
		t.Fatal("expected environment-based proxy support in direct mode")
	}
	if transport.DialContext == nil {
		t.Fatal("expected default dial context in direct mode")
	}
}

func TestNewHTTPClientHTTPAndHTTPSProxyConfig(t *testing.T) {
	for _, rawURL := range []string{
		"http://user:pass@proxy.internal.example:3128",
		"https://user:pass@proxy.internal.example:8443",
	} {
		client, err := NewHTTPClient("token-123", time.Second, nil, WithProxyURL(rawURL))
		if err != nil {
			t.Fatalf("NewHTTPClient(%q) error = %v", rawURL, err)
		}

		transport, ok := client.client.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("expected *http.Transport, got %T", client.client.Transport)
		}
		if transport.Proxy == nil {
			t.Fatalf("expected explicit proxy function for %q", rawURL)
		}

		req, err := http.NewRequest(http.MethodPost, "https://api.telegram.org/botTOKEN/sendMessage", nil)
		if err != nil {
			t.Fatalf("NewRequest() error = %v", err)
		}
		proxyURL, err := transport.Proxy(req)
		if err != nil {
			t.Fatalf("transport.Proxy() error = %v", err)
		}
		if proxyURL == nil {
			t.Fatalf("expected non-nil proxy URL for %q", rawURL)
		}
		if proxyURL.Scheme == "" || proxyURL.Host == "" {
			t.Fatalf("expected proxy URL scheme and host for %q, got %v", rawURL, proxyURL)
		}
	}
}

func TestNewHTTPClientSOCKS5ProxyConfig(t *testing.T) {
	client, err := NewHTTPClient("token-123", time.Second, nil, WithProxyURL("socks5://user:pass@proxy.internal.example:1080"))
	if err != nil {
		t.Fatalf("NewHTTPClient() error = %v", err)
	}

	transport, ok := client.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", client.client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("expected HTTP proxy function to be disabled for socks5 transport")
	}
	if transport.DialContext == nil {
		t.Fatal("expected socks5 dial context to be configured")
	}
}

func TestNewHTTPClientRejectsUnsupportedProxyScheme(t *testing.T) {
	_, err := NewHTTPClient("token-123", time.Second, nil, WithProxyURL("mtproto://proxy.internal.example:443"))
	if err == nil {
		t.Fatal("expected error for mtproto proxy")
	}
}

func TestNewHTTPClientRejectsInvalidBaseURL(t *testing.T) {
	_, err := NewHTTPClient("token-123", time.Second, nil, WithBaseURL("ftp://botapi.internal.example"))
	if err == nil {
		t.Fatal("expected error for invalid base URL")
	}
}

func TestRedactRemovesProxyCredentials(t *testing.T) {
	client, err := NewHTTPClient("bot-token", time.Second, nil, WithProxyURL("http://user:pass@proxy.internal.example:3128"))
	if err != nil {
		t.Fatalf("NewHTTPClient() error = %v", err)
	}

	redacted := client.redact("proxy failure via http://user:pass@proxy.internal.example:3128 while using bot-token")
	if want := "[REDACTED]@proxy.internal.example:3128"; redacted == "" || !strings.Contains(redacted, want) {
		t.Fatalf("expected redacted proxy URL to contain %q, got %q", want, redacted)
	}
	if strings.Contains(redacted, "user:pass") {
		t.Fatalf("expected proxy credentials to be redacted, got %q", redacted)
	}
	if strings.Contains(redacted, "bot-token") {
		t.Fatalf("expected bot token to be redacted, got %q", redacted)
	}
}
