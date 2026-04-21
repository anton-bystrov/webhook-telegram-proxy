package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/anton-bystrov/webhook-telegram-proxy/internal/metrics"
)

type Client interface {
	SendMessage(ctx context.Context, chatID, text, parseMode string) (SentMessage, error)
}

type SentMessage struct {
	MessageID int64
	ChatID    int64
}

type APIError struct {
	StatusCode  int
	Description string
	Retryable   bool
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram api error: status=%d description=%s", e.StatusCode, e.Description)
}

type HTTPClient struct {
	baseURL string
	token   string
	client  *http.Client
	metrics *metrics.Metrics
}

func NewHTTPClient(token string, timeout time.Duration, m *metrics.Metrics) *HTTPClient {
	return &HTTPClient{
		baseURL: "https://api.telegram.org",
		token:   token,
		client: &http.Client{
			Timeout: timeout,
		},
		metrics: m,
	}
}

func (c *HTTPClient) SendMessage(ctx context.Context, chatID, text, parseMode string) (SentMessage, error) {
	start := time.Now()

	payload := map[string]interface{}{
		"chat_id":                  chatID,
		"text":                     text,
		"parse_mode":               parseMode,
		"disable_web_page_preview": true,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return SentMessage{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/bot"+c.token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return SentMessage{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		c.observe("send_message", classifyNetworkResult(err), time.Since(start))
		return SentMessage{}, &APIError{
			StatusCode:  0,
			Description: err.Error(),
			Retryable:   isRetryableNetworkError(err),
		}
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		c.observe("send_message", "server_error", time.Since(start))
		return SentMessage{}, err
	}

	var payloadResp struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      struct {
			MessageID int64 `json:"message_id"`
			Chat      struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"result"`
	}
	if err := json.Unmarshal(responseBody, &payloadResp); err != nil {
		c.observe("send_message", "server_error", time.Since(start))
		return SentMessage{}, err
	}

	if resp.StatusCode >= 400 || !payloadResp.OK {
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		result := "client_error"
		if retryable {
			result = "server_error"
		}
		c.observe("send_message", result, time.Since(start))
		return SentMessage{}, &APIError{
			StatusCode:  resp.StatusCode,
			Description: strings.TrimSpace(payloadResp.Description),
			Retryable:   retryable,
		}
	}

	c.observe("send_message", "success", time.Since(start))
	return SentMessage{
		MessageID: payloadResp.Result.MessageID,
		ChatID:    payloadResp.Result.Chat.ID,
	}, nil
}

func (c *HTTPClient) observe(operation, result string, duration time.Duration) {
	if c.metrics == nil {
		return
	}
	c.metrics.ObserveTelegramRequest(operation, result, duration)
}

func classifyNetworkResult(err error) string {
	if isRetryableNetworkError(err) {
		return "timeout"
	}
	return "server_error"
}

func isRetryableNetworkError(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout() || netErr.Temporary()
	}
	return true
}
