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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/anton-bystrov/webhook-telegram-proxy/internal/metrics"
)

// Default values for the HTTP transport. Tuned for a small service talking
// exclusively to api.telegram.org: one host, steady low-to-medium RPS.
const (
	defaultBaseURL             = "https://api.telegram.org"
	defaultUserAgent           = "webhook-telegram-proxy"
	defaultMaxResponseBytes    = 1 << 20 // 1 MiB — Telegram responses are <5 KB
	defaultDialTimeout         = 5 * time.Second
	defaultKeepAlive           = 30 * time.Second
	defaultTLSHandshakeTimeout = 10 * time.Second
	defaultIdleConnTimeout     = 90 * time.Second
	defaultMaxIdleConns        = 10
	defaultMaxIdleConnsPerHost = 10
	defaultExpectContinue      = 1 * time.Second
)

type Client interface {
	SendMessage(ctx context.Context, chatID, text, parseMode string) (SentMessage, error)
}

type SentMessage struct {
	MessageID int64
	ChatID    int64
}

// APIError surfaces both transport-level and Telegram-level failures.
//
// For transport errors (DNS, TCP, TLS, timeout) StatusCode is 0 and
// Description carries a redacted error message (token stripped). ErrorCode
// is Telegram's own error_code when the body was parseable.
//
// RetryAfter is populated from Telegram's parameters.retry_after or the
// Retry-After HTTP header; it's zero when neither is present. The caller
// should prefer it over local backoff when non-zero.
type APIError struct {
	StatusCode  int
	ErrorCode   int
	Description string
	Retryable   bool
	RetryAfter  time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf(
		"telegram api error: status=%d code=%d description=%s",
		e.StatusCode,
		e.ErrorCode,
		e.Description,
	)
}

// HTTPClient talks to the Telegram Bot API. The token is held only on this
// struct — it's never placed into any error type. All errors that might
// contain the token (via the full request URL in net/http's formatting)
// pass through redactError before leaving this package.
type HTTPClient struct {
	baseURL   string
	token     string
	client    *http.Client
	metrics   *metrics.Metrics
	userAgent string
	maxBody   int64
}

// Option configures an HTTPClient.
type Option func(*HTTPClient)

// WithBaseURL overrides the Telegram API host. Use for tests, mocks, or
// pointing at a self-hosted Bot API server.
func WithBaseURL(url string) Option {
	return func(c *HTTPClient) {
		if url != "" {
			c.baseURL = strings.TrimRight(url, "/")
		}
	}
}

// WithHTTPClient substitutes the underlying *http.Client. Useful for tests
// and for wrapping with instrumentation (e.g. otelhttp).
func WithHTTPClient(hc *http.Client) Option {
	return func(c *HTTPClient) {
		if hc != nil {
			c.client = hc
		}
	}
}

// WithUserAgent sets the User-Agent header. Include the service version so
// Telegram ops can identify your traffic.
func WithUserAgent(ua string) Option {
	return func(c *HTTPClient) {
		if ua != "" {
			c.userAgent = ua
		}
	}
}

// WithMaxResponseBytes caps response body size. Anything larger is treated
// as a decode failure. Default 1 MiB.
func WithMaxResponseBytes(n int64) Option {
	return func(c *HTTPClient) {
		if n > 0 {
			c.maxBody = n
		}
	}
}

// NewHTTPClient constructs a client. Token is required; panics (in test)
// or returns an unusable client (in prod) are avoided — if token is empty
// every SendMessage will fail fast with a non-retryable error.
//
// The supplied timeout is the total request deadline (connect + TLS + send
// + receive). Callers may still pass a tighter ctx deadline; whichever
// fires first wins.
func NewHTTPClient(token string, timeout time.Duration, m *metrics.Metrics, opts ...Option) *HTTPClient {
	c := &HTTPClient{
		baseURL:   defaultBaseURL,
		token:     token,
		metrics:   m,
		userAgent: defaultUserAgent,
		maxBody:   defaultMaxResponseBytes,
	}

	// Default http.Client with a tuned Transport. If the caller supplies one
	// via WithHTTPClient, it wins.
	c.client = &http.Client{
		Timeout:   timeout,
		Transport: defaultTransport(),
	}

	for _, opt := range opts {
		opt(c)
	}
	// Ensure the caller's timeout sticks even if they passed a custom client
	// without setting it.
	if c.client.Timeout == 0 && timeout > 0 {
		c.client.Timeout = timeout
	}

	return c
}

func defaultTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   defaultDialTimeout,
			KeepAlive: defaultKeepAlive,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          defaultMaxIdleConns,
		MaxIdleConnsPerHost:   defaultMaxIdleConnsPerHost,
		IdleConnTimeout:       defaultIdleConnTimeout,
		TLSHandshakeTimeout:   defaultTLSHandshakeTimeout,
		ExpectContinueTimeout: defaultExpectContinue,
	}
}

// sendMessageRequest is the typed body for POST /sendMessage. Using a struct
// instead of map[string]any gives us compile-time protection against field
// name typos and makes the supported options self-documenting.
type sendMessageRequest struct {
	ChatID                string `json:"chat_id"`
	Text                  string `json:"text"`
	ParseMode             string `json:"parse_mode,omitempty"`
	DisableWebPagePreview bool   `json:"disable_web_page_preview"`
}

// sendMessageResponse models the Telegram envelope. Parameters carries the
// retry_after on 429 and migrate_to_chat_id on chat upgrade; ErrorCode is
// Telegram's own numeric code (usually equal to the HTTP status for errors).
type sendMessageResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
	ErrorCode   int    `json:"error_code"`
	Parameters  struct {
		RetryAfter      int   `json:"retry_after"`
		MigrateToChatID int64 `json:"migrate_to_chat_id"`
	} `json:"parameters"`
	Result struct {
		MessageID int64 `json:"message_id"`
		Chat      struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	} `json:"result"`
}

