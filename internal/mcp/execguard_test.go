package mcp

import (
	"encoding/json"
	"net/http"
	"reflect"
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

// asyncJobOpts returns the ExecOptions the background job was launched with —
// the very struct handed to executeGuarded.
func asyncJobOpts(t *testing.T, s *Server, id string) (dbconn.ExecOptions, bool) {
	t.Helper()
	s.asyncJobs.mu.Lock()
	defer s.asyncJobs.mu.Unlock()
	j, ok := s.asyncJobs.jobs[id]
	if !ok {
		return dbconn.ExecOptions{}, false
	}
	return j.opts, true
}

// submitJob posts to the real async submit endpoint and returns the job id.
func submitJob(t *testing.T, mux *http.ServeMux, body, tok string) string {
	t.Helper()
	rec := doReq(t, mux, "POST", "/api/query/submit", body, withCookie(tok))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Submitted bool   `json:"submitted"`
		JobID     string `json:"job_id"`
		Poll      string `json:"poll"`
		TTL       int    `json:"result_ttl_minutes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode submit response: %v (%s)", err, rec.Body.String())
	}
	if !out.Submitted || out.JobID == "" || out.Poll != "/api/query/job/"+out.JobID || out.TTL != 10 {
		t.Fatalf("submit response contract changed: %s", rec.Body.String())
	}
	return out.JobID
}

// newBindServer boots a meta-mode server with a profile alice may query.
func newBindServer(t *testing.T) (*Server, *http.ServeMux, string) {
	t.Helper()
	s, mux, _, aliceTok := newAuthServer(t)
	profile := `{"id":"alice-db","connect_string":"h:1521/S","username":"APP_RO","password_ref":"env:X","visibility":"private"}`
	if rec := doReq(t, mux, "POST", "/api/db-profiles", profile, withCookie(aliceTok)); rec.Code != 200 {
		t.Fatalf("create profile: %d %s", rec.Code, rec.Body.String())
	}
	return s, mux, aliceTok
}

// The async submit endpoint must build the same ExecOptions.Binds as the sync
// one: a placeholder query submitted with binds has to reach the driver bound,
// not with zero arguments.
func TestAsyncSubmitPassesBindsToExecOptions(t *testing.T) {
	s, mux, aliceTok := newBindServer(t)

	const sql = "SELECT CUST_NO FROM TS.TBL1 WHERE CUST_NO = $1 AND USE_AMT > $2"
	body := `{"profile_id":"alice-db","sql":"` + sql + `","max_rows":33,"timeout_seconds":7,` +
		`"binds":["C-1",1500],"user":"mallory"}`
	id := submitJob(t, mux, body, aliceTok)

	opts, ok := asyncJobOpts(t, s, id)
	if !ok {
		t.Fatal("job vanished from the store right after submit")
	}
	want := []any{"C-1", float64(1500)} // JSON numbers decode to float64, as on the sync path
	if !reflect.DeepEqual(opts.Binds, want) {
		t.Fatalf("job ran with Binds=%#v, want %#v", opts.Binds, want)
	}
	if opts.MaxRows != 33 || opts.TimeoutSeconds != 7 {
		t.Fatalf("limits not propagated: %+v", opts)
	}
	// the async path must keep forcing the authenticated actor, never the body's user
	if opts.User != "alice" {
		t.Fatalf("User must be the authenticated actor, got %q", opts.User)
	}
}

// Both query endpoints have to accept one and the same request body, so that a
// caller can move a placeholder query from sync to async without rewriting it
// (or concatenating parameters into the SQL).
func TestSyncAndAsyncAcceptTheSameQueryBody(t *testing.T) {
	s, mux, aliceTok := newBindServer(t)
	const body = `{"profile_id":"alice-db","sql":"SELECT CUST_NO FROM TS.TBL1 WHERE CUST_NO = $1",` +
		`"max_rows":11,"timeout_seconds":3,"binds":[42]}`

	// sync: reaches the driver (which cannot connect here) rather than being rejected
	rec := doReq(t, mux, "POST", "/api/query/execute", body, withCookie(aliceTok))
	if rec.Code != 200 {
		t.Fatalf("sync execute: %d %s", rec.Code, rec.Body.String())
	}
	var sync map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &sync); err != nil {
		t.Fatal(err)
	}
	if reason, _ := sync["reason"].(string); reason != "" {
		t.Fatalf("sync execute refused the body before execution: %s", rec.Body.String())
	}

	// async: same body, and the binds in it reach the job's ExecOptions
	opts, ok := asyncJobOpts(t, s, submitJob(t, mux, body, aliceTok))
	if !ok {
		t.Fatal("job vanished from the store right after submit")
	}
	if !reflect.DeepEqual(opts.Binds, []any{float64(42)}) {
		t.Fatalf("async lost the binds the sync path accepts: %#v", opts.Binds)
	}
}

// Omitting binds keeps the old behaviour byte for byte: nil Binds, 202 body
// unchanged.
func TestAsyncSubmitWithoutBindsStaysNil(t *testing.T) {
	s, mux, aliceTok := newBindServer(t)
	id := submitJob(t, mux, `{"profile_id":"alice-db","sql":"SELECT CUST_NO FROM TS.TBL1"}`, aliceTok)
	opts, ok := asyncJobOpts(t, s, id)
	if !ok {
		t.Fatal("job vanished from the store right after submit")
	}
	if opts.Binds != nil {
		t.Fatalf("omitted binds must stay nil, got %#v", opts.Binds)
	}
	// the polled view must not have grown a binds field
	rec := doReq(t, mux, "GET", "/api/query/job/"+id, "", withCookie(aliceTok))
	if rec.Code != 200 {
		t.Fatalf("poll: %d %s", rec.Code, rec.Body.String())
	}
	var view map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"binds", "opts", "exec_options"} {
		if _, bad := view[k]; bad {
			t.Fatalf("job view must not expose %q: %s", k, rec.Body.String())
		}
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
