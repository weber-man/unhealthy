package main

import (
	"strings"
	"testing"
)

func TestRenderTemplateReplacesContainerFields(t *testing.T) {
	tmpl, err := parseTemplate("body", `{"message":"The container {{ container.name }} has the status {{ container.status }} at {{ time.rfc3339 }}"}`)
	if err != nil {
		t.Fatalf("parseTemplate() error = %v", err)
	}

	rendered, err := renderTemplate(tmpl, map[string]any{
		"container": map[string]any{
			"name":   "api",
			"status": "unhealthy",
		},
		"time": map[string]any{
			"rfc3339": "2026-03-20T17:21:00Z",
		},
	})
	if err != nil {
		t.Fatalf("renderTemplate() error = %v", err)
	}

	wantParts := []string{`api`, `unhealthy`, `2026-03-20T17:21:00Z`}
	for _, want := range wantParts {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered template %q does not contain %q", rendered, want)
		}
	}
}

func TestRenderTemplateReplacesEventFields(t *testing.T) {
	tmpl, err := parseTemplate("body", `{"event":"{{ event.type }}","transition":"{{ event.previous_state }} -> {{ event.current_state }}"}`)
	if err != nil {
		t.Fatalf("parseTemplate() error = %v", err)
	}

	rendered, err := renderTemplate(tmpl, map[string]any{
		"event": map[string]any{
			"type":           "running_state_change",
			"previous_state": "running",
			"current_state":  "exited",
		},
	})
	if err != nil {
		t.Fatalf("renderTemplate() error = %v", err)
	}

	wantParts := []string{`running_state_change`, `running -> exited`}
	for _, want := range wantParts {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered template %q does not contain %q", rendered, want)
		}
	}
}

func TestIsUnhealthyContainer(t *testing.T) {
	t.Run("unhealthy", func(t *testing.T) {
		if !isUnhealthyContainer(containerDetails{Health: "unhealthy"}) {
			t.Fatal("expected unhealthy container to be detected")
		}
	})

	t.Run("healthy", func(t *testing.T) {
		if isUnhealthyContainer(containerDetails{Health: "healthy"}) {
			t.Fatal("expected healthy container to be ignored")
		}
	})

	t.Run("missing health state", func(t *testing.T) {
		if isUnhealthyContainer(containerDetails{}) {
			t.Fatal("expected container without health status to be ignored")
		}
	})
}

func TestIsRunningStateChange(t *testing.T) {
	t.Run("running to exited", func(t *testing.T) {
		if !isRunningStateChange(containerDetails{State: "running"}, containerDetails{State: "exited"}) {
			t.Fatal("expected running to exited to be detected")
		}
	})

	t.Run("running to running", func(t *testing.T) {
		if isRunningStateChange(containerDetails{State: "running"}, containerDetails{State: "running"}) {
			t.Fatal("expected running to running to be ignored")
		}
	})

	t.Run("exited to dead", func(t *testing.T) {
		if isRunningStateChange(containerDetails{State: "exited"}, containerDetails{State: "dead"}) {
			t.Fatal("expected non-running transition to be ignored")
		}
	})

	t.Run("missing previous state", func(t *testing.T) {
		if isRunningStateChange(containerDetails{}, containerDetails{State: "exited"}) {
			t.Fatal("expected missing previous state to be ignored")
		}
	})
}
