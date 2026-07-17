package mcp

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sqlon/internal/catalog"
)

func newAuditServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	s := &Server{}
	s.setCatalog(&catalog.Catalog{DataDir: dir, Tables: map[string]*catalog.Table{}})
	return s, dir
}

func TestAuditChainAppendsAndVerifies(t *testing.T) {
	s, _ := newAuditServer(t)
	for i := 0; i < 5; i++ {
		s.appendAudit(map[string]any{"tool": "t", "n": i})
	}
	res := s.VerifyAuditChain("")
	if res["valid"] != true {
		t.Fatalf("expected valid chain, got %+v", res)
	}
	if res["entries"].(int) != 5 {
		t.Fatalf("expected 5 entries, got %v", res["entries"])
	}
}

func TestCanonicalJSONStableOrder(t *testing.T) {
	a := canonicalJSON(map[string]any{"b": 1, "a": 2, "c": 3})
	b := canonicalJSON(map[string]any{"c": 3, "a": 2, "b": 1})
	if a != b {
		t.Fatalf("canonical JSON not order-stable: %q vs %q", a, b)
	}
	if a != `{"a":2,"b":1,"c":3}` {
		t.Fatalf("unexpected canonical form: %q", a)
	}
}

func auditFilePath(dir string) (string, error) {
	adir := filepath.Join(dir, "audit")
	entries, err := os.ReadDir(adir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "audit-") {
			return filepath.Join(adir, e.Name()), nil
		}
	}
	return "", os.ErrNotExist
}

func TestAuditChainDetectsTampering(t *testing.T) {
	s, dir := newAuditServer(t)
	for i := 0; i < 4; i++ {
		s.appendAudit(map[string]any{"tool": "t", "detail": "d", "n": i})
	}
	path, err := auditFilePath(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	// tamper with the 2nd entry's content, keeping its hash
	lines[1] = strings.Replace(lines[1], `"detail":"d"`, `"detail":"HACKED"`, 1)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := s.VerifyAuditChain("")
	if res["valid"] != false {
		t.Fatalf("tampering should break the chain: %+v", res)
	}
	if res["broken_at_line"].(int) != 2 {
		t.Fatalf("expected break at line 2, got %v", res["broken_at_line"])
	}
}

func TestAuditChainDetectsDeletion(t *testing.T) {
	s, dir := newAuditServer(t)
	for i := 0; i < 4; i++ {
		s.appendAudit(map[string]any{"tool": "t", "n": i})
	}
	path, _ := auditFilePath(dir)
	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	// delete the 2nd line
	lines = append(lines[:1], lines[2:]...)
	_ = os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	res := s.VerifyAuditChain("")
	if res["valid"] != false {
		t.Fatalf("deletion should break the chain: %+v", res)
	}
}

func TestMetricsRecordAndRender(t *testing.T) {
	s, _ := newAuditServer(t)
	s.metrics = newMetricsRegistry()
	s.recordToolMetric("run_sql_safely", 12, false)
	s.recordToolMetric("run_sql_safely", 8, true)
	s.recordToolMetric("validate_sql", 3, false)

	rr := httptest.NewRecorder()
	s.serveMetrics(rr, nil)
	body := rr.Body.String()
	for _, want := range []string{
		`sqlon_up 1`,
		`sqlon_tool_calls_total{tool="run_sql_safely",status="ok"} 1`,
		`sqlon_tool_calls_total{tool="run_sql_safely",status="error"} 1`,
		`sqlon_tool_duration_ms_sum{tool="run_sql_safely"} 20`,
		`sqlon_catalog_tables 0`,
		`sqlon_metadata_quality_score`,
		// Deprecated aliases preserve existing dashboards for one release.
		`jamypg_up 1`,
		`jamypg_tool_calls_total{tool="run_sql_safely",status="ok"} 1`,
		`jamypg_tool_calls_total{tool="run_sql_safely",status="error"} 1`,
		`jamypg_tool_duration_ms_sum{tool="run_sql_safely"} 20`,
		`jamypg_catalog_tables 0`,
		`jamypg_metadata_quality_score`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q\n---\n%s", want, body)
		}
	}
}
