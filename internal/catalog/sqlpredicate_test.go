package catalog

import "testing"

func TestDefaultFilterRequiresExactPredicateClause(t *testing.T) {
	c := loadTestCatalog(t)
	c.Overrides.DefaultFilters = []DefaultFilter{{
		PolicyID: "activity-status", Table: "PUBLIC.JAMYPG_MCP_ACTIVITY",
		Condition: "STATUS = 'ok'", Reason: "exclude failed activity",
	}}

	tests := []struct {
		name    string
		sql     string
		missing bool
	}{
		{"qualified exact predicate", "SELECT COUNT(*) FROM PUBLIC.JAMYPG_MCP_ACTIVITY a WHERE a.STATUS = 'ok'", false},
		{"format and comment tolerant", "SELECT COUNT(*) FROM PUBLIC.JAMYPG_MCP_ACTIVITY a WHERE /* policy */ ( a.status='ok' )", false},
		{"different literal", "SELECT COUNT(*) FROM PUBLIC.JAMYPG_MCP_ACTIVITY a WHERE a.STATUS = 'failed'", true},
		{"literal case must match", "SELECT COUNT(*) FROM PUBLIC.JAMYPG_MCP_ACTIVITY a WHERE a.STATUS = 'OK'", true},
		{"select mention is not predicate", "SELECT STATUS, COUNT(*) FROM PUBLIC.JAMYPG_MCP_ACTIVITY a GROUP BY STATUS", true},
		{"string mention cannot spoof", "SELECT COUNT(*) FROM PUBLIC.JAMYPG_MCP_ACTIVITY a WHERE a.TOOL = 'STATUS = ''ok'''", true},
		{"dollar string cannot spoof", "SELECT COUNT(*) FROM PUBLIC.JAMYPG_MCP_ACTIVITY a WHERE a.TOOL = $$ STATUS = 'ok' $$", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := c.ValidateSQL(ValidateRequest{SQL: tt.sql})
			if hasValidationCode(got.Warnings, "DEFAULT_FILTER_MISSING") != tt.missing {
				t.Fatalf("warnings=%+v, missing=%v", got.Warnings, tt.missing)
			}
		})
	}
}

func TestDefaultFilterBindsExplicitAliasToTable(t *testing.T) {
	c := loadTestCatalog(t)
	c.Overrides.DefaultFilters = []DefaultFilter{{
		Table: "PUBLIC.JAMYPG_MCP_ACTIVITY", Condition: "STATUS = 'ok'",
	}}
	sql := `SELECT COUNT(*)
FROM PUBLIC.JAMYPG_MCP_ACTIVITY a
JOIN PUBLIC.JAMYPG_USERS u ON u.ID = a.USER_ID
WHERE u.STATUS = 'ok'`
	got := c.ValidateSQL(ValidateRequest{SQL: sql})
	if !hasValidationCode(got.Warnings, "DEFAULT_FILTER_MISSING") {
		t.Fatalf("another table's qualified STATUS must not satisfy policy: %+v", got.Warnings)
	}
}

func TestDefaultFilterCanBeEnforcedAsError(t *testing.T) {
	c := loadTestCatalog(t)
	c.Overrides.DefaultFilters = []DefaultFilter{{
		PolicyID: "tenant-boundary", Table: "PUBLIC.JAMYPG_MCP_ACTIVITY",
		Condition: "USER_ID = 'tenant-1'", Enforcement: "error",
	}}
	got := c.ValidateSQL(ValidateRequest{
		SQL: "SELECT COUNT(*) FROM PUBLIC.JAMYPG_MCP_ACTIVITY a WHERE a.STATUS = 'ok'",
	})
	if got.Valid || !hasValidationCode(got.Errors, "DEFAULT_FILTER_MISSING") {
		t.Fatalf("enforcement=error must block SQL: %+v", got)
	}
}

func hasValidationCode(issues []ValidationIssue, code string) bool {
	for _, issue := range issues {
		if issue.Code == code {
			return true
		}
	}
	return false
}
