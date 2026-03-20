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
	pollInterval  time.Duration
	requestMethod string
	requestURL    *template.Template
	requestBody   *template.Template
	headers       map[string]string
	httpTimeout   time.Duration
}

type monitor struct {
	docker   *dockerClient
	http     *http.Client
	cfg      config
	notified map[string]struct{}
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

type dockerClient struct {
	baseURL    string
	httpClient *http.Client
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
		notified: make(map[string]struct{}),
	}

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

	return cfg, nil
}

func parseTemplate(name, value string) (*template.Template, error) {
	return template.New(name).Option("missingkey=error").Parse(normalizeTemplate(value))
}

func normalizeTemplate(value string) string {
	return simpleTemplatePattern.ReplaceAllString(value, "{{ .$1 }}")
}

func (m *monitor) run(ctx context.Context) error {
	if err := m.check(ctx); err != nil {
		log.Printf("initial check failed: %v", err)
	}

	ticker := time.NewTicker(m.cfg.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := m.check(ctx); err != nil {
				log.Printf("scheduled check failed: %v", err)
			}
		}
	}
}

func (m *monitor) check(ctx context.Context) error {
	containers, err := m.docker.listUnhealthyContainers(ctx)
	if err != nil {
		return err
	}

	unhealthyIDs := make(map[string]struct{}, len(containers))
	for _, summary := range containers {
		details, err := m.docker.inspectContainer(ctx, summary.ID)
		if err != nil {
			return err
		}
		if !isUnhealthyContainer(details) {
			continue
		}

		unhealthyIDs[summary.ID] = struct{}{}

		if _, alreadyNotified := m.notified[summary.ID]; alreadyNotified {
			continue
		}

		if err := m.notify(ctx, details); err != nil {
			return err
		}

		m.notified[summary.ID] = struct{}{}
		log.Printf("sent notification for container %s", details.Name)
	}

	for id := range m.notified {
		if _, unhealthy := unhealthyIDs[id]; !unhealthy {
			delete(m.notified, id)
		}
	}

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
			baseURL:    "http://docker",
			httpClient: &http.Client{Transport: transport},
		}, nil
	case "tcp":
		return &dockerClient{
			baseURL:    "http://" + parsed.Host,
			httpClient: &http.Client{},
		}, nil
	case "http", "https":
		return &dockerClient{
			baseURL:    parsed.String(),
			httpClient: &http.Client{},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported DOCKER_HOST scheme %q", parsed.Scheme)
	}
}

func isUnhealthyContainer(details containerDetails) bool {
	return strings.EqualFold(details.Health, "unhealthy")
}

func (m *monitor) notify(ctx context.Context, details containerDetails) error {
	now := time.Now().UTC()
	data := map[string]any{
		"container": map[string]any{
			"id":         details.ID,
			"name":       details.Name,
			"image":      details.Image,
			"status":     details.Status,
			"state":      details.State,
			"health":     details.Health,
			"started_at": details.StartedAt,
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

func renderTemplate(tmpl *template.Template, data map[string]any) (string, error) {
	var b strings.Builder
	if err := tmpl.Execute(&b, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

func (d *dockerClient) listUnhealthyContainers(ctx context.Context) ([]dockerContainerSummary, error) {
	filterPayload, err := json.Marshal(map[string]map[string]bool{
		"health": {
			"unhealthy": true,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("encode filters: %w", err)
	}

	query := url.Values{
		"all":     []string{"1"},
		"filters": []string{string(filterPayload)},
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
