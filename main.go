package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/template"
	"time"
)

const (
	defaultPollInterval   = 30 * time.Second
	defaultRequestMethod  = http.MethodPost
	defaultRequestTimeout = 10 * time.Second
	defaultDockerHost     = "unix:///var/run/docker.sock"
)

var simpleTemplatePattern = regexp.MustCompile(`\{\{\s*([a-zA-Z_][a-zA-Z0-9_.]*)\s*\}\}`)

type config struct {
	pollInterval               time.Duration
	requestMethod              string
	requestURL                 *template.Template
	requestBody                *template.Template
	headers                    map[string]string
	httpTimeout                time.Duration
	notifyOnRunningStateChange bool
}

type monitor struct {
	docker   *dockerClient
	http     *http.Client
	cfg      config
	notified map[string]containerDetails
	lastSeen map[string]containerDetails
}

type containerDetails struct {
	ID        string
	Name      string
	Image     string
	Status    string
	State     string
	Health    string
	StartedAt string
}

type notificationDetails struct {
	Type          string
	Current       containerDetails
	PreviousState string
}

type dockerClient struct {
	baseURL     string
	displayHost string
	httpClient  *http.Client
}

type dockerContainerSummary struct {
	ID string `json:"Id"`
}

type dockerContainerInspect struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Config struct {
		Image string `json:"Image"`
	} `json:"Config"`
	State *struct {
		Status    string `json:"Status"`
		StartedAt string `json:"StartedAt"`
		Health    *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}

	dockerClient, err := newDockerClient()
	if err != nil {
		log.Fatalf("create docker client: %v", err)
	}

	m := &monitor{
		docker:   dockerClient,
		http:     &http.Client{Timeout: cfg.httpTimeout},
		cfg:      cfg,
		notified: make(map[string]containerDetails),
		lastSeen: make(map[string]containerDetails),
	}

	log.Printf("starting unhealthy monitor: poll_interval=%s request_method=%s request_timeout=%s docker_host=%s notify_on_running_state_change=%t headers=%s",
		cfg.pollInterval,
		cfg.requestMethod,
		cfg.httpTimeout,
		dockerClient.displayHost,
		cfg.notifyOnRunningStateChange,
		strings.Join(sortedHeaderKeys(cfg.headers), ","),
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := m.run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

func loadConfig() (config, error) {
	cfg := config{
		pollInterval:  defaultPollInterval,
		requestMethod: defaultRequestMethod,
		headers:       map[string]string{"Content-Type": "application/json"},
		httpTimeout:   defaultRequestTimeout,
	}

	if pollInterval := strings.TrimSpace(os.Getenv("POLL_INTERVAL")); pollInterval != "" {
		parsed, err := time.ParseDuration(pollInterval)
		if err != nil {
			return config{}, fmt.Errorf("parse POLL_INTERVAL: %w", err)
		}
		cfg.pollInterval = parsed
	}

	if requestMethod := strings.TrimSpace(os.Getenv("REQUEST_METHOD")); requestMethod != "" {
		cfg.requestMethod = strings.ToUpper(requestMethod)
	}

	requestURL := strings.TrimSpace(os.Getenv("REQUEST_URL"))
	if requestURL == "" {
		return config{}, errors.New("REQUEST_URL is required")
	}

	var err error
	cfg.requestURL, err = parseTemplate("request-url", requestURL)
	if err != nil {
		return config{}, fmt.Errorf("parse REQUEST_URL: %w", err)
	}

	requestBody := os.Getenv("REQUEST_BODY_TEMPLATE")
	if strings.TrimSpace(requestBody) == "" {
		return config{}, errors.New("REQUEST_BODY_TEMPLATE is required")
	}

	cfg.requestBody, err = parseTemplate("request-body", requestBody)
	if err != nil {
		return config{}, fmt.Errorf("parse REQUEST_BODY_TEMPLATE: %w", err)
	}

	if headerJSON := strings.TrimSpace(os.Getenv("REQUEST_HEADERS_JSON")); headerJSON != "" {
		if err := json.Unmarshal([]byte(headerJSON), &cfg.headers); err != nil {
			return config{}, fmt.Errorf("parse REQUEST_HEADERS_JSON: %w", err)
		}
	}

	if contentType := strings.TrimSpace(os.Getenv("REQUEST_CONTENT_TYPE")); contentType != "" {
		cfg.headers["Content-Type"] = contentType
	}

	if httpTimeout := strings.TrimSpace(os.Getenv("REQUEST_TIMEOUT")); httpTimeout != "" {
		parsed, err := time.ParseDuration(httpTimeout)
		if err != nil {
			return config{}, fmt.Errorf("parse REQUEST_TIMEOUT: %w", err)
		}
		cfg.httpTimeout = parsed
	}

	if runningStateChange := strings.TrimSpace(os.Getenv("NOTIFY_ON_RUNNING_STATE_CHANGE")); runningStateChange != "" {
		parsed, err := strconv.ParseBool(runningStateChange)
		if err != nil {
			return config{}, fmt.Errorf("parse NOTIFY_ON_RUNNING_STATE_CHANGE: %w", err)
		}
		cfg.notifyOnRunningStateChange = parsed
	}

	return cfg, nil
}

func parseTemplate(name, value string) (*template.Template, error) {
	return template.New(name).Option("missingkey=error").Parse(normalizeTemplate(value))
}

func normalizeTemplate(value string) string {
	return simpleTemplatePattern.ReplaceAllString(value, "{{ .$1 }}")
}

func (m *monitor) run(ctx context.Context) error {
	log.Printf("running initial health check")
	if err := m.check(ctx); err != nil {
		log.Printf("initial check failed: %v", err)
	}

	ticker := time.NewTicker(m.cfg.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("shutdown signal received, stopping monitor")
			return ctx.Err()
		case <-ticker.C:
			log.Printf("running scheduled health check")
			if err := m.check(ctx); err != nil {
				log.Printf("scheduled check failed: %v", err)
			}
		}
	}
}

