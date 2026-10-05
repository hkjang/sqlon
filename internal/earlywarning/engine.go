package earlywarning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"sqlon/internal/change"
	"sqlon/internal/collector"
	"sqlon/internal/dbconn"
	"sqlon/internal/metasync"
	"sqlon/internal/observability"
	"sqlon/internal/storage"
)

type ProfileSource interface {
	Profiles(context.Context) ([]dbconn.Profile, error)
}

type MaintenanceChecker interface {
	Maintenance(context.Context, dbconn.Profile) observability.Response[observability.MaintenanceData]
}

type SchemaCollector interface {
	Collect(context.Context, metasync.CollectRequest) (*metasync.RawSnapshot, error)
}

type PlanLister interface {
	List() []change.Plan
}

// HistoryScanner streams stored collector snapshots, oldest first. It seeds
// the series once so forecasts start from existing history.
type HistoryScanner interface {
	Scan(context.Context, storage.Query, func(storage.Record) error) error
}

type Config struct {
	// Dir holds the engine's state (alerts, series, schema baselines).
	Dir string
	// MinNotifySeverity is the lowest severity sent to the notifier.
	MinNotifySeverity string
	// RenotifyInterval re-sends a still-firing, unacknowledged alert.
	RenotifyInterval time.Duration
	// MaintenanceEvery and SchemaEvery space out the heavier checks; the
	// capacity and collection checks run on every collection cycle.
	MaintenanceEvery time.Duration
	SchemaEvery      time.Duration
	// CollectionDownAfter consecutive failed collections raise an alert.
	CollectionDownAfter int
	// EventTTL closes an unacknowledged event (a schema change).
	EventTTL         time.Duration
	HistoryRetention time.Duration
	MaxHistory       int
	Concurrency      int
}

func (c Config) withDefaults() Config {
	if Rank(c.MinNotifySeverity) == 0 {
		c.MinNotifySeverity = SevWarning
	}
	if c.RenotifyInterval == 0 {
		c.RenotifyInterval = 6 * time.Hour
	}
	if c.MaintenanceEvery <= 0 {
		c.MaintenanceEvery = 5 * time.Minute
	}
	if c.SchemaEvery == 0 {
		c.SchemaEvery = 15 * time.Minute
	}
	if c.CollectionDownAfter <= 0 {
		c.CollectionDownAfter = 3
	}
	if c.EventTTL <= 0 {
		c.EventTTL = 24 * time.Hour
	}
	if c.HistoryRetention <= 0 {
		c.HistoryRetention = 30 * 24 * time.Hour
	}
	if c.MaxHistory <= 0 {
		c.MaxHistory = 2000
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 4
	}
	return c
}

