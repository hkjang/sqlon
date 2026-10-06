package earlywarning

import (
	"strings"
	"testing"
	"time"

	"sqlon/internal/change"
	"sqlon/internal/collector"
	"sqlon/internal/dbconn"
	"sqlon/internal/metasync"
	"sqlon/internal/observability"
)

func pgProfile(limit string) dbconn.Profile {
	p := dbconn.Profile{ID: "prod-pg", Type: "postgres", Environment: "production", Criticality: "high"}
	if limit != "" {
		p.Capacity = &dbconn.CapacityConfig{StorageLimit: limit}
	}
	return dbconn.ApplyDefaults(p)
}

// footprintSnapshot builds a PostgreSQL-shaped snapshot whose footprint has
// the declared limit stamped on, exactly as the collector produces it.
func footprintSnapshot(p dbconn.Profile, at time.Time, data, wal float64, tables map[string]float64) collector.Snapshot {
	snap := collector.Snapshot{ProfileID: p.ID, Engine: "postgres", CollectedAt: at, Capacity: []collector.Capacity{
		{Scope: "database", Name: "app", UsedBytes: data},
		{Scope: collector.ScopeCluster, Name: "databases", UsedBytes: data},
		{Scope: collector.ScopeWAL, Name: "pg_wal", UsedBytes: wal},
		{Scope: collector.ScopeStorage, Name: collector.FootprintName, UsedBytes: data + wal},
	}}
	for name, v := range tables {
		snap.Capacity = append(snap.Capacity, collector.Capacity{Scope: "table", Name: name, UsedBytes: v})
	}
	collector.ApplyDeclaredLimit(&snap, p)
	return snap
}

func conditionByRule(conds []Condition, rule string) *Condition {
	for i := range conds {
		if conds[i].Rule == rule {
			return &conds[i]
		}
	}
	return nil
}

func TestForecastProjectsDaysToFullFromDeclaredLimit(t *testing.T) {
	p := pgProfile("500GiB")
	ps := newProfileSeries()
	perDay := 40 * gib
	var snap collector.Snapshot
	for h := 0; h <= 48; h++ {
		at := t0.Add(time.Duration(h) * time.Hour)
		snap = footprintSnapshot(p, at, 300*gib+perDay*float64(h)/24, 20*gib, nil)
		ps.record(at, snap.Capacity)
	}
	now := t0.Add(48 * time.Hour)
	fcs := buildForecasts(p, snap, ps, now)
	if len(fcs) == 0 || !fcs[0].Primary || fcs[0].Asset != "storage:footprint" {
		t.Fatalf("the footprint must be the primary forecast: %+v", fcs)
	}
	f := fcs[0]
	// used = 300+80+20 = 400GiB, 100GiB left at 40GiB/day → 2.5 days
	if f.LimitSource != "declared" || f.DaysToFull == nil || *f.DaysToFull < 2.4 || *f.DaysToFull > 2.6 {
		t.Fatalf("expected ~2.5 days on the declared limit, got %+v", f)
	}
	if f.Status != SevCritical {
		t.Fatalf("2.5 days is inside the 3-day critical threshold: %s", f.Status)
	}
	conds := capacityConditions(p, snap, fcs)
	fc := conditionByRule(conds, RuleCapacityForecast)
	if fc == nil || fc.Severity != SevCritical || !strings.Contains(fc.Title, "2.5일") {
		t.Fatalf("missing critical forecast condition: %+v", conds)
	}
	if !strings.Contains(fc.Detail, "WAL 20.0 GiB") {
		t.Fatalf("the detail must break the footprint down so the cause is visible: %s", fc.Detail)
	}
}

func TestForecastIgnoresAStepInsideTheShortWindow(t *testing.T) {
	// Flat for days, then a single 30GiB bulk load an hour ago: the 6h fit
	// is poor (R² < 0.8), so it must not drive a days-to-full projection.
	p := pgProfile("500GiB")
	ps := newProfileSeries()
	var snap collector.Snapshot
	for m := 0; m <= 3*24*60; m += 10 {
		at := t0.Add(time.Duration(m) * time.Minute)
		data := 300 * gib
		if m >= 3*24*60-60 {
			data += 30 * gib
		}
		snap = footprintSnapshot(p, at, data, 1*gib, nil)
		ps.record(at, snap.Capacity)
	}
	f := buildForecasts(p, snap, ps, t0.Add(3*24*time.Hour))[0]
	if f.DaysToFull != nil && *f.DaysToFull < 14 {
		t.Fatalf("a one-off load projected the disk full in %.1f days (short R2 %.2f)", *f.DaysToFull, f.GrowthShort.R2)
	}
}

