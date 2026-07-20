package app

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRuntimeParsesFullSQLONCommandSurface(t *testing.T) {
	env := map[string]string{"SQLON_ADMIN_TOKEN": "new", "JAMYPG_ADMIN_TOKEN": "old", "SQLON_META_DB": "postgres://meta"}
	rt := Runtime{Stderr: &bytes.Buffer{}, Getenv: func(k string) string { return env[k] }}
	cfg, err := rt.parse([]string{"-transport", "streamable-http", "-addr", "0.0.0.0:9797", "-endpoint", "/sqlon-mcp", "-stateless", "-sse-post", "-sync-interval", "5m", "-openmetadata-sync", "-dba-digest"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.adminToken != "new" || cfg.metaDSN != "postgres://meta" {
		t.Fatalf("SQLON env precedence failed: %+v", cfg)
	}
	if cfg.transport != "streamable-http" || cfg.endpoint != "/sqlon-mcp" || !cfg.stateless || !cfg.ssePost || cfg.syncInterval.String() != "5m0s" || !cfg.omSync || !cfg.dbaDigest {
		t.Fatalf("flags not preserved: %+v", cfg)
	}
	if filepath.ToSlash(cfg.dataDir) != "data/sqlon" || !cfg.autoMigrate {
		t.Fatalf("default migration settings not applied: %+v", cfg)
	}
}

func TestRuntimeDisablesAutomaticMigrationForExplicitDataLocation(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{name: "SQLON environment", env: map[string]string{"SQLON_DATA_DIR": "/srv/sqlon"}, want: "/srv/sqlon"},
		{name: "legacy environment", env: map[string]string{"JAMYPG_DATA_DIR": "/srv/jamypg"}, want: "/srv/jamypg"},
		{name: "command flag", args: []string{"-data", "/srv/chosen"}, want: "/srv/chosen"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt := Runtime{Stderr: &bytes.Buffer{}, Getenv: func(k string) string { return tc.env[k] }}
			cfg, err := rt.parse(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.dataDir != tc.want || cfg.autoMigrate {
				t.Fatalf("explicit data location must be preserved: %+v", cfg)
			}
		})
	}
}

func TestRuntimeSupportsLegacyEnvironmentAlias(t *testing.T) {
	rt := Runtime{Stderr: &bytes.Buffer{}, Getenv: func(k string) string {
		if k == "JAMYPG_ADMIN_TOKEN" {
			return "legacy"
		}
		return ""
	}}
	cfg, err := rt.parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.adminToken != "legacy" {
		t.Fatalf("legacy alias not honored: %q", cfg.adminToken)
	}
}

func TestRuntimeObservationCollectorPolicyFromSQLONAndLegacyEnvironment(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"SQLON":  {"SQLON_OBSERVE_INTERVAL": "45s", "SQLON_OBSERVE_RETENTION_DAYS": "14"},
		"legacy": {"JAMYPG_OBSERVE_INTERVAL": "2m", "JAMYPG_OBSERVE_RETENTION_DAYS": "7"},
	} {
		t.Run(name, func(t *testing.T) {
			rt := Runtime{Stderr: &bytes.Buffer{}, Getenv: func(k string) string { return env[k] }}
			cfg, err := rt.parse(nil)
			if err != nil {
				t.Fatal(err)
			}
			wantInterval, _ := time.ParseDuration(env[map[bool]string{true: "SQLON_OBSERVE_INTERVAL", false: "JAMYPG_OBSERVE_INTERVAL"}[name == "SQLON"]])
			if cfg.observeInterval != wantInterval || cfg.observeRetentionDays <= 0 {
				t.Fatalf("observation policy not parsed: %+v", cfg)
			}
		})
	}
	rt := Runtime{Stderr: &bytes.Buffer{}, Getenv: func(k string) string {
		if k == "SQLON_OBSERVE_INTERVAL" {
			return "not-a-duration"
		}
		return ""
	}}
	if _, err := rt.parse(nil); err == nil {
		t.Fatal("invalid observation interval accepted")
	}
}

func TestValidateHTTPExposureUsesSQLONGuidance(t *testing.T) {
	err := ValidateHTTPExposure("http", "0.0.0.0:9797", "", "", true)
	if err == nil || !strings.Contains(err.Error(), "SQLON_ADMIN_TOKEN") || strings.Contains(err.Error(), "JAMYPG_ADMIN_TOKEN") {
		t.Fatalf("unexpected guidance: %v", err)
	}
}