func (m *monitor) check(ctx context.Context) error {
	containers, err := m.docker.listContainers(ctx)
	if err != nil {
		return err
	}

	currentSeen := make(map[string]containerDetails, len(containers))
	unhealthyIDs := make(map[string]struct{}, len(containers))
	newlyNotified := 0
	alreadyTracked := 0
	recovered := 0
	runningStateChanges := 0
	for _, summary := range containers {
		details, err := m.docker.inspectContainer(ctx, summary.ID)
		if err != nil {
			return err
		}

		currentSeen[summary.ID] = details

		if isRunningStateChange(m.lastSeen[summary.ID], details) && m.cfg.notifyOnRunningStateChange {
			if err := m.notify(ctx, notificationDetails{
				Type:          "running_state_change",
				Current:       details,
				PreviousState: m.lastSeen[summary.ID].State,
			}); err != nil {
				return err
			}

			runningStateChanges++
			log.Printf("running state change notification delivered for container %s (%s): %s -> %s",
				details.Name,
				details.ID,
				m.lastSeen[summary.ID].State,
				details.State,
			)
		}

		if !isUnhealthyContainer(details) {
			continue
		}

		unhealthyIDs[summary.ID] = struct{}{}

		if _, alreadyNotified := m.notified[summary.ID]; alreadyNotified {
			alreadyTracked++
			continue
		}

		if err := m.notify(ctx, notificationDetails{Type: "unhealthy", Current: details}); err != nil {
			return err
		}

		m.notified[summary.ID] = details
		newlyNotified++
		log.Printf("notification delivered for container %s (%s)", details.Name, details.ID)
	}

	for id, details := range m.notified {
		if _, unhealthy := unhealthyIDs[id]; !unhealthy {
			log.Printf("container recovered: %s (%s)", details.Name, id)
			delete(m.notified, id)
			recovered++
		}
	}

	for id := range m.lastSeen {
		if _, ok := currentSeen[id]; !ok {
			delete(m.lastSeen, id)
		}
	}

	for id, details := range currentSeen {
		m.lastSeen[id] = details
	}

	log.Printf("health check complete: containers=%d unhealthy=%d newly_notified=%d already_tracked=%d recovered=%d running_state_changes=%d active_notifications=%d",
		len(currentSeen),
		len(unhealthyIDs),
		newlyNotified,
		alreadyTracked,
		recovered,
		runningStateChanges,
		len(m.notified),
	)

	return nil
}

func newDockerClient() (*dockerClient, error) {
	rawHost := strings.TrimSpace(os.Getenv("DOCKER_HOST"))
	if rawHost == "" {
		rawHost = defaultDockerHost
	}

	parsed, err := url.Parse(rawHost)
	if err != nil {
		return nil, fmt.Errorf("parse DOCKER_HOST: %w", err)
	}

	switch parsed.Scheme {
	case "unix":
		socketPath := parsed.Path
		if socketPath == "" {
			socketPath = parsed.Opaque
		}
		transport := &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
		}
		return &dockerClient{
			baseURL:     "http://docker",
			displayHost: rawHost,
			httpClient:  &http.Client{Transport: transport},
		}, nil
	case "tcp":
		return &dockerClient{
			baseURL:     "http://" + parsed.Host,
			displayHost: rawHost,
			httpClient:  &http.Client{},
		}, nil
	case "http", "https":
		return &dockerClient{
			baseURL:     parsed.String(),
			displayHost: rawHost,
			httpClient:  &http.Client{},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported DOCKER_HOST scheme %q", parsed.Scheme)
	}
}

