package alerttemplate

import (
	"bytes"
	"fmt"
	"html"
	"os"
	"path/filepath"
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
	Status        string
	Name          string
	Severity      string
	Summary       string
	Description   string
	StartsAt      time.Time
	EndsAt        time.Time
	GeneratorURL  string
	SilenceURL    string
	DashboardURL  string
	PanelURL      string
	ValueString   string
	Labels        map[string]string
	Annotations   map[string]string
	WhereLines    []string
	SinceUTC      string
	SinceLocal    string
	SinceAgo      string
	ResolvedUTC   string
	ResolvedLocal string
	Duration      string
	ActionLinks   []ActionLink
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
	CommonLabels      map[string]string
	CommonAnnotations map[string]string
	Alerts            []AlertData
	PartIndex         int
	PartCount         int
	StatusIcon        string
	EnvironmentName   string
	EnvironmentIcon   string
	GroupContext      string
}

type ActionLink struct {
	Label string
	URL   string
}

type Renderer struct {
	path            string
	tmpl            *template.Template
	metrics         *metrics.Metrics
	displayLocation *time.Location
}

var noisyLabels = map[string]struct{}{
	// Already shown in the alert header
	"alertname":          {},
	"severity":           {},
	"status":             {},
	"grafana_folder":     {},
	"folder":             {},
	"rule_uid":           {},
	"dashboard_uid":      {},
	"panel_id":           {},
	"__alert_rule_uid__": {},
	"pod_template_hash":  {},
	"__name__":           {},
	"prometheus":         {},
}

var exporterPodPrefixes = []string{
	"kube-prometheus-stack-kube-state-metrics-",
	"kube-prometheus-stack-prometheus-node-exporter-",
	"kube-prometheus-stack-operator-",
	"kube-state-metrics-",
	"node-exporter-",
}

var groupContextPriority = []string{
	"namespace",
	"cluster",
	"service",
	"team",
	"environment",
	"env",
}

var whereLabelPriority = []string{
	"deployment",
	"statefulset",
	"daemonset",
	"job_name",
	"namespace",
	"pod",
	"container",
	"instance",
	"node",
	"mount",
	"device",
}

func Load(path string, m *metrics.Metrics) (*Renderer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read alert template: %w", err)
	}

	tmpl, err := template.New(filepath.Base(path)).Funcs(template.FuncMap{
		"append":       appendString,
		"duration":     durationBetween,
		"env":          environmentName,
		"envIcon":      envIcon,
		"filterLabels": filterLabels,
		"formatTime":   formatTemplateTime,
		"isZeroTime":   isZeroTime,
		"join":         join,
		"joinPairs":    joinPairs,
		"list":         list,
		"orDash":       orDash,
		"pickFirst":    pickFirst,
		"severityIcon": severityIcon,
		"since":        sinceTime,
		"statusIcon":   statusIcon,
		"sub":          sub,
		"truncate":     truncate,
		"upper":        strings.ToUpper,
	}).Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("parse alert template: %w", err)
	}

	return &Renderer{
		path:            path,
		tmpl:            tmpl,
		metrics:         m,
		displayLocation: time.Local,
	}, nil
}

