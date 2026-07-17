package dbconn

import "testing"

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
	if err := ValidateReadOnlySQL("SELECT * FROM v$session FOR UPDATE", d.DeniedExtras()); err == nil {
		t.Fatal("FOR UPDATE accepted")
	}
	if err := ValidateReadOnlySQL("BEGIN NULL; END;", d.DeniedExtras()); err == nil {
		t.Fatal("PL/SQL block accepted")
	}
}
