package catalog

import (
	"path/filepath"
	"strings"
	"testing"
)

func loadTestCatalog(t *testing.T) *Catalog {
	t.Helper()
	c, err := Load(filepath.Join("..", "..", "data", "metadb"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	return c
}

func TestLoadCatalog(t *testing.T) {
	c := loadTestCatalog(t)
	if len(c.Tables) == 0 {
		t.Fatal("expected tables")
	}
	if len(c.Relations) == 0 {
		t.Fatal("expected relations")
	}
	if len(c.Samples) == 0 {
		t.Fatal("expected samples")
	}
	if _, ok := c.ResolveTable("PUBLIC.JAMYPG_USERS"); !ok {
		t.Fatal("expected PUBLIC.JAMYPG_USERS")
	}
	if _, ok := c.ResolveTable("public.jamypg_mcp_activity"); !ok {
		t.Fatal("expected case-insensitive resolution of public.jamypg_mcp_activity")
	}
}

func TestSearchSchema(t *testing.T) {
	c := loadTestCatalog(t)
	res := c.SearchSchema(SearchRequest{
		Question:       "가장 많이 호출된 MCP 도구 상위 3개를 알려줘",
		TopK:           10,
		IncludeColumns: true,
	})
	if len(res.Results) == 0 {
		t.Fatal("expected search results")
	}
	foundActivity := false
	for _, r := range res.Results {
		if strings.Contains(r.Table, "JAMYPG_MCP_ACTIVITY") || strings.Contains(r.LogicalName, "활동") {
			foundActivity = true
			break
		}
	}
	if !foundActivity {
		t.Fatalf("expected the activity-log table in top results: %+v", res.Results)
	}
}

func TestJoinPath(t *testing.T) {
	c := loadTestCatalog(t)
	out, err := c.GetJoinPaths(JoinPathRequest{
		Tables:   []string{"PUBLIC.JAMYPG_MCP_ACTIVITY", "PUBLIC.JAMYPG_USERS"},
		MaxDepth: 2,
	})
	if err != nil {
		t.Fatalf("GetJoinPaths() error = %v", err)
	}
	paths := out["join_paths"].([]JoinPathResult)
	if len(paths) != 1 || !paths[0].Found {
		t.Fatalf("expected join path, got %+v", paths)
	}
}

func TestValidateSQL(t *testing.T) {
	c := loadTestCatalog(t)
	sql := `SELECT COUNT(*) AS CNT
FROM PUBLIC.JAMYPG_MCP_ACTIVITY T1
WHERE T1.STATUS = 'ok'
  AND T1.TOOL = 'run_sql_safely'
LIMIT 10`
	res := c.ValidateSQL(ValidateRequest{SQL: sql, Limit: 10})
	if !res.Valid {
		t.Fatalf("expected valid SQL, errors = %+v", res.Errors)
	}
}

func TestValidateSQLCarriesLint(t *testing.T) {
	c := loadTestCatalog(t)
	// SELECT * is valid but smelly → still Valid, but lint surfaces select_star
	res := c.ValidateSQL(ValidateRequest{SQL: "SELECT * FROM PUBLIC.JAMYPG_MCP_ACTIVITY LIMIT 10", Limit: 10})
	if !res.Valid {
		t.Fatalf("SELECT * should remain valid (lint is advisory), errors = %+v", res.Errors)
	}
	found := false
	for _, f := range res.Lint {
		if f.Rule == "select_star" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected advisory lint 'select_star' in ValidationResult, got %+v", res.Lint)
	}
}