func (r *Renderer) SetDisplayLocation(location *time.Location) {
	if location == nil {
		location = time.Local
	}
	r.displayLocation = location
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
		labels := sanitizeStringMap(alert.Labels, 128, 1024)
		annotations := sanitizeStringMap(alert.Annotations, 128, 2048)
		startUTC, startLocal, startAgo := r.formatSince(alert.StartsAt)
		resolvedUTC, resolvedLocal := r.formatResolved(alert.EndsAt)

		alerts = append(alerts, AlertData{
			Status:        sanitize(alert.Status, 32),
			Name:          sanitize(firstNonEmpty(alert.Labels["alertname"], alert.Fingerprint, "unnamed-alert"), 256),
			Severity:      sanitize(firstNonEmpty(alert.Labels["severity"], "unknown"), 64),
			Summary:       sanitize(firstNonEmpty(alert.Annotations["summary"], alert.Annotations["message"]), 1024),
			Description:   sanitize(alert.Annotations["description"], 4096),
			StartsAt:      alert.StartsAt,
			EndsAt:        alert.EndsAt,
			GeneratorURL:  sanitize(alert.GeneratorURL, 2048),
			SilenceURL:    sanitize(alert.SilenceURL, 2048),
			DashboardURL:  sanitize(alert.DashboardURL, 2048),
			PanelURL:      sanitize(alert.PanelURL, 2048),
			ValueString:   sanitize(firstNonEmpty(alert.ValueString, formatValues(alert.Values)), 1024),
			Labels:        labels,
			Annotations:   annotations,
			WhereLines:    renderWhereLines(labels),
			SinceUTC:      startUTC,
			SinceLocal:    startLocal,
			SinceAgo:      startAgo,
			ResolvedUTC:   resolvedUTC,
			ResolvedLocal: resolvedLocal,
			Duration:      durationBetween(alert.StartsAt, alert.EndsAt),
			ActionLinks:   buildActionLinks(annotations, sanitize(alert.GeneratorURL, 2048), sanitize(alert.DashboardURL, 2048), sanitize(alert.PanelURL, 2048), sanitize(alert.SilenceURL, 2048)),
		})
	}

	commonLabels := sanitizeStringMap(payload.CommonLabels, 128, 1024)
	envName := environmentName(commonLabels)

	return MessageData{
		Receiver:          sanitize(payload.Receiver, 128),
		Status:            sanitize(payload.Status, 32),
		GroupKey:          sanitize(payload.GroupKey, 256),
		ExternalURL:       sanitize(payload.ExternalURL, 2048),
		Title:             sanitize(payload.Title, 256),
		Message:           sanitize(payload.Message, 2048),
		FiringCount:       payload.FiringCount(),
		ResolvedCount:     payload.ResolvedCount(),
		TotalAlerts:       len(payload.Alerts) + payload.TruncatedAlerts,
		TruncatedAlerts:   payload.TruncatedAlerts,
		CommonLabels:      commonLabels,
		CommonAnnotations: sanitizeStringMap(payload.CommonAnnotations, 128, 2048),
		Alerts:            alerts,
		PartIndex:         1,
		PartCount:         1,
		StatusIcon:        statusIcon(payload.Status),
		EnvironmentName:   strings.ToUpper(envName),
		EnvironmentIcon:   envIcon(envName),
		GroupContext:      renderGroupContext(commonLabels),
	}
}

func CloneWithAlerts(data MessageData, alerts []AlertData, partIndex, partCount int) MessageData {
	clone := data
	clone.Alerts = alerts
	clone.TotalAlerts = len(alerts) + clone.TruncatedAlerts
	clone.FiringCount = 0
	clone.ResolvedCount = 0
	for _, alert := range alerts {
		switch alert.Status {
		case "firing":
			clone.FiringCount++
		case "resolved":
			clone.ResolvedCount++
		}
	}
	clone.PartIndex = partIndex
	clone.PartCount = partCount
	return clone
}

func (r *Renderer) formatSince(value time.Time) (string, string, string) {
	if value.IsZero() {
		return "", "", ""
	}
	return formatUTCTime(value), r.formatLocalTime(value), sinceTime(value)
}

func (r *Renderer) formatResolved(value time.Time) (string, string) {
	if value.IsZero() {
		return "", ""
	}
	return formatUTCTime(value), r.formatLocalTime(value)
}

func (r *Renderer) formatLocalTime(value time.Time) string {
	if value.IsZero() || r.displayLocation == nil {
		return ""
	}
	local := value.In(r.displayLocation)
	zone, _ := local.Zone()
	if zone == "UTC" {
		return ""
	}
	return local.Format("15:04 ") + zone
}

func sanitizeStringMap(values map[string]string, maxKeyLen, maxValueLen int) map[string]string {
	if len(values) == 0 {
		return nil
	}
	sanitized := make(map[string]string, len(values))
	for key, value := range values {
		sanitized[sanitize(key, maxKeyLen)] = sanitize(value, maxValueLen)
	}
	return sanitized
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
	if maxLen <= 1 {
		return string(runes[:maxLen])
	}
	return string(runes[:maxLen-1]) + "…"
}

func joinPairs(values any) string {
	pairs := mapToPairs(values)
	if len(pairs) == 0 {
		return ""
	}
	lines := make([]string, 0, len(pairs))
	for _, value := range pairs {
		lines = append(lines, fmt.Sprintf("%s=%s", value.Key, value.Value))
	}
	return strings.Join(lines, ", ")
}

func orDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "—"
	}
	return value
}

func severityIcon(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "critical":
		return "🔴"
	case "warning":
		return "🟠"
	case "info":
		return "🟡"
	default:
		return "⚪"
	}
}

func statusIcon(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "firing":
		return "🔥"
	case "resolved":
		return "✅"
	default:
		return "⚪"
	}
}

func envIcon(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "prod":
		return "🟥"
	case "staging":
		return "🟨"
	case "dev":
		return "🟩"
	default:
		return "⬜"
	}
}

func formatTemplateTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.Format("2006-01-02 15:04 MST")
}

func formatUTCTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format("2006-01-02 15:04 MST")
}

func sinceTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	now := time.Now()
	if value.After(now) {
		return "in " + compactDuration(value.Sub(now))
	}
	return compactDuration(now.Sub(value)) + " ago"
}

func durationBetween(start, end time.Time) string {
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return ""
	}
	return compactDuration(end.Sub(start))
}

func isZeroTime(value time.Time) bool {
	return value.IsZero()
}

func filterLabels(values any) map[string]string {
	source := pairsToMap(mapToPairs(values))
	if len(source) == 0 {
		return nil
	}

	filtered := make(map[string]string, len(source))
	for key, value := range source {
		if _, skip := noisyLabels[strings.ToLower(key)]; skip {
			continue
		}
		if strings.EqualFold(key, "pod") && isExporterPod(value) {
			continue
		}
		filtered[key] = value
	}
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

func renderGroupContext(labels map[string]string) string {
	selected := selectOnlyPreferredLabels(filterLabels(labels), groupContextPriority)
	if len(selected) == 0 {
		selected = filterLabels(labels)
	}
	if len(selected) == 0 {
		return ""
	}
	return joinPairs(selected)
}

func renderWhereLines(labels map[string]string) []string {
	filtered := filterLabels(labels)
	if len(filtered) == 0 {
		return nil
	}

	lines := make([]string, 0, len(filtered))
	for _, pair := range orderedLabelPairs(filtered, whereLabelPriority) {
		lines = append(lines, fmt.Sprintf("- %s: %s", pair.Key, pair.Value))
	}
	return lines
}

func buildActionLinks(annotations map[string]string, generatorURL, dashboardURL, panelURL, silenceURL string) []ActionLink {
	links := make([]ActionLink, 0, 5)
	if url := strings.TrimSpace(annotations["runbook_url"]); url != "" {
		links = append(links, ActionLink{Label: "📕 runbook", URL: url})
	}
	if generatorURL != "" {
		links = append(links, ActionLink{Label: "📊 graph", URL: generatorURL})
	}
	if dashboardURL != "" {
		links = append(links, ActionLink{Label: "📈 dashboard", URL: dashboardURL})
	}
	if panelURL != "" {
		links = append(links, ActionLink{Label: "🔍 panel", URL: panelURL})
	}
	if silenceURL != "" {
		links = append(links, ActionLink{Label: "🔕 silence", URL: silenceURL})
	}
	if len(links) == 0 {
		return nil
	}
	return links
}

func selectPriorityLabels(source map[string]string, preferred []string) map[string]string {
	if len(source) == 0 {
		return nil
	}

	selected := make(map[string]string)
	seen := make(map[string]struct{}, len(preferred))
	for _, key := range preferred {
		for sourceKey, value := range source {
			if strings.EqualFold(sourceKey, key) {
				selected[sourceKey] = value
				seen[strings.ToLower(sourceKey)] = struct{}{}
				break
			}
		}
	}

	for key, value := range source {
		if _, ok := seen[strings.ToLower(key)]; ok {
			continue
		}
		selected[key] = value
	}

	if len(selected) == 0 {
		return nil
	}
	return selected
}

func selectOnlyPreferredLabels(source map[string]string, preferred []string) map[string]string {
	if len(source) == 0 {
		return nil
	}

	selected := make(map[string]string)
	for _, key := range preferred {
		for sourceKey, value := range source {
			if strings.EqualFold(sourceKey, key) {
				selected[sourceKey] = value
				break
			}
		}
	}
	if len(selected) == 0 {
		return nil
	}
	return selected
}

func orderedLabelPairs(source map[string]string, preferred []string) []Pair {
	if len(source) == 0 {
		return nil
	}

	pairs := make([]Pair, 0, len(source))
	seen := make(map[string]struct{}, len(source))
	for _, key := range preferred {
		for sourceKey, value := range source {
			if strings.EqualFold(sourceKey, key) {
				pairs = append(pairs, Pair{Key: sourceKey, Value: value})
				seen[strings.ToLower(sourceKey)] = struct{}{}
				break
			}
		}
	}

	remainder := make([]Pair, 0, len(source))
	for key, value := range source {
		if _, ok := seen[strings.ToLower(key)]; ok {
			continue
		}
		remainder = append(remainder, Pair{Key: key, Value: value})
	}
	sort.Slice(remainder, func(i, j int) bool {
		return remainder[i].Key < remainder[j].Key
	})

	return append(pairs, remainder...)
}

func isExporterPod(name string) bool {
	for _, prefix := range exporterPodPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func truncate(value string, limit int) string {
	if limit <= 0 || value == "" {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	if limit == 1 {
		return string(runes[:1])
	}
	return string(runes[:limit-1]) + "…"
}

func pickFirst(values any, keys ...string) string {
	source := pairsToMap(mapToPairs(values))
	for _, key := range keys {
		if value := strings.TrimSpace(source[key]); value != "" {
			return value
		}
	}
	return ""
}

func environmentName(values any) string {
	source := pairsToMap(mapToPairs(values))
	candidates := []string{
		source["env"],
		source["environment"],
		source["cluster"],
	}
	for _, candidate := range candidates {
		switch normalized := normalizeEnvironment(candidate); normalized {
		case "prod", "staging", "dev":
			return normalized
		}
	}
	return ""
}

func join(values []string, separator string) string {
	return strings.Join(values, separator)
}

func list(values ...string) []string {
	if len(values) == 0 {
		return []string{}
	}
	return append([]string(nil), values...)
}

func appendString(values []string, value string) []string {
	return append(values, value)
}

func sub(left, right int) int {
	return left - right
}

func mapToPairs(values any) []Pair {
	switch typed := values.(type) {
	case nil:
		return nil
	case []Pair:
		if len(typed) == 0 {
			return nil
		}
		out := make([]Pair, len(typed))
		copy(out, typed)
		sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
		return out
	case map[string]string:
		if len(typed) == 0 {
			return nil
		}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		pairs := make([]Pair, 0, len(keys))
		for _, key := range keys {
			pairs = append(pairs, Pair{Key: key, Value: typed[key]})
		}
		return pairs
	default:
		return nil
	}
}

func pairsToMap(values []Pair) map[string]string {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]string, len(values))
	for _, value := range values {
		result[value.Key] = value.Value
	}
	return result
}

func compactDuration(value time.Duration) string {
	if value < 0 {
		value = -value
	}
	if value < time.Minute {
		return "<1m"
	}

	totalMinutes := int(value.Round(time.Minute) / time.Minute)
	if totalMinutes < 60 {
		return fmt.Sprintf("%dm", totalMinutes)
	}

	totalHours := totalMinutes / 60
	minutes := totalMinutes % 60
	if totalHours < 24 {
		if minutes == 0 {
			return fmt.Sprintf("%dh", totalHours)
		}
		return fmt.Sprintf("%dh%dm", totalHours, minutes)
	}

	days := totalHours / 24
	hours := totalHours % 24
	if hours == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd%dh", days, hours)
}

func normalizeEnvironment(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	switch {
	case normalized == "":
		return ""
	case normalized == "prod", normalized == "production":
		return "prod"
	case normalized == "staging", normalized == "stage":
		return "staging"
	case normalized == "dev", normalized == "development", normalized == "test":
		return "dev"
	case strings.Contains(normalized, "prod"):
		return "prod"
	case strings.Contains(normalized, "stag"):
		return "staging"
	case strings.Contains(normalized, "dev"), strings.Contains(normalized, "test"):
		return "dev"
	default:
		return ""
	}
}

func formatValues(values map[string]interface{}) string {
	if len(values) == 0 {
		return ""
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", key, values[key]))
	}
	return strings.Join(parts, ", ")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
