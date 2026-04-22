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
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/proxy"

	"github.com/anton-bystrov/webhook-telegram-proxy/internal/metrics"
)

// Default values for the HTTP transport. Tuned for a small service talking
// exclusively to the Telegram Bot API: one host, steady low-to-medium RPS.
const (
	defaultBaseURL             = "https://api.telegram.org"
	defaultUserAgent           = "webhook-telegram-proxy"
	defaultMaxResponseBytes    = 1 << 20 // 1 MiB — Telegram responses are normally tiny.
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
// Description carries a redacted error message. ErrorCode is Telegram's own
// error_code when the body was parseable.
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

// HTTPClient talks to the Telegram Bot API.
type HTTPClient struct {
	baseURL            string
	token              string
	client             *http.Client
	metrics            *metrics.Metrics
	userAgent          string
	maxBody            int64
	proxyURLRaw        string
	customHTTPClient   bool
	sensitiveReplacers []stringReplacer
}

type stringReplacer struct {
	old string
	new string
}

// Option configures an HTTPClient.
type Option func(*HTTPClient)

// WithBaseURL overrides the Telegram API host. Use for tests, mocks, or
// pointing at a self-hosted Bot API server.
func WithBaseURL(rawURL string) Option {
	return func(c *HTTPClient) {
		if rawURL != "" {
			c.baseURL = strings.TrimSpace(rawURL)
		}
	}
}

// WithProxyURL configures an explicit egress proxy for Telegram Bot API
// requests. Supported schemes are http, https, and socks5.
func WithProxyURL(rawURL string) Option {
	return func(c *HTTPClient) {
		c.proxyURLRaw = strings.TrimSpace(rawURL)
	}
}

// WithHTTPClient substitutes the underlying *http.Client. Useful for tests
// and for wrapping with instrumentation (e.g. otelhttp).
func WithHTTPClient(hc *http.Client) Option {
	return func(c *HTTPClient) {
		if hc != nil {
			c.client = hc
			c.customHTTPClient = true
		}
	}
}

// WithUserAgent sets the User-Agent header.
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

// NewHTTPClient constructs a client. The supplied timeout is the total request
// deadline (connect + TLS + send + receive).
func NewHTTPClient(token string, timeout time.Duration, m *metrics.Metrics, opts ...Option) (*HTTPClient, error) {
	c := &HTTPClient{
		baseURL:   defaultBaseURL,
		token:     token,
		metrics:   m,
		userAgent: defaultUserAgent,
		maxBody:   defaultMaxResponseBytes,
	}

	for _, opt := range opts {
		opt(c)
	}

	baseURL, err := normalizeBaseURL(c.baseURL)
	if err != nil {
		return nil, err
	}
	c.baseURL = baseURL

	proxyURL, err := parseProxyURL(c.proxyURLRaw)
	if err != nil {
		return nil, err
	}

	if c.customHTTPClient && proxyURL != nil {
		return nil, errors.New("telegram proxy URL cannot be combined with a custom HTTP client")
	}

	if !c.customHTTPClient {
		transport, err := newTransport(proxyURL)
		if err != nil {
			return nil, err
		}
		c.client = &http.Client{
			Timeout:   timeout,
			Transport: transport,
		}
	}
	if c.client == nil {
		c.client = &http.Client{Timeout: timeout}
	}
	if c.client.Timeout == 0 && timeout > 0 {
		c.client.Timeout = timeout
	}

	c.sensitiveReplacers = buildSensitiveReplacers(c.token, proxyURL)
	return c, nil
}

func newTransport(proxyURL *url.URL) (*http.Transport, error) {
	transport := &http.Transport{
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

	if proxyURL == nil {
		return transport, nil
	}

	switch strings.ToLower(proxyURL.Scheme) {
	case "http", "https":
		transport.Proxy = http.ProxyURL(proxyURL)
		return transport, nil
	case "socks5":
		dialer, err := proxy.FromURL(proxyURL, &net.Dialer{
			Timeout:   defaultDialTimeout,
			KeepAlive: defaultKeepAlive,
		})
		if err != nil {
			return nil, fmt.Errorf("configure telegram socks5 proxy: %w", err)
		}
		transport.Proxy = nil
		transport.DialContext = dialContextFromDialer(dialer)
		return transport, nil
	default:
		return nil, fmt.Errorf("unsupported telegram proxy scheme %q", proxyURL.Scheme)
	}
}

func dialContextFromDialer(dialer proxy.Dialer) func(ctx context.Context, network, address string) (net.Conn, error) {
	if contextDialer, ok := dialer.(proxy.ContextDialer); ok {
		return contextDialer.DialContext
	}

	return func(ctx context.Context, network, address string) (net.Conn, error) {
		type result struct {
			conn net.Conn
			err  error
		}

		done := make(chan result, 1)
		go func() {
			conn, err := dialer.Dial(network, address)
			done <- result{conn: conn, err: err}
		}()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case result := <-done:
			return result.conn, result.err
		}
	}
}

func normalizeBaseURL(rawURL string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", errors.New("telegram base URL must not be empty")
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse telegram base URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("telegram base URL scheme %q is not supported", parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", errors.New("telegram base URL must include a host")
	}
	if parsed.User != nil {
		return "", errors.New("telegram base URL must not contain credentials")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("telegram base URL must not contain query parameters or fragments")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func parseProxyURL(rawURL string) (*url.URL, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, nil
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse telegram proxy URL: %w", err)
	}
	if parsed.Host == "" {
		return nil, errors.New("telegram proxy URL must include a host")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("telegram proxy URL must not contain query parameters or fragments")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return nil, errors.New("telegram proxy URL must not contain a path")
	}

	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "socks5":
		return parsed, nil
	case "mtproto":
		return nil, errors.New("mtproto proxies are not supported for the HTTP Telegram Bot API client; use socks5/http(s), a local Bot API server, or network-level egress instead")
	default:
		return nil, fmt.Errorf("telegram proxy URL scheme %q is not supported", parsed.Scheme)
	}
}

func buildSensitiveReplacers(token string, proxyURL *url.URL) []stringReplacer {
	replacers := make([]stringReplacer, 0, 3)
	if token != "" {
		replacers = append(replacers, stringReplacer{old: token, new: "[REDACTED]"})
	}
	if proxyURL != nil && proxyURL.User != nil {
		if userInfo := proxyURL.User.String(); userInfo != "" {
			replacers = append(replacers, stringReplacer{old: userInfo + "@", new: "[REDACTED]@"})
		}
		sanitized := sanitizeURLForLog(proxyURL)
		if sanitized != proxyURL.String() {
			replacers = append(replacers, stringReplacer{old: proxyURL.String(), new: sanitized})
		}
	}
	return replacers
}

func sanitizeURLForLog(rawURL *url.URL) string {
	copyURL := *rawURL
	copyURL.User = nil
	copyURL.RawQuery = ""
	copyURL.Fragment = ""
	copyURL.Path = ""
	return copyURL.String()
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

// sendMessageResponse models the Telegram envelope.
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

	requestURL := c.baseURL + "/bot" + c.token + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(body))
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

	statusCode := resp.StatusCode
	if statusCode < 400 && !parsed.OK && parsed.ErrorCode != 0 {
		statusCode = parsed.ErrorCode
	}

	retryAfter := time.Duration(parsed.Parameters.RetryAfter) * time.Second
	if retryAfter == 0 {
		retryAfter = parseRetryAfterHeader(resp.Header.Get("Retry-After"))
	}

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

func classifyNetworkError(err error) (retryable bool, metricLabel string) {
	if err == nil {
		return false, "success"
	}

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
		return true, "dns"
	}

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

	return true, "unknown"
}

func (c *HTTPClient) redact(s string) string {
	for _, replacer := range c.sensitiveReplacers {
		if replacer.old != "" && strings.Contains(s, replacer.old) {
			s = strings.ReplaceAll(s, replacer.old, replacer.new)
		}
	}
	return s
}

func (c *HTTPClient) redactError(err error) error {
	if err == nil {
		return nil
	}
	return &redactedError{err: err, redact: c.redact}
}

type redactedError struct {
	err    error
	redact func(string) string
}

func (r *redactedError) Error() string {
	if r.redact == nil {
		return r.err.Error()
	}
	return r.redact(r.err.Error())
}

func (r *redactedError) Unwrap() error { return r.err }

func (c *HTTPClient) observe(operation, result string, duration time.Duration) {
	if c.metrics == nil {
		return
	}
	c.metrics.ObserveTelegramRequest(operation, result, duration)
}
