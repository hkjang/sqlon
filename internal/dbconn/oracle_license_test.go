package dbconn

import (
	"errors"
	"testing"
)

func TestOracleLicensePolicyDefaultsToBaseOnly(t *testing.T) {
	p := (&Profile{Type: "oracle"}).withDefaults()
	base := "SELECT sid, serial# FROM v$session"
	if err := EnforceOracleLicense(p, base); err != nil {
		t.Fatalf("base query rejected: %v", err)
	}
	for _, query := range []string{
		"SELECT * FROM v$active_session_history",
		"SELECT * FROM dba_hist_sqlstat",
		"SELECT DBMS_SQLTUNE.REPORT_TUNING_TASK('x') FROM dual",
	} {
		err := EnforceOracleLicense(p, query)
		var denied *OracleLicenseError
		if !errors.As(err, &denied) {
			t.Fatalf("licensed query was not policy-denied: %q err=%v", query, err)
		}
	}
}

func TestOracleTuningRequiresBothPackDeclarations(t *testing.T) {
	p := (&Profile{Type: "oracle", LicensePolicy: LicensePolicy{TuningPack: "enabled"}}).withDefaults()
	query := "SELECT DBMS_SQLTUNE.REPORT_TUNING_TASK('x') FROM dual"
	if err := EnforceOracleLicense(p, query); err == nil {
		t.Fatal("tuning query accepted without diagnostics pack")
	}
	p.LicensePolicy.DiagnosticsPack = "enabled"
	if err := EnforceOracleLicense(p, query); err != nil {
		t.Fatalf("declared tuning query rejected: %v", err)
	}
}

func TestOracleLicenseClassifierIgnoresCommentsAndLiterals(t *testing.T) {
	class, _ := ClassifyOracleQuery("SELECT 'DBA_HIST_SQLSTAT' label FROM dual /* V$ACTIVE_SESSION_HISTORY */")
	if class != OracleLicenseBase {
		t.Fatalf("class = %s, want base", class)
	}
}