// DeliveryStatus is the notifier's health, shown on the console so a broken
// webhook is visible instead of silently swallowing every warning.
type DeliveryStatus struct {
	Configured          bool       `json:"configured"`
	Target              string     `json:"target,omitempty"`
	LastAttemptAt       *time.Time `json:"last_attempt_at,omitempty"`
	LastSuccessAt       *time.Time `json:"last_success_at,omitempty"`
	LastError           string     `json:"last_error,omitempty"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	NextRetryAt         *time.Time `json:"next_retry_at,omitempty"`
	Delivered           int64      `json:"delivered"`
	Failed              int64      `json:"failed"`
}

type profileMeta struct {
	Failures          int       `json:"consecutive_failures"`
	LastStatus        string    `json:"last_status,omitempty"`
	LastError         string    `json:"last_error,omitempty"`
	LastCollectedAt   time.Time `json:"last_collected_at,omitempty"`
	LastMaintenanceAt time.Time `json:"last_maintenance_at,omitempty"`
	MaintenanceStatus string    `json:"maintenance_status,omitempty"`
	LastSchemaAt      time.Time `json:"last_schema_at,omitempty"`
	SchemaStatus      string    `json:"schema_status,omitempty"`
	SchemaBaselineAt  time.Time `json:"schema_baseline_at,omitempty"`
	SchemaTables      int       `json:"schema_tables,omitempty"`
}

type persisted struct {
	Version     int                     `json:"version"`
	book                                // alerts
	Profiles    map[string]*profileMeta `json:"profiles"`
	Delivery    DeliveryStatus          `json:"delivery"`
	LastCycleAt time.Time               `json:"last_cycle_at,omitempty"`
	Cycles      int64                   `json:"cycles"`
}

// Engine evaluates collection cycles into alerts. All exported methods are
// safe for concurrent use; Evaluate calls are serialized.
type Engine struct {
	cfg         Config
	Profiles    ProfileSource
	Maintenance MaintenanceChecker
	Schema      SchemaCollector
	Plans       PlanLister
	History     HistoryScanner
	Notifier    Notifier
	Now         func() time.Time
	Logf        func(string, ...any)

	evalMu    sync.Mutex
	mu        sync.Mutex
	st        *persisted
	series    map[string]*profileSeries
	forecasts map[string][]Forecast
	bridged   map[string][]collector.Alert
	baseMu    sync.Mutex
	baselines map[string]*metasync.RawSnapshot
}

// New loads persisted state from cfg.Dir. A corrupt state file is moved
// aside rather than blocking startup.
func New(cfg Config) *Engine {
	e := &Engine{cfg: cfg.withDefaults(), Now: time.Now, Logf: log.Printf,
		series: map[string]*profileSeries{}, forecasts: map[string][]Forecast{}, bridged: map[string][]collector.Alert{}, baselines: map[string]*metasync.RawSnapshot{}}
	e.st = &persisted{Version: 1, Profiles: map[string]*profileMeta{}}
	if e.cfg.Dir != "" {
		path := filepath.Join(e.cfg.Dir, "state.json")
		if b, err := os.ReadFile(path); err == nil {
			var st persisted
			if json.Unmarshal(b, &st) == nil {
				if st.Profiles == nil {
					st.Profiles = map[string]*profileMeta{}
				}
				e.st = &st
			} else {
				_ = os.Rename(path, path+".corrupt-"+time.Now().UTC().Format("20060102T150405"))
				e.logf("early-warning: unreadable state moved aside: %s", path)
			}
		}
	}
	return e
}

func (e *Engine) Config() Config { return e.cfg }

func (e *Engine) now() time.Time {
	if e.Now == nil {
		return time.Now().UTC()
	}
	return e.Now().UTC()
}

func (e *Engine) logf(format string, args ...any) {
	if e.Logf != nil {
		e.Logf(format, args...)
	}
}

func (e *Engine) meta(profileID string) *profileMeta {
	m := e.st.Profiles[profileID]
	if m == nil {
		m = &profileMeta{}
		e.st.Profiles[profileID] = m
	}
	return m
}

// Ingest receives alerts raised by the collector's own alert engine so they
// flow through the same lifecycle and webhook. Safe from collector workers.
func (e *Engine) Ingest(profileID string, alerts []collector.Alert) {
	if e == nil || len(alerts) == 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.bridged[profileID] = append(e.bridged[profileID], alerts...)
}

type EvaluateOptions struct {
	// Force runs the maintenance and schema checks regardless of cadence.
	Force bool
}

// CycleReport summarizes one evaluation.
type CycleReport struct {
	At            time.Time `json:"at"`
	Profiles      int       `json:"profiles"`
	Conditions    int       `json:"conditions"`
	Firing        int       `json:"firing"`
	Notifications int       `json:"notifications"`
	Delivered     bool      `json:"delivered"`
	DeliveryError string    `json:"delivery_error,omitempty"`
	Error         string    `json:"error,omitempty"`
}

type profileWork struct {
	p          dbconn.Profile
	ran        map[string]bool
	conds      []Condition
	doMaint    bool
	doSchema   bool
	tableBytes map[string]float64
	maintState string
	schemaNote string
	newBase    *metasync.RawSnapshot
}

// Evaluate turns one collection batch into alert state and notifications.
func (e *Engine) Evaluate(ctx context.Context, batch collector.BatchResult, opts EvaluateOptions) CycleReport {
	e.evalMu.Lock()
	defer e.evalMu.Unlock()
	now := e.now()
	report := CycleReport{At: now}
	if e.Profiles == nil {
		report.Error = "profile source not configured"
		return report
	}
	profiles, err := e.Profiles.Profiles(ctx)
	if err != nil {
		report.Error = "프로파일 조회 실패: " + err.Error()
		e.logf("early-warning: %s", report.Error)
		return report
	}
	byID := map[string]dbconn.Profile{}
	for _, p := range profiles {
		byID[p.ID] = dbconn.ApplyDefaults(p)
	}

	var works []*profileWork
	for _, result := range batch.Results {
		p, ok := byID[result.Snapshot.ProfileID]
		if !ok {
			continue
		}
		e.ensureSeries(ctx, p.ID, now)
		works = append(works, &profileWork{p: p, ran: map[string]bool{}})
	}
	resultOf := map[string]collector.ProfileResult{}
	for _, r := range batch.Results {
		resultOf[r.Snapshot.ProfileID] = r
	}

	// Phase A — fast, in memory: series, forecasts, standing conditions.
	e.mu.Lock()
	for _, w := range works {
		pid := w.p.ID
		result := resultOf[pid]
		m := e.meta(pid)
		collected := result.Status == "ok" || result.Status == "partial"
		w.ran[CheckCollection] = true
		m.LastStatus = result.Status
		if collected {
			m.Failures, m.LastError, m.LastCollectedAt = 0, "", result.CollectedAt
		} else {
			m.Failures++
			m.LastError = result.Error
		}
		w.conds = append(w.conds, collectionConditions(w.p, result, m.Failures, e.cfg.CollectionDownAfter)...)
		if collected && len(result.Snapshot.Capacity) > 0 {
			ps := e.series[pid]
			ps.record(result.Snapshot.CollectedAt, result.Snapshot.Capacity)
			fcs := buildForecasts(w.p, result.Snapshot, ps, now)
			e.forecasts[pid] = fcs
			w.conds = append(w.conds, capacityConditions(w.p, result.Snapshot, fcs)...)
			w.conds = append(w.conds, tableConditions(result.Snapshot, ps, now)...)
			w.ran[CheckCapacity], w.ran[CheckTables] = true, true
			w.tableBytes = map[string]float64{}
			for _, c := range result.Snapshot.Capacity {
				if c.Scope == "table" {
					w.tableBytes[c.Name] = c.UsedBytes
				}
			}
		}
		if bridged := e.bridged[pid]; len(bridged) > 0 {
			w.conds = append(w.conds, bridgedConditions(bridged)...)
			delete(e.bridged, pid)
		}
		if collected && e.Maintenance != nil && (opts.Force || now.Sub(m.LastMaintenanceAt) >= e.cfg.MaintenanceEvery) {
			w.doMaint, m.LastMaintenanceAt = true, now
		}
		if collected && e.Schema != nil && e.cfg.SchemaEvery > 0 && (opts.Force || now.Sub(m.LastSchemaAt) >= e.cfg.SchemaEvery) {
			w.doSchema, m.LastSchemaAt = true, now
		}
	}
	e.mu.Unlock()

	// Phase B — database round trips, a few profiles at a time.
	var plans []change.Plan
	if e.Plans != nil {
		plans = e.Plans.List()
	}
	sem := make(chan struct{}, e.cfg.Concurrency)
	var wg sync.WaitGroup
	for _, w := range works {
		if !w.doMaint && !w.doSchema {
			continue
		}
		wg.Add(1)
		go func(w *profileWork) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if w.doMaint {
				e.runMaintenance(ctx, w)
			}
			if w.doSchema {
				e.runSchema(ctx, w, plans, now)
			}
		}(w)
	}
	wg.Wait()

	// Phase C — reconcile and work out what to say.
	e.mu.Lock()
	for _, w := range works {
		e.st.reconcile(w.p.ID, w.ran, w.conds, now)
		report.Conditions += len(w.conds)
		m := e.meta(w.p.ID)
		if w.doMaint {
			m.MaintenanceStatus = w.maintState
		}
		if w.doSchema {
			m.SchemaStatus = w.schemaNote
			if w.newBase != nil {
				m.SchemaBaselineAt, m.SchemaTables = w.newBase.CollectedAt, len(w.newBase.Tables)
			}
		}
	}
	for pid := range e.st.Profiles {
		if _, ok := byID[pid]; !ok {
			e.st.resolveProfile(pid, now)
			delete(e.st.Profiles, pid)
			delete(e.series, pid)
			delete(e.forecasts, pid)
			e.removeProfileFiles(pid)
		}
	}
	e.st.expire(now, e.cfg.EventTTL, e.cfg.HistoryRetention, e.cfg.MaxHistory)
	var notes []Notification
	if e.Notifier != nil && (e.st.Delivery.NextRetryAt == nil || !now.Before(*e.st.Delivery.NextRetryAt)) {
		notes = e.st.pending(now, e.cfg.MinNotifySeverity, e.cfg.RenotifyInterval)
	}
	names := map[string]string{}
	for _, p := range byID {
		names[p.ID] = p.Name
	}
	e.st.LastCycleAt = now
	e.st.Cycles++
	report.Profiles = len(works)
	e.mu.Unlock()

	// Phase D — deliver.
	if len(notes) > 0 {
		report.Notifications = len(notes)
		dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := e.Notifier.Notify(dctx, notes, names)
		cancel()
		e.mu.Lock()
		at := e.now()
		d := &e.st.Delivery
		d.LastAttemptAt = &at
		if err != nil {
			d.Failed++
			d.ConsecutiveFailures++
			d.LastError = err.Error()
			backoff := time.Duration(1<<min(d.ConsecutiveFailures-1, 5)) * time.Minute // 1,2,4…32 min
			next := at.Add(backoff)
			d.NextRetryAt = &next
			report.DeliveryError = err.Error()
			e.logf("early-warning: notification delivery failed (%d pending, retry after %s): %v", len(notes), backoff, err)
		} else {
			d.Delivered += int64(len(notes))
			d.ConsecutiveFailures, d.LastError, d.NextRetryAt = 0, "", nil
			d.LastSuccessAt = &at
			e.st.markDelivered(notes, at)
			report.Delivered = true
		}
		e.mu.Unlock()
	}

	// Phase E — persist. Baselines are written after the state that records
	// the change, so a crash in between re-detects rather than loses it.
	e.mu.Lock()
	for _, a := range e.st.Alerts {
		if a.State == StateFiring {
			report.Firing++
		}
	}
	e.st.Delivery.Configured = e.Notifier != nil
	if e.Notifier != nil {
		e.st.Delivery.Target = e.Notifier.Target()
	}
	saveErr := e.saveStateLocked()
	e.saveSeriesLocked(now, false)
	e.mu.Unlock()
	if saveErr != nil {
		e.logf("early-warning: save state: %v", saveErr)
	}
	for _, w := range works {
		if w.newBase != nil {
			if err := e.storeBaseline(w.p.ID, w.newBase); err != nil {
				e.logf("early-warning: save schema baseline %s: %v", w.p.ID, err)
			}
		}
	}
	return report
}

func (e *Engine) runMaintenance(ctx context.Context, w *profileWork) {
	resp := e.Maintenance.Maintenance(ctx, w.p)
	switch resp.Status {
	case "ok", "partial", "warning", "critical":
		w.ran[CheckMaintenance] = true
		w.conds = append(w.conds, maintenanceConditions(resp.Data.Findings)...)
		w.maintState = fmt.Sprintf("%s (점검 %d종, 발견 %d건)", resp.Status, resp.Data.Checks, len(resp.Data.Findings))
	default:
		cause := ""
		if len(resp.Warnings) > 0 {
			cause = resp.Warnings[0]
		} else if len(resp.Limitations) > 0 {
			cause = resp.Limitations[0]
		}
		w.maintState = resp.Status + ": " + cause
	}
}

func (e *Engine) runSchema(ctx context.Context, w *profileWork, plans []change.Plan, now time.Time) {
	snap, err := e.Schema.Collect(ctx, metasync.CollectRequest{SourceID: w.p.ID, IncludeViews: true})
	switch {
	case err != nil:
		w.schemaNote = "수집 실패: " + err.Error()
		return
	case snap == nil || snap.Status != "success":
		// A partial snapshot is missing tables; diffing it would report
		// them as dropped.
		w.schemaNote = "부분 수집이라 비교를 건너뜀"
		if snap != nil && len(snap.ErrorSummary) > 0 {
			w.schemaNote += ": " + snap.ErrorSummary[0]
		}
		return
	}
	snap.CollectedAt = now
	if snap.SnapshotID == "" {
		snap.SnapshotID = "ew-" + now.Format("20060102T150405Z")
	}
	base, err := e.loadBaseline(w.p.ID)
	if err != nil {
		w.schemaNote = "기준선 읽기 실패: " + err.Error()
		return
	}
	w.ran[CheckSchema] = true
	switch {
	case base == nil:
		w.newBase = snap
		w.schemaNote = fmt.Sprintf("기준선 수립 (테이블 %d개)", len(snap.Tables))
	case base.SchemaHash == snap.SchemaHash:
		w.schemaNote = fmt.Sprintf("변경 없음 (테이블 %d개)", len(snap.Tables))
	default:
		w.conds = append(w.conds, schemaConditions(w.p, base, snap, plans, w.tableBytes)...)
		w.newBase = snap
		w.schemaNote = fmt.Sprintf("변경 감지 (테이블 %d개)", len(snap.Tables))
	}
}

// Ack acknowledges a firing alert.
func (e *Engine) Ack(id, actor, note string) (Alert, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	a, err := e.st.ack(id, actor, note, e.now())
	if err == nil {
		if saveErr := e.saveStateLocked(); saveErr != nil {
			e.logf("early-warning: save state: %v", saveErr)
		}
	}
	return a, err
}

// IsNotFound reports whether err means the alert id does not exist.
func IsNotFound(err error) bool { return errors.Is(err, errAlertNotFound) }

// Alert returns one alert by id.
func (e *Engine) Alert(id string) (Alert, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, a := range e.st.Alerts {
		if a.ID == id {
			return *a, true
		}
	}
	return Alert{}, false
}

// ---- persistence ----

var unsafeID = regexp.MustCompile(`[^A-Za-z0-9._-]`)

func (e *Engine) profilePath(kind, profileID string) string {
	return filepath.Join(e.cfg.Dir, kind, unsafeID.ReplaceAllString(profileID, "_")+".json")
}

func writeJSONAtomic(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (e *Engine) saveStateLocked() error {
	if e.cfg.Dir == "" {
		return nil
	}
	return writeJSONAtomic(filepath.Join(e.cfg.Dir, "state.json"), e.st)
}

// saveSeriesLocked writes dirty series at most every ten minutes (always
// when force): losing a few samples to a crash costs nothing, rewriting a
// week of history every minute would.
func (e *Engine) saveSeriesLocked(now time.Time, force bool) {
	if e.cfg.Dir == "" {
		return
	}
	for pid, ps := range e.series {
		if !ps.dirty || (!force && now.Sub(ps.savedAt) < 10*time.Minute) {
			continue
		}
		if err := writeJSONAtomic(e.profilePath("series", pid), ps); err != nil {
			e.logf("early-warning: save series %s: %v", pid, err)
			continue
		}
		ps.dirty, ps.savedAt = false, now
	}
}

// Flush writes everything immediately (shutdown, tests).
func (e *Engine) Flush() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.saveSeriesLocked(e.now(), true)
	return e.saveStateLocked()
}

func (e *Engine) removeProfileFiles(pid string) {
	if e.cfg.Dir == "" {
		return
	}
	_ = os.Remove(e.profilePath("series", pid))
	_ = os.Remove(e.profilePath("schema", pid))
	e.baseMu.Lock()
	delete(e.baselines, pid)
	e.baseMu.Unlock()
}

// ensureSeries loads a profile's series from disk or, when there is no
// series file yet (first start, or a restart before the first periodic
// save), rebuilds it from the collector's stored snapshots so forecasts do
// not start from zero.
func (e *Engine) ensureSeries(ctx context.Context, pid string, now time.Time) {
	e.mu.Lock()
	if _, ok := e.series[pid]; ok {
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()

	ps := newProfileSeries()
	loaded := false
	if e.cfg.Dir != "" {
		if b, err := os.ReadFile(e.profilePath("series", pid)); err == nil && json.Unmarshal(b, ps) == nil && ps.Assets != nil {
			loaded = true
			ps.savedAt = now
		} else {
			ps = newProfileSeries()
		}
	}
	if !loaded && e.History != nil {
		count := 0
		err := e.History.Scan(ctx, storage.Query{Kind: collector.SnapshotKind, ProfileID: pid, Since: now.Add(-seriesRetention)}, func(r storage.Record) error {
			var snap struct {
				CollectedAt time.Time            `json:"collected_at"`
				Capacity    []collector.Capacity `json:"capacity"`
			}
			if json.Unmarshal(r.Data, &snap) == nil && !snap.CollectedAt.IsZero() {
				ps.record(snap.CollectedAt, snap.Capacity)
				count++
			}
			return nil
		})
		if err != nil {
			e.logf("early-warning: seed series %s from history: %v", pid, err)
		} else if count > 0 {
			e.logf("early-warning: seeded %s capacity series from %d stored snapshots", pid, count)
		}
	}
	e.mu.Lock()
	if _, ok := e.series[pid]; !ok {
		e.series[pid] = ps
	}
	e.mu.Unlock()
}

func (e *Engine) loadBaseline(pid string) (*metasync.RawSnapshot, error) {
	e.baseMu.Lock()
	defer e.baseMu.Unlock()
	if b, ok := e.baselines[pid]; ok {
		return b, nil
	}
	if e.cfg.Dir == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(e.profilePath("schema", pid))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var snap metasync.RawSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, err
	}
	e.baselines[pid] = &snap
	return &snap, nil
}

func (e *Engine) storeBaseline(pid string, snap *metasync.RawSnapshot) error {
	e.baseMu.Lock()
	e.baselines[pid] = snap
	e.baseMu.Unlock()
	if e.cfg.Dir == "" {
		return nil
	}
	return writeJSONAtomic(e.profilePath("schema", pid), snap)
}

// ---- read models ----

type BoardSettings struct {
	MinNotifySeverity   string  `json:"min_notify_severity"`
	RenotifyHours       float64 `json:"renotify_hours"`
	MaintenanceMinutes  float64 `json:"maintenance_minutes"`
	SchemaMinutes       float64 `json:"schema_minutes"`
	CollectionDownAfter int     `json:"collection_down_after"`
}

type BoardSummary struct {
	Critical          int `json:"critical"`
	Warning           int `json:"warning"`
	Info              int `json:"info"`
	Profiles          int `json:"profiles"`
	WithoutLimit      int `json:"without_limit"`
	ObservationFailed int `json:"observation_failed"`
}

type ProfileBoard struct {
	ProfileID           string     `json:"profile_id"`
	Name                string     `json:"name,omitempty"`
	Engine              string     `json:"engine"`
	Environment         string     `json:"environment,omitempty"`
	Criticality         string     `json:"criticality,omitempty"`
	Status              string     `json:"status"` // ok | info | warning | critical | pending
	StorageLimit        string     `json:"storage_limit,omitempty"`
	Forecasts           []Forecast `json:"forecasts"`
	Firing              int        `json:"firing"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	LastError           string     `json:"last_error,omitempty"`
	LastCollectedAt     *time.Time `json:"last_collected_at,omitempty"`
	LastMaintenanceAt   *time.Time `json:"last_maintenance_at,omitempty"`
	MaintenanceStatus   string     `json:"maintenance_status,omitempty"`
	LastSchemaAt        *time.Time `json:"last_schema_at,omitempty"`
	SchemaStatus        string     `json:"schema_status,omitempty"`
	SchemaBaselineAt    *time.Time `json:"schema_baseline_at,omitempty"`
	SchemaTables        int        `json:"schema_tables,omitempty"`
}

