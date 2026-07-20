package mcp

import (
	"testing"

	"sqlon/internal/change"
)

func TestIncidentRankingOrdersBySeverity(t *testing.T) {
	sig := incidentSignals{
		LockEdges:     3,
		RootBlocker:   "42",
		RootAffected:  2,
		Waiting:       1,
		Drifted:       2,
		MaintCritical: 1,
	}
	hs := rankIncidentHypotheses(sig)
	if len(hs) < 3 {
		t.Fatalf("expected several hypotheses, got %d", len(hs))
	}
	// maintenance-critical (95) must outrank lock contention (90) and config drift (55)
	if hs[0].Category != "maintenance" || hs[0].Rank != 1 {
		t.Fatalf("wraparound-critical must rank first: %+v", hs[0])
	}
	if hs[1].Category != "lock_contention" {
		t.Fatalf("lock contention must rank second: %+v", hs[1])
	}
	// scores must be monotonically non-increasing
	for i := 1; i < len(hs); i++ {
		if hs[i].Score > hs[i-1].Score {
			t.Fatalf("hypotheses not sorted by score: %+v", hs)
		}
	}
	if incidentStatus(hs) != "critical" {
		t.Fatalf("a 95-score hypothesis must make the incident critical")
	}
}

func TestIncidentFailedChangeIsHighConfidence(t *testing.T) {
	sig := incidentSignals{
		RecentChanges: []incidentChange{
			{ID: "chg-1", Target: "orders", State: string(change.Failed), Risk: "high", Reason: "add index"},
		},
	}
	hs := rankIncidentHypotheses(sig)
	if len(hs) != 1 || hs[0].Category != "recent_change" || hs[0].Confidence != "high" {
		t.Fatalf("failed change must be a single high-confidence hypothesis: %+v", hs)
	}
	if len(hs[0].Evidence) == 0 {
		t.Fatalf("hypothesis must carry evidence")
	}
}

func TestIncidentNoSignalsIsOK(t *testing.T) {
	hs := rankIncidentHypotheses(incidentSignals{})
	if len(hs) != 0 {
		t.Fatalf("no signals must yield no hypotheses: %+v", hs)
	}
	if incidentStatus(hs) != "ok" {
		t.Fatalf("no hypotheses must be status ok")
	}
}

func TestIncidentWaitingOnlyIsDegradedNotCritical(t *testing.T) {
	hs := rankIncidentHypotheses(incidentSignals{Waiting: 2})
	if len(hs) != 1 || hs[0].Category != "waiting_sessions" {
		t.Fatalf("expected a single waiting-sessions hypothesis: %+v", hs)
	}
	if incidentStatus(hs) != "degraded" {
		t.Fatalf("a 40-score signal should be degraded, got %s", incidentStatus(hs))
	}
	if hs[0].Confidence != "low" {
		t.Fatalf("a 40-score hypothesis should be low confidence: %+v", hs[0])
	}
}

func TestIncidentLongRunningSupersedesWaiting(t *testing.T) {
	// When long-running sessions exist, the waiting-only hypothesis is not
	// additionally emitted (long-running is the stronger, more actionable one).
	hs := rankIncidentHypotheses(incidentSignals{LongRunning: 1, Waiting: 3})
	count := 0
	for _, h := range hs {
		if h.Category == "long_running_sessions" || h.Category == "waiting_sessions" {
			count++
		}
	}
	if count != 1 || hs[0].Category != "long_running_sessions" {
		t.Fatalf("long-running should supersede waiting: %+v", hs)
	}
}