func TestForecastWithoutLimitIsUnknownAndSaysWhy(t *testing.T) {
	p := pgProfile("")
	snap := footprintSnapshot(p, t0, 10*gib, gib, nil)
	ps := newProfileSeries()
	ps.record(t0, snap.Capacity)
	fcs := buildForecasts(p, snap, ps, t0)
	if len(fcs) != 1 || fcs[0].Status != "unknown" || fcs[0].DaysToFull != nil || !strings.Contains(fcs[0].Note, "storage_limit") {
		t.Fatalf("no limit → unknown with a hint: %+v", fcs)
	}
	conds := capacityConditions(p, snap, fcs)
	c := conditionByRule(conds, RuleLimitUndeclared)
	if c == nil || c.Severity != SevInfo {
		t.Fatalf("an undeclared limit must be an info condition (visible, never paged): %+v", conds)
	}
}

func TestUsageThresholdsUseProfileOverrides(t *testing.T) {
	p := pgProfile("100GiB")
	p.Capacity.WarnPercent, p.Capacity.CriticalPercent = 60, 70
	snap := footprintSnapshot(p, t0, 64*gib, gib, nil) // 65%
	conds := capacityConditions(p, snap, buildForecasts(p, snap, newProfileSeries(), t0))
	c := conditionByRule(conds, RuleCapacityUsage)
	if c == nil || c.Severity != SevWarning {
		t.Fatalf("65%% against a 60/70 override must warn: %+v", conds)
	}
}

func TestGrowthSurgeWithoutLimit(t *testing.T) {
	p := pgProfile("")
	ps := newProfileSeries()
	var snap collector.Snapshot
	// 6 days at +1GiB/day, then the last 6 hours at +48GiB/day (WAL piling up).
	for m := 0; m <= 6*24*60; m += 10 {
		at := t0.Add(time.Duration(m) * time.Minute)
		days := float64(m) / (24 * 60)
		wal := 1 * gib
		if surge := days - (6 - 0.25); surge > 0 {
			wal += 48 * gib * surge
		}
		snap = footprintSnapshot(p, at, 200*gib+gib*days, wal, nil)
		ps.record(at, snap.Capacity)
	}
	conds := capacityConditions(p, snap, buildForecasts(p, snap, ps, t0.Add(6*24*time.Hour)))
	if c := conditionByRule(conds, RuleGrowthSurge); c == nil || c.Severity != SevWarning {
		t.Fatalf("a sustained 48x growth surge must warn even without a limit: %+v", conds)
	}
}

func TestTempSpillAndPrivilegeHint(t *testing.T) {
	p := pgProfile("200GiB")
	snap := footprintSnapshot(p, t0, 50*gib, gib, nil)
	snap.Capacity = append(snap.Capacity, collector.Capacity{Scope: collector.ScopeTemp, Name: "pgsql_tmp", UsedBytes: 12 * gib})
	snap.Evidence = append(snap.Evidence, collector.Evidence{Code: "STORAGE_FOOTPRINT_PARTIAL", Severity: "info", Summary: "grant pg_monitor"})
	conds := capacityConditions(p, snap, buildForecasts(p, snap, newProfileSeries(), t0))
	if c := conditionByRule(conds, RuleTempSpill); c == nil || c.Severity != SevWarning {
		t.Fatalf("12GiB of temp files (>5%% of 200GiB) must warn: %+v", conds)
	}
	if c := conditionByRule(conds, RuleMonitorPrivilege); c == nil || c.Severity != SevInfo {
		t.Fatalf("a partial footprint must surface the privilege hint: %+v", conds)
	}
}

func TestTableSurgeAgainstItsOwnBaseline(t *testing.T) {
	p := pgProfile("1TiB")
	ps := newProfileSeries()
	var snap collector.Snapshot
	for h := 0; h <= 5*24; h++ {
		at := t0.Add(time.Duration(h) * time.Hour)
		days := float64(h) / 24
		orders := 50*gib + 0.5*gib*days // steady +0.5GiB/day
		events := 10 * gib
		if days > 4 {
			events += 20 * gib * (days - 4) // +20GiB/day over the last day
		}
		snap = footprintSnapshot(p, at, 100*gib, gib, map[string]float64{"public.orders": orders, "public.events": events})
		ps.record(at, snap.Capacity)
	}
	conds := tableConditions(snap, ps, t0.Add(5*24*time.Hour))
	var objects []string
	for _, c := range conds {
		objects = append(objects, c.Rule+":"+c.Object)
	}
	if len(conds) != 1 || conds[0].Rule != RuleTableSurge || conds[0].Object != "public.events" {
		t.Fatalf("only public.events should surge, got %v", objects)
	}
}

