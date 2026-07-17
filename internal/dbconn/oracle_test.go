package dbconn

import (
	"strings"
	"testing"
)

func TestOracleProfileDefaultsAndReadOnlyGuard(t *testing.T) {
	p := (&Profile{ID: "ora-prod", Type: "oracle", ConnectString: "dbhost:1521/PRODPDB", Username: "monitor", PasswordRef: "env:ORA_PASSWORD"}).withDefaults()
	if p.LicensePolicy.DiagnosticsPack != "disabled" || p.LicensePolicy.TuningPack != "disabled" {
		t.Fatalf("Oracle packs must default disabled: %+v", p.LicensePolicy)
	}
	d, err := DialectFor("oracle")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateReadOnlySQL("SELECT * FROM v$session", d.DeniedExtras()); err != nil {
		t.Fatal(err)
	}
	if err := ValidateReadOnly(d, "WITH x AS (SELECT 1 n FROM dual) SELECT n FROM x", d.DeniedExtras()); err != nil {
		t.Fatalf("safe Oracle CTE rejected: %v", err)
	}
	if err := ValidateReadOnlySQL("SELECT * FROM v$session FOR UPDATE", d.DeniedExtras()); err == nil {
		t.Fatal("FOR UPDATE accepted")
	}
	if err := ValidateReadOnlySQL("BEGIN NULL; END;", d.DeniedExtras()); err == nil {
		t.Fatal("PL/SQL block accepted")
	}
	for _, unsafe := range []string{
		"SELECT * FROM users FOR UPDATE",
		"SELECT * FROM users; DELETE FROM users",
		"SELECT * FROM remote_table@prod_link",
		"SELECT DBMS_XPLAN.DISPLAY() FROM dual",
		"SELECT 'unterminated FROM dual",
	} {
		if err := ValidateReadOnly(d, unsafe, d.DeniedExtras()); err == nil {
			t.Errorf("unsafe Oracle SQL accepted: %q", unsafe)
		}
	}
}

func TestOracleProfileRejectsPrivilegedIdentityAndInvalidLicenseCombination(t *testing.T) {
	base := Profile{ID: "ora", Type: "oracle", ConnectString: "db:1521/PDB", Username: "sys", PasswordRef: "env:PW"}
	if err := base.Validate(); err == nil {
		t.Fatal("SYS monitoring profile accepted")
	}
	base.Username = "sqlon_monitor"
	base.LicensePolicy.TuningPack = "enabled"
	base.LicensePolicy.DiagnosticsPack = "disabled"
	if err := base.Validate(); err == nil {
		t.Fatal("tuning pack accepted without diagnostics pack")
	}
}

func TestOracleDSNEscapesCredentialsAndIncludesClientLibrary(t *testing.T) {
	d, _ := DialectFor("oracle")
	p := Profile{Type: "oracle", ConnectString: "db:1521/PDB", Username: `monitor" user`, Oracle: &OracleConfig{ClientLibDir: "/opt/oracle/client lib"}}
	dsn, err := d.BuildDSN(p, `secret" value`)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{`user="monitor\" user"`, `password="secret\" value"`, `connectString="db:1521/PDB"`, `libDir="/opt/oracle/client lib"`} {
		if !strings.Contains(dsn, part) {
			t.Fatalf("DSN %q does not contain %q", dsn, part)
		}
	}
}
