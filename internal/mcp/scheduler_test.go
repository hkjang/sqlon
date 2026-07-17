package mcp

import (
	"context"
	"testing"

	"sqlon/internal/catalog"
)

func TestSchedulerConfigEnabled(t *testing.T) {
	cases := []struct {
		cfg  SchedulerConfig
		want bool
	}{
		{SchedulerConfig{}, false},
		{SchedulerConfig{Source: "pg"}, true},
		{SchedulerConfig{WebhookURL: "http://x"}, true},
		{SchedulerConfig{OpenMetadata: true}, true},
	}
	for _, c := range cases {
		if got := c.cfg.enabled(); got != c.want {
			t.Errorf("enabled(%+v)=%v want %v", c.cfg, got, c.want)
		}
	}
}

func TestRunScheduledOMImportSkipsWhenUnconfigured(t *testing.T) {
	s := &Server{}
	s.setCatalog(&catalog.Catalog{DataDir: t.TempDir(), Tables: map[string]*catalog.Table{}})
	// no OpenMetadata config → must return cleanly without touching anything
	s.runScheduledOMImport(context.Background(), "")
	// an audit entry should NOT have been written (skipped before any work)
	res := s.VerifyAuditChain("")
	if n, _ := res["entries"].(int); n != 0 {
		t.Fatalf("unconfigured import must not audit anything, got %d entries", n)
	}
}
