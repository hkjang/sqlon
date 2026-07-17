package oracle

import "testing"

func TestParseDataGuardInterval(t *testing.T) {
	cases := []struct {
		value string
		want  float64
		ok    bool
	}{
		{"+00 00:00:00", 0, true},
		{"+00 00:05:00", 300, true},
		{"+01 02:03:04", 93784, true},
		{"", 0, false},
		{"unknown", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseDataGuardInterval(tc.value)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("parseDataGuardInterval(%q) = %v,%v want %v,%v", tc.value, got, ok, tc.want, tc.ok)
		}
	}
}