type Board struct {
	GeneratedAt  time.Time      `json:"generated_at"`
	LastCycleAt  *time.Time     `json:"last_cycle_at,omitempty"`
	Headline     string         `json:"headline"`
	Settings     BoardSettings  `json:"settings"`
	Delivery     DeliveryStatus `json:"delivery"`
	Summary      BoardSummary   `json:"summary"`
	Profiles     []ProfileBoard `json:"profiles"`
	Firing       []Alert        `json:"firing"`
	Recent       []Alert        `json:"recent"`
	SchemaEvents []Alert        `json:"schema_events"`
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// Board renders the console view for the given (permission-filtered)
// profiles.
func (e *Engine) Board(profiles []dbconn.Profile) Board {
	e.mu.Lock()
	defer e.mu.Unlock()
	board := Board{GeneratedAt: e.now(), LastCycleAt: timePtr(e.st.LastCycleAt), Delivery: e.st.Delivery,
		Settings: BoardSettings{MinNotifySeverity: e.cfg.MinNotifySeverity, RenotifyHours: e.cfg.RenotifyInterval.Hours(), MaintenanceMinutes: e.cfg.MaintenanceEvery.Minutes(), SchemaMinutes: e.cfg.SchemaEvery.Minutes(), CollectionDownAfter: e.cfg.CollectionDownAfter},
		Profiles: []ProfileBoard{}, Firing: []Alert{}, Recent: []Alert{}, SchemaEvents: []Alert{}}
	board.Delivery.Configured = e.Notifier != nil
	if e.Notifier != nil {
		board.Delivery.Target = e.Notifier.Target()
	}
	allowed := map[string]bool{}
	for _, raw := range profiles {
		p := dbconn.ApplyDefaults(raw)
		allowed[p.ID] = true
		m := e.st.Profiles[p.ID]
		if m == nil {
			m = &profileMeta{}
		}
		pb := ProfileBoard{ProfileID: p.ID, Name: p.Name, Engine: p.Type, Environment: p.Environment, Criticality: p.Criticality,
			Status: "pending", Forecasts: e.forecasts[p.ID], ConsecutiveFailures: m.Failures, LastError: m.LastError,
			LastCollectedAt: timePtr(m.LastCollectedAt), LastMaintenanceAt: timePtr(m.LastMaintenanceAt), MaintenanceStatus: m.MaintenanceStatus,
			LastSchemaAt: timePtr(m.LastSchemaAt), SchemaStatus: m.SchemaStatus, SchemaBaselineAt: timePtr(m.SchemaBaselineAt), SchemaTables: m.SchemaTables}
		if p.Capacity != nil {
			pb.StorageLimit = p.Capacity.StorageLimit
		}
		if pb.Forecasts == nil {
			pb.Forecasts = []Forecast{}
		}
		if len(pb.Forecasts) > 6 {
			pb.Forecasts = pb.Forecasts[:6]
		}
		if !m.LastCollectedAt.IsZero() || m.Failures > 0 {
			pb.Status = "ok"
		}
		for _, f := range pb.Forecasts {
			if f.Primary && f.LimitBytes == 0 {
				board.Summary.WithoutLimit++
			}
		}
		if m.Failures > 0 {
			board.Summary.ObservationFailed++
		}
		board.Profiles = append(board.Profiles, pb)
	}
	index := map[string]int{}
	for i, pb := range board.Profiles {
		index[pb.ProfileID] = i
	}
	for _, a := range e.st.Alerts {
		if !allowed[a.ProfileID] {
			continue
		}
		if a.Rule == RuleSchemaChange {
			board.SchemaEvents = append(board.SchemaEvents, *a)
		}
		if a.State == StateFiring {
			board.Firing = append(board.Firing, *a)
			switch a.Severity {
			case SevCritical:
				board.Summary.Critical++
			case SevWarning:
				board.Summary.Warning++
			default:
				board.Summary.Info++
			}
			if i, ok := index[a.ProfileID]; ok {
				pb := &board.Profiles[i]
				pb.Firing++
				if Rank(a.Severity) > Rank(pb.Status) {
					pb.Status = a.Severity
				}
			}
		} else {
			board.Recent = append(board.Recent, *a)
		}
	}
	board.Summary.Profiles = len(board.Profiles)
	sort.SliceStable(board.Firing, func(i, j int) bool {
		if Rank(board.Firing[i].Severity) != Rank(board.Firing[j].Severity) {
			return Rank(board.Firing[i].Severity) > Rank(board.Firing[j].Severity)
		}
		return board.Firing[i].FirstSeen.After(board.Firing[j].FirstSeen)
	})
	sort.SliceStable(board.Recent, func(i, j int) bool { return board.Recent[i].ResolvedAt.After(*board.Recent[j].ResolvedAt) })
	if len(board.Recent) > 50 {
		board.Recent = board.Recent[:50]
	}
	sort.SliceStable(board.SchemaEvents, func(i, j int) bool { return board.SchemaEvents[i].FirstSeen.After(board.SchemaEvents[j].FirstSeen) })
	if len(board.SchemaEvents) > 20 {
		board.SchemaEvents = board.SchemaEvents[:20]
	}
	sort.SliceStable(board.Profiles, func(i, j int) bool {
		if Rank(board.Profiles[i].Status) != Rank(board.Profiles[j].Status) {
			return Rank(board.Profiles[i].Status) > Rank(board.Profiles[j].Status)
		}
		return board.Profiles[i].ProfileID < board.Profiles[j].ProfileID
	})
	board.Headline = headline(board)
	return board
}

func headline(b Board) string {
	if b.LastCycleAt == nil {
		return "아직 평가 주기가 한 번도 돌지 않았습니다 — 관측 수집기(-observe-interval)가 켜져 있는지 확인하세요."
	}
	if b.Summary.Critical+b.Summary.Warning == 0 {
		msg := fmt.Sprintf("프로파일 %d개 모두 예방 경보 없음.", b.Summary.Profiles)
		if b.Summary.WithoutLimit > 0 {
			msg += fmt.Sprintf(" 단, %d개는 용량 한도가 선언되지 않아 고갈 예측을 하지 못합니다.", b.Summary.WithoutLimit)
		}
		return msg
	}
	msg := fmt.Sprintf("긴급 %d · 경고 %d건이 발생 중입니다.", b.Summary.Critical, b.Summary.Warning)
	for _, a := range b.Firing {
		if a.Rule == RuleCapacityForecast || a.Rule == RuleCapacityUsage {
			msg += " 가장 급한 저장공간 경보: " + a.ProfileID + " — " + a.Title
			break
		}
	}
	return msg
}

// ForecastMetric is one gauge sample for /metrics.
type ForecastMetric struct {
	ProfileID string
	Forecast  Forecast
}

// Metrics returns the latest forecasts and firing counts for Prometheus.
func (e *Engine) Metrics() (forecasts []ForecastMetric, firing map[string]int, delivery DeliveryStatus) {
	e.mu.Lock()
	defer e.mu.Unlock()
	firing = map[string]int{SevCritical: 0, SevWarning: 0, SevInfo: 0}
	for _, a := range e.st.Alerts {
		if a.State == StateFiring {
			firing[a.Severity]++
		}
	}
	pids := make([]string, 0, len(e.forecasts))
	for pid := range e.forecasts {
		pids = append(pids, pid)
	}
	sort.Strings(pids)
	for _, pid := range pids {
		for _, f := range e.forecasts[pid] {
			forecasts = append(forecasts, ForecastMetric{ProfileID: pid, Forecast: f})
		}
	}
	return forecasts, firing, e.st.Delivery
}
