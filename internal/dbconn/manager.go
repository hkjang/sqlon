package dbconn

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Manager owns one *sql.DB pool per profile (postgres/mysql/mariadb),
// applies pool settings, guards executions, and records audit/history/metrics.
type Manager struct {
	dataDir string
	store   ProfileStore

	mu          sync.Mutex
	pools       map[string]*pooledDB
	brk         map[string]*breaker
	execs       map[string]*execution
	inventories map[string]*inventoryEntry

	histMu  sync.Mutex
	history []HistoryEntry

	metrics Metrics
}

type pooledDB struct {
	db  *sql.DB
	sig string // profile signature; pool is rebuilt when config changes
}

type execution struct {
	ID        string    `json:"execution_id"`
	ProfileID string    `json:"profile_id"`
	SQLHash   string    `json:"sql_hash"`
	User      string    `json:"user,omitempty"`
	StartedAt time.Time `json:"started_at"`
	cancel    context.CancelFunc
}

// breaker is a small circuit breaker (VIP 장애 대응): after `threshold`
// consecutive failures the profile is blocked for `cooldown`.
type breaker struct {
	failures  int
	openUntil time.Time
}

const (
	breakerThreshold = 3
	breakerCooldown  = 30 * time.Second
	historyLimit     = 200
)

type Metrics struct {
	mu           sync.Mutex
	QueryTotal   int64 `json:"db_query_total"`
	QuerySuccess int64 `json:"db_query_success_total"`
	QueryFailure int64 `json:"db_query_failure_total"`
	SlowTotal    int64 `json:"db_query_slow_total"`
	PingFailure  int64 `json:"db_connection_ping_failure_total"`
	ElapsedMsSum int64 `json:"db_query_elapsed_ms_sum"`
}

const slowQueryMs = 5000

func NewManager(dataDir string) *Manager {
	return &Manager{
		dataDir: dataDir,
		store:   FileProfileStore{DataDir: dataDir},
		pools:   map[string]*pooledDB{},
		brk:     map[string]*breaker{},
		execs:   map[string]*execution{},
	}
}

// Available reports whether the DB drivers are compiled in (always true:
// both pgx and go-sql-driver/mysql are pure Go and linked unconditionally).
func (m *Manager) Available() bool { return driverAvailable }

// DriverNote explains how to enable the driver when it is absent.
func (m *Manager) DriverNote() string { return driverSummary() }

// DriverCapabilities lets API/UI distinguish an unsupported edition from a
// failed connection or insufficient database privileges.
func (m *Manager) DriverCapabilities() map[string]bool { return driverCapabilities() }

// Profiles returns the currently visible profiles through the active store.
// It is the single profile discovery path for fleet/collector services.
func (m *Manager) Profiles(ctx context.Context) ([]Profile, error) {
	return m.store.ListAllProfiles(ctx)
}

// Invalidate closes and forgets a profile's pool (after CRUD changes).
func (m *Manager) Invalidate(profileID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.pools[profileID]; ok {
		_ = p.db.Close()
		delete(m.pools, profileID)
	}
	delete(m.brk, profileID)
	m.invalidateInventory(profileID)
}

// Close shuts down every pool (GO-RUN-008 graceful shutdown).
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, p := range m.pools {
		_ = p.db.Close()
		delete(m.pools, id)
	}
	m.invalidateInventory("")
}

