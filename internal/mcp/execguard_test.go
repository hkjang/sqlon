package mcp

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
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

func mustCacheKey(t *testing.T, profile, sqlText string, maxRows int, binds []any) string {
	t.Helper()
	key, ok := cacheKey(profile, sqlText, maxRows, binds)
	if !ok {
		t.Fatalf("cacheKey refused encodable binds %#v", binds)
	}
	return key
}

// Binds that cannot be encoded canonically must disable caching rather than
// fall back to a binds-blind key.
func TestCacheKeyRefusesUnencodableBinds(t *testing.T) {
	if _, ok := cacheKey("p", "SELECT 1", 10, []any{func() {}}); ok {
		t.Fatal("unencodable binds must report the result as non-cacheable")
	}
}

func TestResultCachePutGetExpiry(t *testing.T) {
	rc := newResultCache()
	key := mustCacheKey(t, "p1", "SELECT 1", 100, nil)
	res := &dbconn.QueryResult{RowCount: 1}
	rc.put(key, res, []string{"C"})

	got, masked, ok := rc.get(key)
	if !ok || got != res || len(masked) != 1 {
		t.Fatalf("cache miss after put: ok=%v", ok)
	}
	// different key → miss
	if _, _, ok := rc.get(mustCacheKey(t, "p1", "SELECT 2", 100, nil)); ok {
		t.Fatal("different SQL must miss")
	}
	if _, _, ok := rc.get(mustCacheKey(t, "p2", "SELECT 1", 100, nil)); ok {
		t.Fatal("different profile must miss")
	}
	// binds are part of the identity
	if _, _, ok := rc.get(mustCacheKey(t, "p1", "SELECT 1", 100, []any{"x"})); ok {
		t.Fatal("bound query must not read the unbound entry")
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

// ---- a fake DB, so result-cache behaviour is observable over real HTTP ----
//
// The standard build links no Oracle driver, so the driver name "godror" is
// free in this test binary. Registering a fake under it gives oracle-typed
// profiles a DB that answers every query with the bind values it received —
// which makes "whose binds produced this row?" assertable end to end. The DSN
// marker keeps every other profile (all postgres-typed) on the real driver.

const fakeBindMarker = "fakebind"

// fakeOracleInstalled is false under -tags oracle, where the real driver owns
// the name.
var fakeOracleInstalled = func() bool {
	for _, n := range sql.Drivers() {
		if n == "godror" {
			return false
		}
	}
	sql.Register("godror", fakeBindDriver{})
	return true
}()

type fakeBindDriver struct{}

func (fakeBindDriver) Open(dsn string) (driver.Conn, error) {
	if !strings.Contains(dsn, fakeBindMarker) {
		return nil, errors.New("fake driver: connect string is not marked for the fake DB")
	}
	return fakeBindConn{}, nil
}

type fakeBindConn struct{}

func (fakeBindConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("fake driver: QueryContext only")
}
func (fakeBindConn) Close() error              { return nil }
func (fakeBindConn) Begin() (driver.Tx, error) { return nil, errors.New("fake driver: read-only") }

func (fakeBindConn) QueryContext(_ context.Context, _ string, args []driver.NamedValue) (driver.Rows, error) {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		parts = append(parts, fmt.Sprint(a.Value))
	}
	return &fakeBindRows{echo: strings.Join(parts, "|")}, nil
}

type fakeBindRows struct {
	echo string
	done bool
}

func (*fakeBindRows) Columns() []string { return []string{"BIND_ECHO"} }
func (*fakeBindRows) Close() error      { return nil }
func (r *fakeBindRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = r.echo
	return nil
}

// newFakeDBServer boots a meta-mode server with an oracle-typed profile wired
// to the fake bind-echoing driver above.
func newFakeDBServer(t *testing.T) (*Server, *http.ServeMux, string) {
	t.Helper()
	s, mux, _, aliceTok := newAuthServer(t)
	profile := `{"id":"fake-db","type":"oracle","connect_string":"` + fakeBindMarker +
		`:1521/FAKE","username":"APP_RO","password_ref":"plain:pw","visibility":"private"}`
	if rec := doReq(t, mux, "POST", "/api/db-profiles", profile, withCookie(aliceTok)); rec.Code != 200 {
		t.Fatalf("create profile: %d %s", rec.Code, rec.Body.String())
	}
	return s, mux, aliceTok
}

const bindCacheSQL = "SELECT CUST_NO FROM TS.TBL1 WHERE CUST_NO = $1"

func bindBody(bind string) string {
	return `{"profile_id":"fake-db","sql":"` + bindCacheSQL + `","max_rows":10,"binds":["` + bind + `"]}`
}

// execEcho runs the sync endpoint and returns the bind values the DB actually
// saw for this response, plus whether it was served from the result cache.
func execEcho(t *testing.T, mux *http.ServeMux, tok, bind string) (string, bool) {
	t.Helper()
	rec := doReq(t, mux, "POST", "/api/query/execute", bindBody(bind), withCookie(tok))
	if rec.Code != 200 {
		t.Fatalf("execute(%s): %d %s", bind, rec.Code, rec.Body.String())
	}
	var out struct {
		Executed bool `json:"executed"`
		Cached   bool `json:"cached"`
		Error    string
		Result   struct {
			Rows []map[string]any `json:"rows"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Executed || len(out.Result.Rows) != 1 {
		t.Fatalf("execute(%s) did not reach the fake DB: %s", bind, rec.Body.String())
	}
	echo, _ := out.Result.Rows[0]["BIND_ECHO"].(string)
	return echo, out.Cached
}

// The 60s result cache keys on profile+SQL+max_rows only, so two requests that
// differ solely in their placeholder values used to share one entry: the second
// caller got the first caller's rows — a cross-tenant leak whenever the
// placeholder is the tenant filter.
func TestResultCacheDoesNotShareEntriesAcrossBinds(t *testing.T) {
	if !fakeOracleInstalled {
		t.Skip("real oracle driver registered (-tags oracle)")
	}
	_, mux, tok := newFakeDBServer(t)

	if echo, cached := execEcho(t, mux, tok, "A"); echo != "A" || cached {
		t.Fatalf("first call: echo=%q cached=%v, want A/false", echo, cached)
	}
	// same SQL, same max_rows, different binds → must execute, not hit A's entry
	if echo, cached := execEcho(t, mux, tok, "B"); echo != "B" || cached {
		t.Fatalf("binds B got binds A's cached rows: echo=%q cached=%v", echo, cached)
	}
	// identical input still caches (the fix narrows the key, it does not
	// disable caching for parameterized queries)
	if echo, cached := execEcho(t, mux, tok, "A"); echo != "A" || !cached {
		t.Fatalf("repeat of A should hit the cache: echo=%q cached=%v", echo, cached)
	}
	if echo, cached := execEcho(t, mux, tok, "B"); echo != "B" || !cached {
		t.Fatalf("repeat of B should hit B's own entry: echo=%q cached=%v", echo, cached)
	}
}

// The async path writes to the cache with fresh=true (read skipped, put kept),
// so its entries are what sync callers later read. Both paths must therefore
// derive the same key from the same input — and different keys from different
// binds.
func TestAsyncAndSyncShareTheCacheKeyPerBinds(t *testing.T) {
	if !fakeOracleInstalled {
		t.Skip("real oracle driver registered (-tags oracle)")
	}
	s, mux, tok := newFakeDBServer(t)

	id := submitJob(t, mux, `{"profile_id":"fake-db","sql":"`+bindCacheSQL+
		`","max_rows":10,"binds":["C"]}`, tok)
	deadline := time.Now().Add(5 * time.Second)
	var job *asyncJob
	for time.Now().Before(deadline) {
		if v, ok := s.asyncJobs.jobView(id); ok && v.Status != "running" {
			job = v
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if job == nil || job.Status != "done" {
		t.Fatalf("async job did not finish against the fake DB: %+v", job)
	}
	if got, _ := job.Result.Rows[0]["BIND_ECHO"].(string); got != "C" {
		t.Fatalf("async job ran with the wrong binds: %q", got)
	}

	// a different bind set must not be served the async result
	if echo, cached := execEcho(t, mux, tok, "D"); echo != "D" || cached {
		t.Fatalf("binds D got the async job's binds-C rows: echo=%q cached=%v", echo, cached)
	}
	// the same bind set must reuse it — same input, same key on both paths
	if echo, cached := execEcho(t, mux, tok, "C"); echo != "C" || !cached {
		t.Fatalf("sync call with the async job's binds should hit its entry: echo=%q cached=%v", echo, cached)
	}
}

func TestResultCacheSetTTLDisables(t *testing.T) {
	rc := newResultCache()
	if !rc.enabled() {
		t.Fatal("default cache should be enabled")
	}
	key := mustCacheKey(t, "p", "SELECT 1", 10, nil)
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
