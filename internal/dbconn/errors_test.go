package dbconn

import (
	"errors"
	"testing"
)

func TestConnectionDiagnosticMySQLFailures(t *testing.T) {
	p := Profile{Type: "mysql"}
	tests := []struct {
		message string
		want    string
	}{
		{"Error 1045 (28000): Access denied for user 'app'@'host'", "authentication"},
		{"Error 1049 (42000): Unknown database 'missing'", "database"},
		{"dial tcp 127.0.0.1:3306: connect: connection refused", "network"},
		{"Error 1193 (HY000): Unknown system variable 'transaction_read_only'", "compatibility"},
		{"environment variable MYSQL_PASSWORD is not set", "secret"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got, hint, steps := connectionDiagnostic(errors.New(tt.message), p)
			if got != tt.want || hint == "" || len(steps) == 0 {
				t.Fatalf("diagnostic = %q, %q, %v; want category %q with guidance", got, hint, steps, tt.want)
			}
		})
	}
}

// godror puts its connect params in the message; "connectionClassLength"
// contains "ssl", which used to turn a wrong Oracle password into a TLS hint.
const godrorParams = `user="sqlon_monitor" standalone params={authMode:0 connectionClass:<nil> connectionClassLength:0 purity:0 newPassword:<nil> newPasswordLength:0}: `

func TestConnectionDiagnosticOracleFailures(t *testing.T) {
	p := Profile{Type: "oracle"}
	tests := []struct {
		message string
		want    string
		code    string
	}{
		{godrorParams + "ORA-01017: invalid credential or not authorized; logon denied", "authentication", "ORA-01017"},
		{godrorParams + "ORA-28000: The account is locked.", "authentication", "ORA-28000"},
		{godrorParams + "ORA-12514: Cannot connect to database. Service NOPDB is not registered with the listener", "database", "ORA-12514"},
		{godrorParams + "ORA-12541: Cannot connect. No listener at host db port 1521.", "network", "ORA-12541"},
		{godrorParams + "ORA-12170: Cannot connect. TCP connect timeout of 60s", "timeout", "ORA-12170"},
		{godrorParams + "ORA-03113: end-of-file on communication channel", "driver", "ORA-03113"},
		{"tls: failed to verify certificate: x509: certificate signed by unknown authority", "tls", "INTERNAL"},
		{"SSL is not enabled on the server", "tls", "INTERNAL"},
	}
	for _, tt := range tests {
		t.Run(tt.code+"/"+tt.want, func(t *testing.T) {
			err := errors.New(tt.message)
			got, hint, steps := connectionDiagnostic(err, p)
			if got != tt.want || hint == "" || len(steps) == 0 {
				t.Fatalf("diagnostic = %q, %q, %v; want category %q with guidance", got, hint, steps, tt.want)
			}
			if code := dbErrCode(err); code != tt.code {
				t.Fatalf("dbErrCode = %q, want %q", code, tt.code)
			}
		})
	}
}
