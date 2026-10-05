package dbconn

import "testing"

func TestParseByteSize(t *testing.T) {
	for in, want := range map[string]int64{
		"500GiB": 500 << 30, "500 GiB": 500 << 30, "500G": 500 << 30, "2TB": 2e12, "1.5TiB": 3 << 39,
		"750gb": 750e9, "1073741824": 1 << 30, "64MiB": 64 << 20,
	} {
		got, err := ParseByteSize(in)
		if err != nil || got != want {
			t.Fatalf("ParseByteSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "GiB", "-5GiB", "12 parsecs", "0", "1e400"} {
		if _, err := ParseByteSize(bad); err == nil {
			t.Fatalf("ParseByteSize(%q) must fail", bad)
		}
	}
}

func TestCapacityConfigValidation(t *testing.T) {
	base := Profile{ID: "p", ConnectString: "h:5432/db", Username: "u", PasswordRef: "env:X"}
	ok := base
	ok.Capacity = &CapacityConfig{StorageLimit: "500GiB", WarnPercent: 75, CriticalPercent: 85, WarnDays: 21, CriticalDays: 5}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid capacity rejected: %v", err)
	}
	for name, c := range map[string]*CapacityConfig{
		"bad size":            {StorageLimit: "lots"},
		"percent above 100":   {WarnPercent: 120},
		"critical below warn": {WarnPercent: 90, CriticalPercent: 80},
		"days inverted":       {WarnDays: 3, CriticalDays: 10},
	} {
		p := base
		p.Capacity = c
		if err := p.Validate(); err == nil {
			t.Fatalf("%s: expected a validation error", name)
		}
	}
	var none *CapacityConfig
	if w, c, wd, cd := none.Thresholds(); w != 80 || c != 90 || wd != 14 || cd != 3 {
		t.Fatalf("defaults wrong: %v %v %v %v", w, c, wd, cd)
	}
	if n, err := none.LimitBytes(); n != 0 || err != nil {
		t.Fatalf("nil config must mean no limit")
	}
}
