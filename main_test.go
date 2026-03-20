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