func isUnhealthyContainer(details containerDetails) bool {
	return strings.EqualFold(details.Health, "unhealthy")
}

func isRunningStateChange(previous, current containerDetails) bool {
	return strings.EqualFold(previous.State, "running") && !strings.EqualFold(current.State, "running")
}

func (m *monitor) notify(ctx context.Context, notification notificationDetails) error {
	now := time.Now().UTC()
	data := map[string]any{
		"container": map[string]any{
			"id":         notification.Current.ID,
			"name":       notification.Current.Name,
			"image":      notification.Current.Image,
			"status":     notification.Current.Status,
			"state":      notification.Current.State,
			"health":     notification.Current.Health,
			"started_at": notification.Current.StartedAt,
		},
		"event": map[string]any{
			"type":           notification.Type,
			"previous_state": notification.PreviousState,
			"current_state":  notification.Current.State,
		},
		"time": map[string]any{
			"rfc3339": now.Format(time.RFC3339),
			"unix":    now.Unix(),
		},
	}

	requestURL, err := renderTemplate(m.cfg.requestURL, data)
	if err != nil {
		return fmt.Errorf("render request url: %w", err)
	}

	requestBody, err := renderTemplate(m.cfg.requestBody, data)
	if err != nil {
		return fmt.Errorf("render request body: %w", err)
	}

	log.Printf("sending %s notification for container %s (%s) to %s",
		m.cfg.requestMethod,
		notification.Current.Name,
		notification.Current.ID,
		sanitizeURLForLog(requestURL),
	)

	req, err := http.NewRequestWithContext(ctx, m.cfg.requestMethod, requestURL, strings.NewReader(requestBody))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	headerKeys := make([]string, 0, len(m.cfg.headers))
	for key := range m.cfg.headers {
		headerKeys = append(headerKeys, key)
	}
	sort.Strings(headerKeys)
	for _, key := range headerKeys {
		req.Header.Set(key, m.cfg.headers[key])
	}

	resp, err := m.http.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("request failed with status %s", resp.Status)
	}

	return nil
}

func sortedHeaderKeys(headers map[string]string) []string {
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sanitizeURLForLog(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "configured target"
	}

	if parsed.Scheme == "" {
		return parsed.Host
	}

	return parsed.Scheme + "://" + parsed.Host
}

func renderTemplate(tmpl *template.Template, data map[string]any) (string, error) {
	var b strings.Builder
	if err := tmpl.Execute(&b, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

func (d *dockerClient) listContainers(ctx context.Context) ([]dockerContainerSummary, error) {
	query := url.Values{
		"all": []string{"1"},
	}

	var containers []dockerContainerSummary
	if err := d.do(ctx, http.MethodGet, "/containers/json", query, &containers); err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}

	return containers, nil
}

func (d *dockerClient) inspectContainer(ctx context.Context, id string) (containerDetails, error) {
	var response dockerContainerInspect
	if err := d.do(ctx, http.MethodGet, "/containers/"+id+"/json", nil, &response); err != nil {
		return containerDetails{}, fmt.Errorf("inspect container %s: %w", id, err)
	}

	details := containerDetails{
		ID:    response.ID,
		Name:  strings.TrimPrefix(response.Name, "/"),
		Image: response.Config.Image,
	}

	if response.State != nil {
		details.State = response.State.Status
		details.Status = response.State.Status
		details.StartedAt = response.State.StartedAt
		if response.State.Health != nil {
			details.Health = response.State.Health.Status
			details.Status = response.State.Health.Status
		}
	}

	return details, nil
}

func (d *dockerClient) do(ctx context.Context, method, path string, query url.Values, out any) error {
	requestURL := d.baseURL + path
	if len(query) > 0 {
		requestURL += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, requestURL, nil)
	if err != nil {
		return fmt.Errorf("create docker request: %w", err)
	}

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send docker request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("docker API returned %s", resp.Status)
	}

	if out == nil {
		return nil
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode docker response: %w", err)
	}

	return nil
}
