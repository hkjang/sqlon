package dbconn

import "testing"

func TestPoolDiagnoseHealthy(t *testing.T) {
	ps := PoolStat{MaxOpenConns: 10, MaxIdleConns: 2, OpenConnections: 3, InUse: 2, Idle: 1}
	a := ps.Diagnose()
	if a.Status != "healthy" || a.Code != "POOL_HEALTHY" {
		t.Fatalf("expected healthy, got %+v", a)
	}
}

func TestPoolDiagnoseContentionAndExhaustion(t *testing.T) {
	// Brief waits, pool not currently saturated → transient warning.
	transient := PoolStat{MaxOpenConns: 10, InUse: 3, WaitCount: 4, WaitDurationMs: 8}
	if a := transient.Diagnose(); a.Status != "warning" || a.Code != "POOL_TRANSIENT_WAIT" {
		t.Fatalf("transient: %+v", a)
	}
	// Sustained contention with long average wait → critical + recommendation.
	crit := PoolStat{MaxOpenConns: 10, InUse: 10, WaitCount: 100, WaitDurationMs: 8000}
	a := crit.Diagnose()
	if a.Status != "critical" || a.Code != "POOL_EXHAUSTION" {
		t.Fatalf("critical expected: %+v", a)
	}
	if a.AvgWaitMs != 80 || a.SuggestedMax != 15 || a.Recommendation == "" {
		t.Fatalf("avg/suggestion wrong: %+v", a)
	}
}

func TestPoolDiagnoseNearSaturationWithoutWaits(t *testing.T) {
	ps := PoolStat{MaxOpenConns: 10, InUse: 9, WaitCount: 0}
	a := ps.Diagnose()
	if a.Status != "warning" || a.Code != "POOL_NEAR_SATURATION" || a.SuggestedMax != 15 {
		t.Fatalf("near-saturation expected: %+v", a)
	}
}

func TestPoolDiagnoseIdleChurnNote(t *testing.T) {
	ps := PoolStat{MaxOpenConns: 20, MaxIdleConns: 2, OpenConnections: 8, InUse: 3, WaitCount: 0, MaxIdleClosed: 500}
	a := ps.Diagnose()
	if a.Status != "healthy" || len(a.Notes) == 0 {
		t.Fatalf("expected healthy with idle-churn note: %+v", a)
	}
	joined := ""
	for _, n := range a.Notes {
		joined += n
	}
	if !contains(joined, "max_idle_conns") {
		t.Fatalf("idle churn note missing max_idle_conns advice: %+v", a.Notes)
	}
}

func TestPoolDiagnoseOverProvisionNote(t *testing.T) {
	ps := PoolStat{MaxOpenConns: 40, MaxIdleConns: 10, OpenConnections: 5, InUse: 2, WaitCount: 0}
	a := ps.Diagnose()
	if a.Status != "healthy" || len(a.Notes) == 0 {
		t.Fatalf("expected over-provision advisory: %+v", a)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
