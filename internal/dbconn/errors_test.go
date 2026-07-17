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
