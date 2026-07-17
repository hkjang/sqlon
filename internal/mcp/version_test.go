package mcp

import (
	"encoding/json"
	"testing"
)

func TestOpenAPIVersionMatchesServerVersion(t *testing.T) {
	var spec struct {
		Info struct {
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"info"`
	}
	if err := json.Unmarshal([]byte(openAPISpec), &spec); err != nil {
		t.Fatalf("openAPI JSON is invalid: %v", err)
	}
	if spec.Info.Version != Version {
		t.Fatalf("openAPI version %q != server version %q", spec.Info.Version, Version)
	}
	if spec.Info.Title == "" {
		t.Fatal("openAPI title is empty")
	}
}
