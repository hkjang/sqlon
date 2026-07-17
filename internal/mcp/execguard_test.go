package mcp

import (
	"testing"
	"time"

	"sqlon/internal/dbconn"
)

func TestMaskPIIResult(t *testing.T) {
	s, _ := newFixtureServer(t)
	// flag one fixture column as PII deterministically
	var piiCol string
	for _, tb := range s.cat().Tables {
		for _, col := range tb.Columns {
			col.PII = true
			piiCol = col.Name
			break
		}
		break
	}
	if piiCol == "" {
		t.Skip("fixture has no columns")
	}
	res := &dbconn.QueryResult{
		Columns: []dbconn.ColumnMeta{{Name: piiCol}, {Name: "SAFE_COL"}},
		Rows: []map[string]any{
			{piiCol: "주민번호값", "SAFE_COL": 1},
			{piiCol: "전화번호값", "SAFE_COL": 2},
		},
		RowCount: 2,
	}
	masked := s.maskPIIResult(res)
	if len(masked) != 1 || masked[0] != piiCol {
		t.Fatalf("masked=%v want [%s]", masked, piiCol)
	}
	for _, row := range res.Rows {
		if row[piiCol] != piiMask {
			t.Fatalf("PII value not masked: %v", row[piiCol])
		}
		if row["SAFE_COL"] == piiMask {
			t.Fatal("non-PII column must be untouched")
		}
	}
}

func TestResultCachePutGetExpiry(t *testing.T) {
	rc := newResultCache()
	key := cacheKey("p1", "SELECT 1", 100)
	res := &dbconn.QueryResult{RowCount: 1}
	rc.put(key, res, []string{"C"})

	got, masked, ok := rc.get(key)
	if !ok || got != res || len(masked) != 1 {
		t.Fatalf("cache miss after put: ok=%v", ok)
	}
	// different key → miss
	if _, _, ok := rc.get(cacheKey("p1", "SELECT 2", 100)); ok {
		t.Fatal("different SQL must miss")
	}
	if _, _, ok := rc.get(cacheKey("p2", "SELECT 1", 100)); ok {
		t.Fatal("different profile must miss")
	}
	// expiry
	rc.mu.Lock()
	e := rc.entries[key]
	e.expires = time.Now().Add(-time.Second)
	rc.entries[key] = e
	rc.mu.Unlock()
	if _, _, ok := rc.get(key); ok {
		t.Fatal("expired entry must miss")
	}
	// oversized results are not cached
	rc.put("big", &dbconn.QueryResult{RowCount: cacheMaxRows + 1}, nil)
	if _, _, ok := rc.get("big"); ok {
		t.Fatal("oversized result must not be cached")
	}
}

func TestAsyncJobLifecycleWithStubDriver(t *testing.T) {
	s, _ := newFixtureServer(t)
	job, refuse := s.submitAsyncQuery("dev-01", "SELECT 1 FROM DUAL", "alice", dbconn.ExecOptions{})
	if refuse != "" || job == nil {
		t.Fatalf("submit refused: %s", refuse)
	}
	// stub driver → job finishes quickly as failed
	deadline := time.Now().Add(3 * time.Second)
	var v *asyncJob
	for time.Now().Before(deadline) {
		got, ok := s.asyncJobs.jobView(job.ID)
		if ok && got.Status != "running" {
			v = got
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if v == nil || v.Status != "failed" || v.Error == "" {
		t.Fatalf("stub job should fail with an error, got %+v", v)
	}
	// ownership: bob cannot cancel alice's job; alice can
	if s.asyncJobs.cancelJob(job.ID, "bob", false) {
		t.Fatal("other user must not cancel the job")
	}
	if !s.asyncJobs.cancelJob(job.ID, "alice", false) {
		t.Fatal("owner cancel should succeed")
	}
	// per-user running limit
	for i := 0; i < jobMaxPerUser; i++ {
		s.asyncJobs.mu.Lock()
		s.asyncJobs.jobs["fake"+string(rune('a'+i))] = &asyncJob{ID: "f", User: "carol", Status: "running", StartedAt: time.Now()}
		s.asyncJobs.mu.Unlock()
	}
	if _, refuse := s.submitAsyncQuery("dev-01", "SELECT 1", "carol", dbconn.ExecOptions{}); refuse == "" {
		t.Fatal("per-user limit must refuse the 6th running job")
	}
}

func TestResultCacheSetTTLDisables(t *testing.T) {
	rc := newResultCache()
	if !rc.enabled() {
		t.Fatal("default cache should be enabled")
	}
	key := cacheKey("p", "SELECT 1", 10)
	res := &dbconn.QueryResult{RowCount: 1}

	// TTL 0 → disabled: put is a no-op, get always misses, entries flushed
	rc.put(key, res, nil)
	rc.SetTTL(0)
	if rc.enabled() {
		t.Fatal("TTL 0 must disable the cache")
	}
	if _, _, ok := rc.get(key); ok {
		t.Fatal("disabled cache must miss")
	}
	rc.put(key, res, nil)
	if _, _, ok := rc.get(key); ok {
		t.Fatal("put on disabled cache must be a no-op")
	}
	// re-enable with a short TTL and confirm a hit
	rc.SetTTL(30)
	rc.put(key, res, nil)
	if _, _, ok := rc.get(key); !ok {
		t.Fatal("re-enabled cache should hit")
	}
}
