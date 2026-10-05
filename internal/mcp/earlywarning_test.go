package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"sqlon/internal/collector"
	"sqlon/internal/dbconn"
	"sqlon/internal/earlywarning"
)

// TestEarlyWarningAPI drives the engine with failed collections (no DB is
// contacted) until collection_down fires, then exercises the REST surface,
// the MCP tool, and the console page.
func TestEarlyWarningAPI(t *testing.T) {
	s, mux := newAdminMux(t, "tok")
	if s.EarlyWarning == nil {
		t.Fatalf("early warning must be on by default")
	}
	profile := dbconn.Profile{ID: "orders-prod", Name: "주문 DB", Type: "postgres", Environment: "production",
		ConnectString: "127.0.0.1:1/none", Username: "mon", PasswordRef: "env:NOPE",
		Capacity: &dbconn.CapacityConfig{StorageLimit: "500GiB"}}
	if err := dbconn.SaveProfiles(s.opDir(), []dbconn.Profile{profile}); err != nil {
		t.Fatal(err)
	}
	failed := collector.BatchResult{Results: []collector.ProfileResult{{Status: "error", ErrorCode: "COLLECTION_FAILED", Error: "connection refused", Snapshot: collector.Snapshot{ProfileID: profile.ID}}}}
	for i := 0; i < 3; i++ {
		s.EarlyWarning.Evaluate(context.Background(), failed, earlywarning.EvaluateOptions{})
	}

	if rec := doReq(t, mux, "GET", "/api/early-warning", "", nil); rec.Code != 401 {
		t.Fatalf("the board requires the admin token in standalone mode, got %d", rec.Code)
	}
	auth := map[string]string{"X-Admin-Token": "tok", "Content-Type": "application/json"}
	rec := doReq(t, mux, "GET", "/api/early-warning", "", auth)
	var board earlywarning.Board
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &board) != nil {
		t.Fatalf("board: %d %s", rec.Code, rec.Body.String())
	}
	if len(board.Firing) != 1 || board.Firing[0].Rule != earlywarning.RuleCollectionDown || board.Firing[0].Severity != earlywarning.SevCritical {
		t.Fatalf("three failed collections of a production DB must fire collection_down: %+v", board.Firing)
	}
	if board.Delivery.Configured {
		t.Fatalf("no webhook was configured")
	}

	id := board.Firing[0].ID
	if rec := doReq(t, mux, "POST", "/api/early-warning/alerts/"+id+"/ack", `{"note":"점검 중"}`, auth); rec.Code != 200 || !strings.Contains(rec.Body.String(), "점검 중") {
		t.Fatalf("ack: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, mux, "POST", "/api/early-warning/alerts/ew-missing/ack", `{}`, auth); rec.Code != 404 {
		t.Fatalf("unknown alert must be 404, got %d", rec.Code)
	}
	if rec := doReq(t, mux, "POST", "/api/early-warning/test-notification", "", auth); rec.Code != 409 {
		t.Fatalf("test-notification without a webhook must be 409, got %d", rec.Code)
	}

	params, _ := json.Marshal(map[string]any{"name": "get_early_warnings", "arguments": map[string]any{"profile": profile.ID}})
	denied, err := s.callTool(context.WithValue(context.Background(), ctxKeyHTTPAdmin{}, false), params)
	if err != nil || denied.(map[string]any)["status"] != "forbidden" {
		t.Fatalf("over HTTP without the token the board must be refused: %+v %v", denied, err)
	}
	out, err := s.callTool(context.WithValue(context.Background(), ctxKeyHTTPAdmin{}, true), params)
	if err != nil {
		t.Fatal(err)
	}
	result, _ := out.(map[string]any)
	if result["status"] != "ok" || !strings.Contains(result["headline"].(string), "긴급 1") {
		t.Fatalf("get_early_warnings: %+v", result)
	}

	if rec := doReq(t, mux, "GET", "/admin/alerts", "", nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), "예방 경보") {
		t.Fatalf("console page missing: %d", rec.Code)
	}
}

func TestEarlyWarningCanBeDisabled(t *testing.T) {
	s, _ := newFixtureServer(t)
	off := NewServer(s.cat(), Options{EarlyWarning: EarlyWarningOptions{Disabled: true}})
	if off.EarlyWarning != nil || off.Collector.AlertSink != nil {
		t.Fatalf("disabled early warning must not wire an engine or sink")
	}
	out, err := off.mcpEarlyWarnings(context.Background(), "")
	if err != nil || out.(map[string]any)["status"] != "disabled" {
		t.Fatalf("disabled tool answer: %+v %v", out, err)
	}
}
