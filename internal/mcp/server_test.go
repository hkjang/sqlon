package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"sqlon/internal/catalog"
)

func newTestServer(t *testing.T, stateful bool) *Server {
	t.Helper()
	c, err := catalog.Load(filepath.Join("..", "..", "data", "metadb"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	return NewServer(c, Options{Stateful: stateful})
}

func postJSON(t *testing.T, s *Server, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.handleMCP(rec, req)
	return rec
}

// Lenient session policy: clients that never echo Mcp-Session-Id (qwen-code,
// opencode, ...) must still be able to call tools/list and tools/call.
func TestStatefulServerToleratesMissingSessionID(t *testing.T) {
	s := newTestServer(t, true)

	init := postJSON(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`, nil)
	if init.Code != http.StatusOK {
		t.Fatalf("initialize status = %d body=%s", init.Code, init.Body.String())
	}
	if init.Header().Get("Mcp-Session-Id") == "" {
		t.Fatal("initialize should still issue a session id")
	}

	// tools/list WITHOUT the session header
	list := postJSON(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, nil)
	if list.Code != http.StatusOK {
		t.Fatalf("tools/list without session = %d body=%s", list.Code, list.Body.String())
	}
	var resp struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
		Error *struct{ Message string } `json:"error"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Error != nil || len(resp.Result.Tools) == 0 {
		t.Fatalf("expected tools, got %s", list.Body.String())
	}

	// unknown/bogus session header must also be tolerated
	bogus := postJSON(t, s, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`, map[string]string{"Mcp-Session-Id": "no-such-session"})
	if bogus.Code != http.StatusOK {
		t.Fatalf("tools/list with bogus session = %d body=%s", bogus.Code, bogus.Body.String())
	}

	// notifications without a session header must be accepted
	note := postJSON(t, s, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil)
	if note.Code != http.StatusAccepted {
		t.Fatalf("notification without session = %d", note.Code)
	}

	// clients that DO echo the session id keep working
	sid := init.Header().Get("Mcp-Session-Id")
	withSid := postJSON(t, s, `{"jsonrpc":"2.0","id":4,"method":"tools/list"}`, map[string]string{"Mcp-Session-Id": sid})
	if withSid.Code != http.StatusOK {
		t.Fatalf("tools/list with valid session = %d", withSid.Code)
	}
}