func TestTableShrinkIsQuiet(t *testing.T) {
	p := pgProfile("1TiB")
	ps := newProfileSeries()
	var snap collector.Snapshot
	for h := 0; h <= 30; h++ {
		at := t0.Add(time.Duration(h) * time.Hour)
		v := 40 * gib
		if h >= 28 {
			v = 2 * gib // truncated
		}
		snap = footprintSnapshot(p, at, 100*gib, gib, map[string]float64{"public.ledger": v})
		ps.record(at, snap.Capacity)
	}
	conds := tableConditions(snap, ps, t0.Add(30*time.Hour))
	if c := conditionByRule(conds, RuleTableShrink); c == nil || !c.QuietResolve {
		t.Fatalf("a 40→2GiB drop must warn and resolve quietly: %+v", conds)
	}
}

func TestCollectionDownNeedsConsecutiveFailures(t *testing.T) {
	p := pgProfile("")
	result := collector.ProfileResult{Status: "error", ErrorCode: "COLLECTION_FAILED", Error: "connection refused"}
	if conds := collectionConditions(p, result, 2, 3); len(conds) != 0 {
		t.Fatalf("two failures are below the threshold")
	}
	conds := collectionConditions(p, result, 3, 3)
	if len(conds) != 1 || conds[0].Severity != SevCritical || !strings.Contains(conds[0].Detail, "connection refused") {
		t.Fatalf("a production profile that cannot be observed is critical: %+v", conds)
	}
}

func TestMaintenanceFindingsBecomeConditions(t *testing.T) {
	conds := maintenanceConditions([]observability.MaintenanceFinding{
		{Category: "wal_archive", Object: "archive_command", Severity: "critical", Detail: "failing"},
		{Category: "config_risk", Object: "max_slot_wal_keep_size", Severity: "info"},
	})
	if len(conds) != 2 || conds[0].Rule != "maint_wal_archive" || conds[0].Check != CheckMaintenance || !strings.HasPrefix(conds[0].Title, "WAL 아카이브") {
		t.Fatalf("maintenance mapping wrong: %+v", conds)
	}
}

func TestBridgedCollectorAlertsSkipSupersededCapacityRules(t *testing.T) {
	conds := bridgedConditions([]collector.Alert{
		{MetricName: "capacity_saturation", Severity: "critical", Message: "old"},
		{MetricName: "query_latency_regression", Severity: "warning", Message: "spiked"},
	})
	if len(conds) != 1 || conds[0].Rule != "query_latency_regression" || !conds[0].Event {
		t.Fatalf("bridge mapping wrong: %+v", conds)
	}
}

func schemaSnap(hash string, at time.Time, tables ...metasync.TableAsset) *metasync.RawSnapshot {
	for i := range tables {
		tables[i].StructHash = tables[i].Name + hash
	}
	return &metasync.RawSnapshot{SourceID: "prod-pg", SchemaHash: hash, CollectedAt: at, Status: "success", Tables: tables}
}

func col(name, typ string) metasync.ColumnAsset {
	return metasync.ColumnAsset{Name: name, DataType: typ, FullType: typ, Nullable: true}
}

func TestSchemaChangeUnplannedVsPlanned(t *testing.T) {
	p := pgProfile("")
	base := schemaSnap("h1", t0,
		metasync.TableAsset{Schema: "public", Name: "orders", Kind: "table", Columns: []metasync.ColumnAsset{col("id", "bigint"), col("memo", "text")}},
		metasync.TableAsset{Schema: "public", Name: "customers", Kind: "table", Columns: []metasync.ColumnAsset{col("id", "bigint")}},
	)
	cur := schemaSnap("h2", t0.Add(15*time.Minute),
		metasync.TableAsset{Schema: "public", Name: "orders", Kind: "table", Columns: []metasync.ColumnAsset{col("id", "bigint")}}, // memo dropped
		metasync.TableAsset{Schema: "public", Name: "customers", Kind: "table", Columns: []metasync.ColumnAsset{col("id", "bigint"), col("grade", "text")}},
	)
	// A plan touched customers, nobody planned the orders change.
	plans := []change.Plan{{ID: "cp-1", ProfileID: "prod-pg", Target: "public.customers", State: change.Completed, UpdatedAt: t0.Add(5 * time.Minute),
		Steps: []change.Step{{Command: "ALTER TABLE public.customers ADD COLUMN grade text"}}}}
	conds := schemaConditions(p, base, cur, plans, map[string]float64{"public.orders": 3 * gib}, nil)
	if len(conds) != 1 {
		t.Fatalf("one event per detected change set, got %+v", conds)
	}
	c := conds[0]
	if !c.Event || c.Severity != SevCritical || c.Attributes["unplanned"] != 1 {
		t.Fatalf("an unplanned column drop in production is critical: %+v", c)
	}
	if !strings.Contains(c.Detail, "[계획 외] 컬럼 삭제 public.orders.memo") || !strings.Contains(c.Detail, "[변경계획] 컬럼 추가 public.customers.grade") {
		t.Fatalf("detail must tag planned/unplanned changes:\n%s", c.Detail)
	}
	for _, ch := range c.Attributes["changes"].([]SchemaChange) {
		// In production an unplanned column add would warn; the plan makes it info.
		if ch.Table == "public.customers" && (!ch.Planned || ch.Severity != SevInfo) {
			t.Fatalf("a change covered by an executed plan must be info: %+v", ch)
		}
	}
}