func (c *HTTPClient) SendMessage(ctx context.Context, chatID, text, parseMode string) (SentMessage, error) {
	start := time.Now()

	if c.token == "" {
		c.observe("send_message", "client_error", time.Since(start))
		return SentMessage{}, &APIError{
			StatusCode:  0,
			Description: "telegram bot token is not configured",
			Retryable:   false,
		}
	}

	body, err := json.Marshal(sendMessageRequest{
		ChatID:                chatID,
		Text:                  text,
		ParseMode:             parseMode,
		DisableWebPagePreview: true,
	})
	if err != nil {
		c.observe("send_message", "client_error", time.Since(start))
		return SentMessage{}, fmt.Errorf("marshal request: %w", err)
	}

	// Build URL once; token is only present here and is redacted from any
	// error we ever return to the caller.
	url := c.baseURL + "/bot" + c.token + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		c.observe("send_message", "client_error", time.Since(start))
		return SentMessage{}, c.redactError(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.client.Do(req)
	if err != nil {
		retryable, reason := classifyNetworkError(err)
		c.observe("send_message", reason, time.Since(start))
		return SentMessage{}, &APIError{
			StatusCode:  0,
			Description: c.redact(err.Error()),
			Retryable:   retryable,
		}
	}
	defer func() {
		// Drain before close so the connection can be reused from the pool
		// even if we didn't read the whole body (e.g., body exceeded maxBody).
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	limit := c.maxBody
	if limit <= 0 {
		limit = defaultMaxResponseBytes
	}
	responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if readErr != nil {
		c.observe("send_message", "network", time.Since(start))
		return SentMessage{}, &APIError{
			StatusCode:  resp.StatusCode,
			Description: c.redact(readErr.Error()),
			Retryable:   true,
		}
	}
	if int64(len(responseBody)) > limit {
		c.observe("send_message", "decode_error", time.Since(start))
		return SentMessage{}, &APIError{
			StatusCode:  resp.StatusCode,
			Description: "response body exceeds size limit",
			Retryable:   true,
		}
	}

	var parsed sendMessageResponse
	unmarshalErr := json.Unmarshal(responseBody, &parsed)

	// If the HTTP status is 2xx but the Telegram envelope indicates an error,
	// prefer the Telegram error code for classification and retry decisions.
	statusCode := resp.StatusCode
	if statusCode < 400 && !parsed.OK && parsed.ErrorCode != 0 {
		statusCode = parsed.ErrorCode
	}

	// Build RetryAfter from whichever source speaks: Telegram's body field
	// is the authoritative one on 429; fall back to the HTTP header for
	// intermediary proxies that set it.
	retryAfter := time.Duration(parsed.Parameters.RetryAfter) * time.Second
	if retryAfter == 0 {
		retryAfter = parseRetryAfterHeader(resp.Header.Get("Retry-After"))
	}

	// Non-2xx OR ok=false => error. This also covers the case where the body
	// failed to parse (malformed JSON from an upstream proxy HTML page) but
	// the status tells us it was a server-side failure.
	if resp.StatusCode >= 400 || !parsed.OK || unmarshalErr != nil {
		apiErr := c.buildAPIError(statusCode, parsed, unmarshalErr, retryAfter)
		c.observe("send_message", resultLabelFor(statusCode, parsed.OK, unmarshalErr), time.Since(start))
		return SentMessage{}, apiErr
	}

	c.observe("send_message", "success", time.Since(start))
	return SentMessage{
		MessageID: parsed.Result.MessageID,
		ChatID:    parsed.Result.Chat.ID,
	}, nil
}

// buildAPIError consolidates the error construction paths so body/parse
// failures and HTTP errors produce consistent APIError values.
func (c *HTTPClient) buildAPIError(statusCode int, parsed sendMessageResponse, unmarshalErr error, retryAfter time.Duration) *APIError {
	description := strings.TrimSpace(parsed.Description)
	if description == "" && unmarshalErr != nil {
		description = "unparseable response body"
	}
	if description == "" {
		description = http.StatusText(statusCode)
	}
	if description == "" {
		description = "telegram error"
	}

	// Retryable: 429 always, 5xx always, decode failures on otherwise successful
	// (2xx/3xx) responses. Client 4xx errors other than 429 are terminal.
	retryable := false
	switch {
	case statusCode == http.StatusTooManyRequests:
		retryable = true
	case statusCode >= 500:
		retryable = true
	case unmarshalErr != nil && statusCode < 400:
		retryable = true
	}

	return &APIError{
		StatusCode:  statusCode,
		ErrorCode:   parsed.ErrorCode,
		Description: c.redact(description),
		Retryable:   retryable,
		RetryAfter:  retryAfter,
	}
}

// parseRetryAfterHeader handles both integer-seconds and HTTP-date forms.
func parseRetryAfterHeader(h string) time.Duration {
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// resultLabelFor maps a response into a metrics result label. Throttled gets
// its own bucket so alerting can distinguish Telegram's "slow down" signal
// from generic server-side errors.
func resultLabelFor(statusCode int, ok bool, decodeErr error) string {
	switch {
	case statusCode == http.StatusTooManyRequests:
		return "throttled"
	case statusCode >= 500:
		return "server_error"
	case statusCode >= 400:
		return "client_error"
	case decodeErr != nil:
		return "decode_error"
	case !ok:
		return "client_error"
	default:
		return "success"
	}
}

// classifyNetworkError decides whether a transport-level error is worth
// retrying and returns a metric label. Timeouts and transient networking
// issues are retryable; context cancellation from the caller (shutdown) is
// not — delivery.go already handles that case separately, but we label it
// so dashboards don't misattribute it as a Telegram outage.
func classifyNetworkError(err error) (retryable bool, metricLabel string) {
	if err == nil {
		return false, "success"
	}

	// Caller-initiated cancel — not Telegram's fault.
	if errors.Is(err, context.Canceled) {
		return false, "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true, "timeout"
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true, "timeout"
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		// DNS failures are almost always transient (resolver blip, upstream hiccup).
		return true, "dns"
	}

	// Connection-level errors: refused, reset, broken pipe.
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true, "network"
	}

	var syscallErr syscall.Errno
	if errors.As(err, &syscallErr) {
		switch syscallErr {
		case syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.EPIPE, syscall.ETIMEDOUT:
			return true, "network"
		}
	}

	// Unknown error classes: retry once, but label as unknown so operators
	// notice if a new error class starts flaring.
	return true, "unknown"
}

// redact strips the bot token from any string. net/http error formatting
// includes the full request URL — which includes the token in the path —
// so any error from http.Client.Do leaks the credential unless we scrub it.
func (c *HTTPClient) redact(s string) string {
	if c.token == "" || !strings.Contains(s, c.token) {
		return s
	}
	return strings.ReplaceAll(s, c.token, "[REDACTED]")
}

// redactError wraps an error so Error() returns a sanitized message while
// preserving errors.Is / errors.As behavior on the original.
func (c *HTTPClient) redactError(err error) error {
	if err == nil {
		return nil
	}
	return &redactedError{err: err, token: c.token}
}

type redactedError struct {
	err   error
	token string
}

func (r *redactedError) Error() string {
	msg := r.err.Error()
	if r.token == "" {
		return msg
	}
	return strings.ReplaceAll(msg, r.token, "[REDACTED]")
}

func (r *redactedError) Unwrap() error { return r.err }

func (c *HTTPClient) observe(operation, result string, duration time.Duration) {
	if c.metrics == nil {
		return
	}
	c.metrics.ObserveTelegramRequest(operation, result, duration)
}
