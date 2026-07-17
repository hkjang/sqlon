package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sqlon/internal/catalog"
)

// newFixtureServer builds a server over a tiny sandbox data dir so dataset
// mutations never touch the real data/kcb.
func newFixtureServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	writeFile := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("meta_physical_models.json", `[
	 {"schema_name":"TS","table_name":"TBL1","column_order":"1","column_name":"CUST_NO","data_type":"VARCHAR2","length_precision":"10","is_pk":"Y","is_fk":"N","description":"테스트 테이블"},
	 {"schema_name":"TS","table_name":"TBL1","column_order":"2","column_name":"USE_AMT","data_type":"NUMBER","length_precision":"15","is_pk":"N","is_fk":"N","description":"이용금액"}
	]`)
	writeFile("meta_logical_models.json", `[
	 {"schema_name":"TS","entity_name_en":"TBL1","entity_name_ko":"테스트_고객","entity_order":"1","attribute_name_ko":"고객번호","attribute_name_en":"CUST_NO","data_type":"VARCHAR2","is_pk":"Y","is_fk":"N","description":""}
	]`)
	c, err := catalog.Load(dir)
	if err != nil {
		t.Fatalf("Load fixture: %v", err)
	}
	return NewServer(c, Options{Stateful: false}), dir
}

func callToolJSON(t *testing.T, s *Server, name, args string) (any, error) {
	t.Helper()
	params, _ := json.Marshal(map[string]any{"name": name, "arguments": json.RawMessage(args)})
	return s.callTool(t.Context(), params)
}

func TestListAndGetDatasets(t *testing.T) {
	s, _ := newFixtureServer(t)
	res, err := callToolJSON(t, s, "list_datasets", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	datasets := res.(map[string]any)["datasets"].([]map[string]any)
	if len(datasets) != len(catalog.DatasetRegistry) {
		t.Fatalf("expected %d datasets, got %d", len(catalog.DatasetRegistry), len(datasets))
	}
	byName := map[string]map[string]any{}
	for _, d := range datasets {
		byName[d["name"].(string)] = d
	}
	if byName["physical_models"]["present"] != true || byName["physical_models"]["required"] != true {
		t.Fatalf("physical_models status wrong: %+v", byName["physical_models"])
	}
	if byName["glossary"]["present"] != false {
		t.Fatalf("glossary should be absent in fixture: %+v", byName["glossary"])
	}
	got, err := callToolJSON(t, s, "get_dataset", `{"name":"physical_models","sample_rows":1}`)
	if err != nil {
		t.Fatal(err)
	}
	g := got.(map[string]any)
	if g["total_entries"].(int) != 2 || len(g["sample"].([]any)) != 1 {
		t.Fatalf("unexpected sample: %+v", g)
	}
	if _, err := callToolJSON(t, s, "get_dataset", `{"name":"nope"}`); err == nil {
		t.Fatal("unknown dataset must error")
	}
}

func TestPutDatasetAppliesAndHotSwaps(t *testing.T) {
	s, dir := newFixtureServer(t)
	before := s.cat()
	res, err := callToolJSON(t, s, "put_dataset", `{"name":"glossary","content":{"entries":[{"term":"고객","synonyms":["cust","cust_no"],"category":"entity"}]}}`)
	if err != nil {
		t.Fatal(err)
	}
	r := res.(map[string]any)
	if r["applied"] != true {
		t.Fatalf("expected applied, got %+v", r)
	}
	if s.cat() == before {
		t.Fatal("catalog was not hot-swapped")
	}
	if len(s.cat().Glossary.Entries) != 1 {
		t.Fatalf("new glossary not loaded: %+v", s.cat().Glossary)
	}
	if _, err := os.Stat(filepath.Join(dir, "glossary.json")); err != nil {
		t.Fatal("glossary.json not written")
	}
	// wrong shape: glossary expects an object
	if _, err := callToolJSON(t, s, "put_dataset", `{"name":"glossary","content":[1,2]}`); err == nil || !strings.Contains(err.Error(), "JSON object") {
		t.Fatalf("expected shape error, got %v", err)
	}
}

func TestPutDatasetRollsBackOnNewErrors(t *testing.T) {
	s, dir := newFixtureServer(t)
	// metric referencing an unknown table -> load error -> rollback
	res, err := callToolJSON(t, s, "put_dataset", `{"name":"metrics","content":[{"name":"엉터리","expression":"SUM(X)","tables":["NO.SUCH_TABLE"]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	r := res.(map[string]any)
	if r["applied"] != false {
		t.Fatalf("expected rollback, got %+v", r)
	}
	if _, err := os.Stat(filepath.Join(dir, "metrics.json")); !os.IsNotExist(err) {
		t.Fatal("metrics.json should have been rolled back (removed)")
	}
	if len(s.cat().Metrics) != 0 {
		t.Fatalf("catalog should not contain the bad metric: %+v", s.cat().Metrics)
	}
	// force=true applies despite the error
	res, err = callToolJSON(t, s, "put_dataset", `{"name":"metrics","content":[{"name":"엉터리","expression":"SUM(X)","tables":["NO.SUCH_TABLE"]}],"force":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.(map[string]any)["applied"] != true {
		t.Fatalf("force should apply, got %+v", res)
	}
	if len(s.cat().Metrics) != 1 {
		t.Fatal("forced metric not loaded")
	}
}

func TestRemoveDatasetAndGuards(t *testing.T) {
	s, dir := newFixtureServer(t)
	if _, err := callToolJSON(t, s, "put_dataset", `{"name":"column_stats","content":[{"schema_name":"TS","table_name":"TBL1","column_name":"USE_AMT","row_count":100}]}`); err != nil {
		t.Fatal(err)
	}
	res, err := callToolJSON(t, s, "remove_dataset", `{"name":"column_stats"}`)
	if err != nil {
		t.Fatal(err)
	}
	r := res.(map[string]any)
	if r["removed"] != true || r["backup"] == "" {
		t.Fatalf("expected removal with backup, got %+v", r)
	}
	if _, err := os.Stat(filepath.Join(dir, "column_stats.json")); !os.IsNotExist(err) {
		t.Fatal("column_stats.json should be gone")
	}
	if _, err := callToolJSON(t, s, "remove_dataset", `{"name":"physical_models"}`); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("required dataset removal must be refused, got %v", err)
	}
	if _, err := callToolJSON(t, s, "remove_dataset", `{"name":"audit"}`); err == nil {
		t.Fatal("system-managed dataset removal must be refused")
	}
	if _, err := callToolJSON(t, s, "put_dataset", `{"name":"feedback","content":[]}`); err == nil {
		t.Fatal("system-managed dataset replacement must be refused")
	}
}

func TestReloadCatalog(t *testing.T) {
	s, dir := newFixtureServer(t)
	// simulate a direct file edit on a mounted volume
	if err := os.WriteFile(filepath.Join(dir, "glossary.json"),
		[]byte(`{"entries":[{"term":"잔액","synonyms":["bal"],"category":"metric"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := callToolJSON(t, s, "reload_catalog", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.(map[string]any)["reloaded"] != true {
		t.Fatalf("expected reload, got %+v", res)
	}
	if len(s.cat().Glossary.Entries) != 1 || s.cat().Glossary.Entries[0].Term != "잔액" {
		t.Fatalf("directly-edited glossary not picked up: %+v", s.cat().Glossary)
	}
}
