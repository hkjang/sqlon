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

func TestEarlyWarningDiskReportAndSilencesAPI(t *testing.T) {
	s, mux := newAdminMux(t, "tok")
	profile := dbconn.Profile{ID: "orders-prod", Type: "postgres", Environment: "production", ConnectString: "127.0.0.1:1/none", Username: "mon", PasswordRef: "env:NOPE"}
	if err := dbconn.SaveProfiles(s.opDir(), []dbconn.Profile{profile}); err != nil {
		t.Fatal(err)
	}
	auth := map[string]string{"Authorization": "Bearer tok", "Content-Type": "application/json"}
	body := `{"profile":"orders-prod","host":"db01","volumes":[{"mount":"/var/lib/postgresql","filesystem":"/dev/sdb1","total_bytes":1000,"used_bytes":950,"avail_bytes":50}]}`
	if rec := doReq(t, mux, "POST", "/api/early-warning/disk", body, map[string]string{"Content-Type": "application/json"}); rec.Code != 401 {
		t.Fatalf("a disk report needs the token, got %d", rec.Code)
	}
	if rec := doReq(t, mux, "POST", "/api/early-warning/disk", strings.Replace(body, "orders-prod", "nope", 1), auth); rec.Code != 404 {
		t.Fatalf("unknown profile must be 404, got %d", rec.Code)
	}
	if rec := doReq(t, mux, "POST", "/api/early-warning/disk", `{"profile":"orders-prod","volumes":[]}`, auth); rec.Code != 400 {
		t.Fatalf("an empty report must be 400, got %d", rec.Code)
	}
	if rec := doReq(t, mux, "POST", "/api/early-warning/disk", body, auth); rec.Code != 200 {
		t.Fatalf("disk report (Bearer token, as the agent script sends it): %d %s", rec.Code, rec.Body.String())
	}
	s.EarlyWarning.Evaluate(context.Background(), collector.BatchResult{Results: []collector.ProfileResult{{Status: "error", ErrorCode: "X", Snapshot: collector.Snapshot{ProfileID: profile.ID}}}}, earlywarning.EvaluateOptions{})
	rec := doReq(t, mux, "GET", "/api/early-warning", "", auth)
	var board earlywarning.Board
	_ = json.Unmarshal(rec.Body.Bytes(), &board)
	found := false
	for _, a := range board.Firing {
		found = found || (a.Rule == earlywarning.RuleCapacityUsage && a.Object == "volume:/var/lib/postgresql" && a.Severity == earlywarning.SevCritical)
	}
	if !found || board.Profiles[0].DiskHost != "db01" {
		t.Fatalf("a 95%% volume report must fire even while the DB is unreachable: %+v", board.Firing)
	}

	if rec := doReq(t, mux, "POST", "/api/early-warning/silences", `{"profile":"orders-prod","duration":"2h"}`, auth); rec.Code != 400 {
		t.Fatalf("a silence without a reason must be 400, got %d", rec.Code)
	}
	rec = doReq(t, mux, "POST", "/api/early-warning/silences", `{"profile":"orders-prod","rule":"capacity_*","duration":"2h","reason":"볼륨 증설"}`, auth)
	var created struct {
		Silence earlywarning.Silence `json:"silence"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &created) != nil || created.Silence.ID == "" {
		t.Fatalf("create silence: %d %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, mux, "GET", "/api/early-warning", "", auth)
	_ = json.Unmarshal(rec.Body.Bytes(), &board)
	if len(board.Silences) != 1 {
		t.Fatalf("the board must list the active silence: %+v", board.Silences)
	}
	if rec := doReq(t, mux, "DELETE", "/api/early-warning/silences/"+created.Silence.ID, "", auth); rec.Code != 200 {
		t.Fatalf("end silence: %d", rec.Code)
	}
	if rec := doReq(t, mux, "DELETE", "/api/early-warning/silences/"+created.Silence.ID, "", auth); rec.Code != 404 {
		t.Fatalf("ending twice must be 404, got %d", rec.Code)
	}
}

func TestProfileAlertingWebhookIsMaskedAndPreserved(t *testing.T) {
	s, mux := newAdminMux(t, "tok")
	auth := map[string]string{"X-Admin-Token": "tok", "Content-Type": "application/json"}
	profile := `{"id":"team-db","type":"postgres","connect_string":"h:5432/db","username":"u","password_ref":"env:X","alerting":{"webhook_ref":"plain:https://mm.example/hooks/SECRET","min_severity":"critical"}}`
	if rec := doReq(t, mux, "POST", "/api/db-profiles", profile, auth); rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	rec := doReq(t, mux, "GET", "/api/db-profiles", "", auth)
	if strings.Contains(rec.Body.String(), "SECRET") || !strings.Contains(rec.Body.String(), "plain:****") {
		t.Fatalf("a plain webhook must be masked like a password: %s", rec.Body.String())
	}
	masked := strings.Replace(profile, "plain:https://mm.example/hooks/SECRET", "plain:****", 1)
	if rec := doReq(t, mux, "PUT", "/api/db-profiles/team-db", masked, auth); rec.Code != 200 {
		t.Fatalf("saving the masked value back: %d %s", rec.Code, rec.Body.String())
	}
	stored, err := dbconn.GetProfile(s.opDir(), "team-db")
	if err != nil || stored.Alerting == nil || stored.Alerting.WebhookRef != "plain:https://mm.example/hooks/SECRET" {
		t.Fatalf("the stored webhook must survive a masked round trip: %+v %v", stored.Alerting, err)
	}
	if rec := doReq(t, mux, "POST", "/api/db-profiles", strings.Replace(masked, "team-db", "other-db", 1), auth); rec.Code != 400 {
		t.Fatalf("a masked placeholder must never be stored on create, got %d", rec.Code)
	}
	if rec := doReq(t, mux, "POST", "/api/db-profiles", strings.Replace(strings.Replace(profile, "team-db", "bad-db", 1), "https://mm.example/hooks/SECRET", "ftp://x", 1), auth); rec.Code != 400 {
		t.Fatalf("a non-http webhook must be rejected, got %d", rec.Code)
	}
}

func TestProfileAlertingWebhookMaskedInMetaMode(t *testing.T) {
	s, mux, _, alice := newAuthServer(t)
	hdr := withCookie(alice)
	hdr["Content-Type"] = "application/json"
	profile := `{"id":"team-db","type":"postgres","connect_string":"h:5432/db","username":"u","password_ref":"env:X","alerting":{"webhook_ref":"plain:https://mm.example/hooks/SECRET"}}`
	if rec := doReq(t, mux, "POST", "/api/db-profiles", profile, hdr); rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, mux, "GET", "/api/db-profiles", "", hdr); strings.Contains(rec.Body.String(), "SECRET") {
		t.Fatalf("the webhook secret leaked: %s", rec.Body.String())
	}
	masked := strings.Replace(profile, "plain:https://mm.example/hooks/SECRET", "plain:****", 1)
	if rec := doReq(t, mux, "PUT", "/api/db-profiles/team-db", masked, hdr); rec.Code != 200 {
		t.Fatalf("masked round trip: %d %s", rec.Code, rec.Body.String())
	}
	rec, err := s.Meta.Store.GetProfile(context.Background(), "team-db")
	if err != nil || !strings.Contains(string(rec.Definition), "hooks/SECRET") {
		t.Fatalf("the stored webhook must survive: %v %s", err, rec.Definition)
	}
	if rec := doReq(t, mux, "POST", "/api/db-profiles", strings.Replace(masked, "team-db", "other-db", 1), hdr); rec.Code != 400 {
		t.Fatalf("a masked placeholder must never be stored on create, got %d", rec.Code)
	}
}