func TestSchemaChangeSeverityOutsideProduction(t *testing.T) {
	p := dbconn.ApplyDefaults(dbconn.Profile{ID: "dev", Type: "postgres", Environment: "development"})
	base := schemaSnap("a", t0, metasync.TableAsset{Schema: "public", Name: "t", Kind: "table", Columns: []metasync.ColumnAsset{col("id", "int")}})
	cur := schemaSnap("b", t0.Add(time.Hour))
	conds := schemaConditions(p, base, cur, nil, nil, nil)
	if len(conds) != 1 || conds[0].Severity != SevWarning {
		t.Fatalf("a dropped table outside production is capped at warning: %+v", conds)
	}
	if got := schemaConditions(p, base, base, nil, nil, nil); len(got) != 0 {
		t.Fatalf("an identical schema yields nothing: %+v", got)
	}
}

func TestPlannedMatchesOnIdentifierBoundaries(t *testing.T) {
	texts := []string{"alter table public.order_items add column x int"}
	if planned(texts, "public.order") || !planned(texts, "public.order_items") {
		t.Fatalf("plan matching must respect identifier boundaries")
	}
}

func TestForecastAtOrOverTheLimitSaysSo(t *testing.T) {
	p := pgProfile("10GiB")
	snap := footprintSnapshot(p, t0, 9*gib, 2*gib, nil) // 11GiB used of 10GiB
	conds := capacityConditions(p, snap, buildForecasts(p, snap, newProfileSeries(), t0))
	c := conditionByRule(conds, RuleCapacityForecast)
	if c == nil || c.Severity != SevCritical || !strings.Contains(c.Title, "한도 도달") || strings.Contains(c.Detail, "R²") {
		t.Fatalf("an exhausted limit must say so, without an invalid trend: %+v", c)
	}
}

func TestTableShrinkWithinTheFirstHour(t *testing.T) {
	// The live reproduction: a table loaded and truncated minutes apart.
	p := pgProfile("1TiB")
	ps := newProfileSeries()
	var snap collector.Snapshot
	sizes := []float64{0, 0.6 * gib, 1.2 * gib, 1.2 * gib, 0, 0}
	for m, v := range sizes {
		at := t0.Add(time.Duration(m) * time.Minute)
		snap = footprintSnapshot(p, at, 100*gib, gib, map[string]float64{"public.events": v})
		ps.record(at, snap.Capacity)
	}
	if c := conditionByRule(tableConditions(snap, ps, t0.Add(5*time.Minute)), RuleTableShrink); c == nil {
		t.Fatalf("a 1.2GiB table truncated minutes after loading must be reported")
	}
}

func TestPrivilegeHintsAreMergedIntoOneCondition(t *testing.T) {
	p := pgProfile("")
	snap := footprintSnapshot(p, t0, gib, 0, nil)
	snap.Evidence = append(snap.Evidence,
		collector.Evidence{Code: "STORAGE_FOOTPRINT_PARTIAL", Summary: "GRANT PROCESS"},
		collector.Evidence{Code: "STORAGE_FOOTPRINT_PARTIAL", Summary: "GRANT REPLICATION CLIENT"})
	conds := capacityConditions(p, snap, buildForecasts(p, snap, newProfileSeries(), t0))
	c := conditionByRule(conds, RuleMonitorPrivilege)
	if c == nil || !strings.Contains(c.Detail, "GRANT PROCESS") || !strings.Contains(c.Detail, "GRANT REPLICATION CLIENT") {
		t.Fatalf("every missing grant must be listed: %+v", c)
	}
}
