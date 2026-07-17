package dbconn

import (
	"fmt"
	"regexp"
	"strings"
)

type OracleLicenseClass string

const (
	OracleLicenseBase        OracleLicenseClass = "base"
	OracleLicenseDiagnostics OracleLicenseClass = "diagnostics_pack"
	OracleLicenseTuning      OracleLicenseClass = "tuning_pack"
)

// OracleLicenseError is intentionally distinguishable from missing database
// privileges and unsupported engine features.
type OracleLicenseError struct {
	Class OracleLicenseClass
	Query string
}

func (e *OracleLicenseError) Error() string {
	return fmt.Sprintf("oracle_license_policy_denied: %s access is disabled for query %q", e.Class, e.Query)
}

var oracleLicensedPatterns = []struct {
	class   OracleLicenseClass
	name    string
	pattern *regexp.Regexp
}{
	{OracleLicenseTuning, "sql_tuning_advisor", regexp.MustCompile(`\b(DBMS_SQLTUNE|DBMS_SQLPA|DBMS_ADVISOR)\b`)},
	{OracleLicenseDiagnostics, "active_session_history", regexp.MustCompile(`\b(GV\$|V\$)ACTIVE_SESSION_HISTORY\b`)},
	{OracleLicenseDiagnostics, "awr_repository", regexp.MustCompile(`\b(DBA_HIST_[A-Z0-9_$#]+|DBMS_WORKLOAD_REPOSITORY|DBMS_ADDM|AWR_|ADDM_)\b`)},
}

// ClassifyOracleQuery returns the highest license class referenced by a query.
// Strings and comments are masked first so a label cannot trigger or bypass
// policy. Unknown system views remain base rather than being guessed.
func ClassifyOracleQuery(query string) (OracleLicenseClass, string) {
	upper := strings.ToUpper(stripSQL(query))
	for _, rule := range oracleLicensedPatterns {
		if rule.class == OracleLicenseTuning && rule.pattern.MatchString(upper) {
			return rule.class, rule.name
		}
	}
	for _, rule := range oracleLicensedPatterns {
		if rule.class == OracleLicenseDiagnostics && rule.pattern.MatchString(upper) {
			return rule.class, rule.name
		}
	}
	return OracleLicenseBase, "base_query"
}

func EnforceOracleLicense(p Profile, query string) error {
	if !strings.EqualFold(p.Type, "oracle") {
		return nil
	}
	class, name := ClassifyOracleQuery(query)
	switch class {
	case OracleLicenseBase:
		return nil
	case OracleLicenseDiagnostics:
		if strings.EqualFold(p.LicensePolicy.DiagnosticsPack, "enabled") {
			return nil
		}
	case OracleLicenseTuning:
		// Oracle Tuning Pack features depend on Diagnostics Pack entitlement;
		// SQLON therefore requires both operator declarations.
		if strings.EqualFold(p.LicensePolicy.DiagnosticsPack, "enabled") && strings.EqualFold(p.LicensePolicy.TuningPack, "enabled") {
			return nil
		}
	}
	return &OracleLicenseError{Class: class, Query: name}
}