func profileSignature(p Profile) string {
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// db returns (building if needed) the pool for a profile.
func (m *Manager) db(p Profile) (*sql.DB, error) {
	sig := profileSignature(p)
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.pools[p.ID]; ok {
		if cur.sig == sig {
			return cur.db, nil
		}
		_ = cur.db.Close()
		delete(m.pools, p.ID)
	}
	password, err := ResolvePassword(p.PasswordRef)
	if err != nil {
		return nil, err
	}
	d, err := DialectFor(p.Type)
	if err != nil {
		return nil, err
	}
	db, err := openDB(d, p, password)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(p.Pool.MaxOpenConns)
	db.SetMaxIdleConns(p.Pool.MaxIdleConns)
	db.SetConnMaxLifetime(durationSeconds(p.Pool.ConnMaxLifetimeSeconds))
	db.SetConnMaxIdleTime(durationSeconds(p.Pool.ConnMaxIdleTimeSeconds))
	m.pools[p.ID] = &pooledDB{db: db, sig: sig}
	return db, nil
}

// ---- circuit breaker ----

func (m *Manager) breakerCheck(profileID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.brk[profileID]; ok && time.Now().Before(b.openUntil) {
		return fmt.Errorf("circuit breaker open for profile %s until %s (%d consecutive failures)",
			profileID, b.openUntil.Format(time.RFC3339), b.failures)
	}
	return nil
}

func (m *Manager) breakerRecord(profileID string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.brk[profileID]
	if b == nil {
		b = &breaker{}
		m.brk[profileID] = b
	}
	if err == nil {
		b.failures = 0
		b.openUntil = time.Time{}
		return
	}
	b.failures++
	if b.failures >= breakerThreshold {
		b.openUntil = time.Now().Add(breakerCooldown)
	}
}

// ---- ping ----

type PingResult struct {
	ProfileID string   `json:"profile_id"`
	OK        bool     `json:"ok"`
	ElapsedMs int64    `json:"elapsed_ms"`
	Error     string   `json:"error,omitempty"`
	ErrorCode string   `json:"error_code,omitempty"`
	Category  string   `json:"category,omitempty"`
	Hint      string   `json:"hint,omitempty"`
	NextSteps []string `json:"next_steps,omitempty"`
}

func (m *Manager) Ping(ctx context.Context, profileID string) PingResult {
	res := PingResult{ProfileID: profileID}
	p, err := m.store.GetProfileByID(ctx, profileID)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	if err := m.breakerCheck(p.ID); err != nil {
		res.Error = err.Error()
		res.ErrorCode = "CIRCUIT_OPEN"
		return res
	}
	start := time.Now()
	db, err := m.db(p)
	if err == nil {
		pctx, cancel := context.WithTimeout(ctx, durationSeconds(p.Policy.ConnectTestTimeoutSeconds))
		defer cancel()
		err = db.PingContext(pctx)
	}
	res.ElapsedMs = time.Since(start).Milliseconds()
	m.breakerRecord(p.ID, err)
	if err != nil {
		m.metrics.mu.Lock()
		m.metrics.PingFailure++
		m.metrics.mu.Unlock()
		res.Error = sanitizeDBError(err)
		res.ErrorCode = dbErrCode(err)
		res.Category, res.Hint, res.NextSteps = connectionDiagnostic(err, p)
		return res
	}
	res.OK = true
	return res
}

// ---- execute ----

type ColumnMeta struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type QueryResult struct {
	ExecutionID string           `json:"execution_id"`
	ProfileID   string           `json:"profile_id"`
	Columns     []ColumnMeta     `json:"columns"`
	Rows        []map[string]any `json:"rows"`
	RowCount    int              `json:"row_count"`
	ElapsedMs   int64            `json:"elapsed_ms"`
	Truncated   bool             `json:"truncated"`
	MaxRows     int              `json:"max_rows"`
	BytesCapped bool             `json:"bytes_capped,omitempty"`
	Hint        string           `json:"hint,omitempty"`
}

type ExecOptions struct {
	MaxRows        int   // 0 → profile default; capped at profile max
	TimeoutSeconds int   // 0 → profile default
	Preview        bool  // force default_max_rows
	Binds          []any // bind variables (GO-SQL-009)
	User           string
	TraceID        string
	// ApprovePlan bypasses the execution-plan approval gate for this one
	// call: set only after a caller (LLM/operator) has reviewed the plan
	// returned in a prior PlanGateError and explicitly chosen to proceed.
	ApprovePlan bool
}

// PlanGateError is returned by Execute when the estimated plan risk meets or
// exceeds the profile's PlanGateRisk threshold and the caller has not
// approved. It carries the analyzed plan so the caller can surface the risk
// factors/suggestions and, if intended, re-issue with ApprovePlan=true.
type PlanGateError struct {
	Plan      *PlanResult
	Threshold string
}

func (e *PlanGateError) Error() string {
	return fmt.Sprintf("execution blocked by plan gate: estimated risk %q >= threshold %q; review risk_factors and either narrow the query or re-run with plan approval",
		e.Plan.Risk, e.Threshold)
}

// PlanCostError is returned when an EXPLAIN estimate exceeds an absolute cost
// ceiling (Policy.MaxPlanCost / MaxPlanRows). Unlike PlanGateError it CANNOT be
// bypassed with ApprovePlan — it is a hard circuit breaker requiring the query
// to be narrowed (or an admin to raise the profile ceiling).
type PlanCostError struct {
	Plan    *PlanResult
	Limit   int64
	Actual  int64
	Measure string // "cost" | "rows"
}

func (e *PlanCostError) Error() string {
	return fmt.Sprintf("execution blocked by cost ceiling: estimated %s %d exceeds the profile cap %d; narrow the query (filters/LIMIT) — this hard cap is not bypassable with plan approval",
		e.Measure, e.Actual, e.Limit)
}

// costCeilingError returns a PlanCostError if the plan's estimated cost or
// cardinality exceeds a configured absolute ceiling, else nil.
func costCeilingError(plan *PlanResult, pol Policy) *PlanCostError {
	if pol.MaxPlanCost > 0 && plan.TotalCost > pol.MaxPlanCost {
		return &PlanCostError{Plan: plan, Limit: pol.MaxPlanCost, Actual: plan.TotalCost, Measure: "cost"}
	}
	if pol.MaxPlanRows > 0 && plan.MaxCardinality > pol.MaxPlanRows {
		return &PlanCostError{Plan: plan, Limit: pol.MaxPlanRows, Actual: plan.MaxCardinality, Measure: "rows"}
	}
	return nil
}

// Execute validates, bounds, runs, and audits one read-only query.
func (m *Manager) Execute(ctx context.Context, profileID, sqlText string, opts ExecOptions) (*QueryResult, error) {
	p, err := m.store.GetProfileByID(ctx, profileID)
	if err != nil {
		return nil, err
	}
	d, err := DialectFor(p.Type)
	if err != nil {
		return nil, err
	}
	if err := ValidateReadOnly(d, sqlText, deniedForProfile(d, p)); err != nil {
		m.audit(p.ID, sqlText, opts, nil, 0, err)
		return nil, err
	}
	if err := EnforceOracleLicense(p, sqlText); err != nil {
		m.audit(p.ID, sqlText, opts, nil, 0, err)
		return nil, err
	}
	if err := m.breakerCheck(p.ID); err != nil {
		return nil, err
	}
	// execution-plan approval gate: run EXPLAIN first and refuse high-risk
	// plans on the operational DB unless the caller has approved. Skipped
	// when disabled by policy, when the caller approved, or for previews
	// (which are already row-capped to default_max_rows).
	costGuard := p.Policy.MaxPlanCost > 0 || p.Policy.MaxPlanRows > 0
	if (p.Policy.planGateEnabled() || costGuard) && !opts.Preview {
		plan, perr := m.ExplainPlan(ctx, profileID, sqlText)
		if perr == nil && plan != nil {
			// hard cost ceiling first — not bypassable with ApprovePlan.
			if ce := costCeilingError(plan, p.Policy); ce != nil {
				m.audit(p.ID, sqlText, opts, nil, 0, ce)
				return nil, ce
			}
			// reviewable risk gate — bypassable after explicit plan approval.
			if p.Policy.planGateEnabled() && !opts.ApprovePlan && riskRank(plan.Risk) >= riskRank(p.Policy.PlanGateRisk) {
				gateErr := &PlanGateError{Plan: plan, Threshold: p.Policy.PlanGateRisk}
				m.audit(p.ID, sqlText, opts, nil, 0, gateErr)
				return nil, gateErr
			}
		}
		// a failed EXPLAIN (perr != nil) does not block execution — the query
		// itself will surface the real error with proper classification.
	}
	maxRows := opts.MaxRows
	if opts.Preview || maxRows <= 0 {
		maxRows = p.Policy.DefaultMaxRows
	}
	if maxRows > p.Policy.MaxRows {
		maxRows = p.Policy.MaxRows
	}
	timeout := durationSeconds(p.Policy.QueryTimeoutSeconds)
	if opts.TimeoutSeconds > 0 && opts.TimeoutSeconds < p.Policy.QueryTimeoutSeconds {
		timeout = durationSeconds(opts.TimeoutSeconds)
	}

	db, err := m.db(p)
	if err != nil {
		m.breakerRecord(p.ID, err)
		m.audit(p.ID, sqlText, opts, nil, 0, err)
		return nil, err
	}

	qctx, cancel := context.WithTimeout(ctx, timeout)
	exec := &execution{
		ID:        newExecutionID(),
		ProfileID: p.ID,
		SQLHash:   sqlHash(sqlText),
		User:      opts.User,
		StartedAt: time.Now(),
		cancel:    cancel,
	}
	m.registerExec(exec)
	defer func() {
		cancel()
		m.unregisterExec(exec.ID)
	}()

	limited := d.WrapLimit(sqlText, maxRows+1)
	start := time.Now()
	rows, err := db.QueryContext(qctx, limited, opts.Binds...)
	if err != nil {
		m.breakerRecord(p.ID, err)
		m.audit(p.ID, sqlText, opts, nil, time.Since(start).Milliseconds(), err)
		return nil, wrapExecError(err, qctx)
	}
	defer rows.Close()

	result, err := scanRows(rows, maxRows, p.Policy.MaxResponseBytes)
	if err != nil {
		m.breakerRecord(p.ID, err)
		m.audit(p.ID, sqlText, opts, nil, time.Since(start).Milliseconds(), err)
		return nil, wrapExecError(err, qctx)
	}
	result.ExecutionID = exec.ID
	result.ProfileID = p.ID
	result.MaxRows = maxRows
	result.ElapsedMs = time.Since(start).Milliseconds()
	if result.RowCount == 0 {
		result.Hint = "결과가 0행입니다. 기간 조건이 과도하게 좁거나 코드값 필터가 실제 값과 다를 수 있습니다 — resolve_time과 find_filter_columns로 조건을 재확인하세요."
	}

	m.breakerRecord(p.ID, nil)
	m.audit(p.ID, sqlText, opts, result, result.ElapsedMs, nil)
	return result, nil
}

// CountRows runs SELECT COUNT(*) over the (guarded) query — used by the
// execution-based golden-set evaluation for row-count sanity checks.
func (m *Manager) CountRows(ctx context.Context, profileID, sqlText string) (int64, error) {
	p, err := m.store.GetProfileByID(ctx, profileID)
	if err != nil {
		return 0, err
	}
	d, err := DialectFor(p.Type)
	if err != nil {
		return 0, err
	}
	if err := ValidateReadOnly(d, sqlText, deniedForProfile(d, p)); err != nil {
		return 0, err
	}
	if err := EnforceOracleLicense(p, sqlText); err != nil {
		return 0, err
	}
	if err := m.breakerCheck(p.ID); err != nil {
		return 0, err
	}
	db, err := m.db(p)
	if err != nil {
		m.breakerRecord(p.ID, err)
		return 0, err
	}
	qctx, cancel := context.WithTimeout(ctx, durationSeconds(p.Policy.QueryTimeoutSeconds))
	defer cancel()
	var n int64
	err = db.QueryRowContext(qctx, d.CountWrap(sqlText)).Scan(&n)
	m.breakerRecord(p.ID, err)
	if err != nil {
		return 0, wrapExecError(err, qctx)
	}
	return n, nil
}

// Metadata runs the query with ROWNUM <= 0 to fetch column metadata only.
func (m *Manager) Metadata(ctx context.Context, profileID, sqlText string) ([]ColumnMeta, error) {
	p, err := m.store.GetProfileByID(ctx, profileID)
	if err != nil {
		return nil, err
	}
	d, err := DialectFor(p.Type)
	if err != nil {
		return nil, err
	}
	if err := ValidateReadOnly(d, sqlText, deniedForProfile(d, p)); err != nil {
		return nil, err
	}
	if err := EnforceOracleLicense(p, sqlText); err != nil {
		return nil, err
	}
	if err := m.breakerCheck(p.ID); err != nil {
		return nil, err
	}
	db, err := m.db(p)
	if err != nil {
		m.breakerRecord(p.ID, err)
		return nil, err
	}
	qctx, cancel := context.WithTimeout(ctx, durationSeconds(p.Policy.ConnectTestTimeoutSeconds))
	defer cancel()
	rows, err := db.QueryContext(qctx, d.WrapLimit(sqlText, 0))
	if err != nil {
		m.breakerRecord(p.ID, err)
		return nil, wrapExecError(err, qctx)
	}
	defer rows.Close()
	cols, err := columnMetas(rows)
	if err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	m.breakerRecord(p.ID, nil)
	return cols, nil
}

// scanRows converts the result set with per-type normalization
// and enforces row/byte caps.
func scanRows(rows *sql.Rows, maxRows int, maxBytes int64) (*QueryResult, error) {
	cols, err := columnMetas(rows)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	result := &QueryResult{Columns: cols, Rows: make([]map[string]any, 0, min(maxRows, 256))}
	var approxBytes int64
	for rows.Next() {
		values := make([]any, len(names))
		pointers := make([]any, len(names))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, err
		}
		if result.RowCount >= maxRows {
			result.Truncated = true
			break
		}
		row := make(map[string]any, len(names))
		for i, col := range names {
			v := normalizeValue(values[i])
			row[col] = v
			approxBytes += int64(len(col)) + approxSize(v) + 8
		}
		result.Rows = append(result.Rows, row)
		result.RowCount++
		if maxBytes > 0 && approxBytes > maxBytes {
			result.Truncated = true
			result.BytesCapped = true
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func columnMetas(rows *sql.Rows) ([]ColumnMeta, error) {
	names, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	types, err := rows.ColumnTypes()
	if err != nil {
		return nil, err
	}
	out := make([]ColumnMeta, 0, len(names))
	for i, n := range names {
		t := ""
		if i < len(types) {
			t = types[i].DatabaseTypeName()
		}
		out = append(out, ColumnMeta{Name: n, Type: t})
	}
	return out, nil
}

// normalizeValue makes NUMBER/VARCHAR2/DATE/TIMESTAMP/CLOB values
// JSON-friendly: []byte (mysql text/blob) becomes string, time.Time becomes RFC3339.
func normalizeValue(v any) any {
	switch value := v.(type) {
	case nil:
		return nil
	case []byte:
		return string(value)
	case time.Time:
		return value.Format(time.RFC3339)
	case fmt.Stringer:
		return value.String()
	default:
		return value
	}
}

func approxSize(v any) int64 {
	switch value := v.(type) {
	case nil:
		return 4
	case string:
		return int64(len(value))
	default:
		return 16
	}
}

// ---- cancel / running (GO API /api/query/cancel) ----

func (m *Manager) registerExec(e *execution) {
	m.mu.Lock()
	m.execs[e.ID] = e
	m.mu.Unlock()
}

func (m *Manager) unregisterExec(id string) {
	m.mu.Lock()
	delete(m.execs, id)
	m.mu.Unlock()
}

// Cancel aborts a running execution by id (trusted caller).
func (m *Manager) Cancel(id string) error { return m.CancelAs(id, "", true) }

// CancelAs aborts an execution enforcing ownership: non-admin requesters may
// only cancel their own executions.
func (m *Manager) CancelAs(id, requester string, admin bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.execs[id]
	if !ok {
		return fmt.Errorf("execution not found or already finished: %s", id)
	}
	if !admin && e.User != requester {
		return fmt.Errorf("execution %s belongs to another user", id)
	}
	e.cancel()
	return nil
}

// Running lists in-flight executions.
func (m *Manager) Running() []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []map[string]any{}
	for _, e := range m.execs {
		out = append(out, map[string]any{
			"execution_id": e.ID,
			"profile_id":   e.ProfileID,
			"sql_hash":     e.SQLHash,
			"user":         e.User,
			"started_at":   e.StartedAt.Format(time.RFC3339),
			"elapsed_ms":   time.Since(e.StartedAt).Milliseconds(),
		})
	}
	return out
}

// ---- history & audit ----

type HistoryEntry struct {
	TraceID     string `json:"trace_id,omitempty"`
	ExecutionID string `json:"execution_id,omitempty"`
	User        string `json:"user_id,omitempty"`
	ProfileID   string `json:"db_profile_id"`
	SQLHash     string `json:"sql_hash"`
	SQLText     string `json:"sql_text"`
	StartedAt   string `json:"started_at"`
	ElapsedMs   int64  `json:"elapsed_ms"`
	RowCount    int    `json:"row_count"`
	Truncated   bool   `json:"truncated"`
	Success     bool   `json:"success"`
	ErrorCode   string `json:"error_code,omitempty"`
	ErrorMsg    string `json:"error_message,omitempty"`
}

func (m *Manager) audit(profileID, sqlText string, opts ExecOptions, res *QueryResult, elapsedMs int64, execErr error) {
	entry := HistoryEntry{
		TraceID:   opts.TraceID,
		User:      opts.User,
		ProfileID: profileID,
		SQLHash:   sqlHash(sqlText),
		SQLText:   truncateStr(sqlText, 4000),
		StartedAt: time.Now().Add(-time.Duration(elapsedMs) * time.Millisecond).Format(time.RFC3339),
		ElapsedMs: elapsedMs,
		Success:   execErr == nil,
	}
	if res != nil {
		entry.ExecutionID = res.ExecutionID
		entry.RowCount = res.RowCount
		entry.Truncated = res.Truncated
	}
	if execErr != nil {
		entry.ErrorCode = dbErrCode(execErr)
		entry.ErrorMsg = sanitizeDBError(execErr)
	}

	m.metrics.mu.Lock()
	m.metrics.QueryTotal++
	m.metrics.ElapsedMsSum += elapsedMs
	if execErr == nil {
		m.metrics.QuerySuccess++
	} else {
		m.metrics.QueryFailure++
	}
	if elapsedMs >= slowQueryMs {
		m.metrics.SlowTotal++
	}
	m.metrics.mu.Unlock()

	m.histMu.Lock()
	m.history = append(m.history, entry)
	if len(m.history) > historyLimit {
		m.history = m.history[len(m.history)-historyLimit:]
	}
	m.histMu.Unlock()

	dir := filepath.Join(m.dataDir, "audit")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	b, err := json.Marshal(map[string]any{"ts": time.Now().Format(time.RFC3339Nano), "tool": "db:execute", "entry": entry})
	if err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "query-"+time.Now().Format("20060102")+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// History returns recent executions, newest first.
func (m *Manager) History(limit int) []HistoryEntry {
	if limit <= 0 || limit > historyLimit {
		limit = 50
	}
	m.histMu.Lock()
	defer m.histMu.Unlock()
	n := len(m.history)
	out := make([]HistoryEntry, 0, min(limit, n))
	for i := n - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, m.history[i])
	}
	return out
}

// ---- metrics ----

// Snapshot returns counters plus per-profile pool stats (db.Stats()).
func (m *Manager) Snapshot() map[string]any {
	m.metrics.mu.Lock()
	counters := map[string]int64{
		"db_query_total":                   m.metrics.QueryTotal,
		"db_query_success_total":           m.metrics.QuerySuccess,
		"db_query_failure_total":           m.metrics.QueryFailure,
		"db_query_slow_total":              m.metrics.SlowTotal,
		"db_connection_ping_failure_total": m.metrics.PingFailure,
		"db_query_elapsed_ms_sum":          m.metrics.ElapsedMsSum,
	}
	m.metrics.mu.Unlock()

	pools := map[string]any{}
	m.mu.Lock()
	for id, p := range m.pools {
		st := p.db.Stats()
		pools[id] = map[string]any{
			"db_pool_open_connections":   st.OpenConnections,
			"db_pool_in_use_connections": st.InUse,
			"db_pool_idle_connections":   st.Idle,
			"db_pool_wait_count":         st.WaitCount,
			"db_pool_wait_duration_ms":   st.WaitDuration.Milliseconds(),
			"max_open_connections":       st.MaxOpenConnections,
		}
	}
	breakers := map[string]any{}
	for id, b := range m.brk {
		if b.failures > 0 || time.Now().Before(b.openUntil) {
			breakers[id] = map[string]any{"consecutive_failures": b.failures, "open_until": b.openUntil.Format(time.RFC3339)}
		}
	}
	m.mu.Unlock()

	return map[string]any{
		"driver_available": driverAvailable,
		"drivers":          driverCapabilities(),
		"counters":         counters,
		"pools":            pools,
		"breakers":         breakers,
	}
}

// PrometheusText renders the snapshot in Prometheus exposition format.
func (m *Manager) PrometheusText() string {
	snap := m.Snapshot()
	var b strings.Builder
	for k, v := range snap["counters"].(map[string]int64) {
		fmt.Fprintf(&b, "# TYPE %s counter\n%s %d\n", k, k, v)
	}
	for id, raw := range snap["pools"].(map[string]any) {
		stats := raw.(map[string]any)
		for k, v := range stats {
			if strings.HasPrefix(k, "db_pool_") {
				fmt.Fprintf(&b, "%s{profile=%q} %v\n", k, id, v)
			}
		}
	}
	return b.String()
}

// ---- helpers ----

// deniedForProfile merges dialect-specific denied keywords with the profile's
// own policy extensions.
func deniedForProfile(d Dialect, p Profile) []string {
	out := append([]string{}, d.DeniedExtras()...)
	return append(out, p.Policy.DeniedKeywords...)
}

func wrapExecError(err error, ctx context.Context) error {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("query timeout exceeded: %w", err)
	case errors.Is(ctx.Err(), context.Canceled):
		return fmt.Errorf("query canceled: %w", err)
	default:
		return err
	}
}

func sqlHash(sqlText string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(sqlText)))
	return hex.EncodeToString(sum[:8])
}

func newExecutionID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("exec-%d", time.Now().UnixNano())
	}
	return "exec-" + hex.EncodeToString(b[:])
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// SetProfileStore swaps the profile source (e.g. Postgres meta DB) and drops
// all cached pools so stale definitions are not reused.
func (m *Manager) SetProfileStore(store ProfileStore) {
	m.mu.Lock()
	for id, p := range m.pools {
		_ = p.db.Close()
		delete(m.pools, id)
	}
	m.invalidateInventory("")
	m.store = store
	m.mu.Unlock()
}
