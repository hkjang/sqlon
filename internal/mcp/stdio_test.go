package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"sqlon/internal/catalog"
)

func TestServeStdio(t *testing.T) {
	c, err := catalog.Load(filepath.Join("..", "..", "data", "metadb"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0.0.1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		"",
	}, "\n")
	var out bytes.Buffer
	if err := ServeStdio(context.Background(), c, strings.NewReader(input), &out, StdioOptions{}); err != nil {
		t.Fatalf("ServeStdio() error = %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 response lines, got %d: %q", len(lines), out.String())
	}
	var initResp map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &initResp); err != nil {
		t.Fatalf("unmarshal initialize response: %v", err)
	}
	if initResp["jsonrpc"] != "2.0" {
		t.Fatalf("unexpected initialize response: %+v", initResp)
	}
	var toolsResp struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &toolsResp); err != nil {
		t.Fatalf("unmarshal tools response: %v", err)
	}
	expectedTools := len((&Server{}).tools())
	if len(toolsResp.Result.Tools) != expectedTools {
		t.Fatalf("expected %d tools from registry, got %d", expectedTools, len(toolsResp.Result.Tools))
	}
	for _, tool := range toolsResp.Result.Tools {
		schema, ok := tool["inputSchema"].(map[string]any)
		if !ok {
			t.Fatalf("tool %v has invalid inputSchema: %T", tool["name"], tool["inputSchema"])
		}
		required, ok := schema["required"]
		if !ok {
			continue
		}
		if _, ok := required.([]any); !ok {
			t.Fatalf("tool %v has invalid required schema field: %T", tool["name"], required)
		}
	}
}
