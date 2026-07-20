package postgres

import (
	"context"
	"fmt"
	"time"

	"sqlon/internal/dbconn"
	"sqlon/internal/observability"
)

// Maintenance implements observability.MaintenanceProvider for PostgreSQL.
//
// It surfaces three latent risks that report no error until they cause an
// outage:
//
//   - Transaction-ID wraparound: age(datfrozenxid) / age(relfrozenxid) approaching
//     autovacuum_freeze_max_age (and ultimately the 2^31 hard ceiling that
//     forces the database into single-user recovery).
//   - Table bloat: high dead-tuple ratio on sizeable tables (pg_stat_user_tables),
//     a proxy for tables that need aggressive VACUUM or pg_repack.
//   - Inactive replication slots: a slot with active=false keeps pinning WAL,
//     silently filling the disk (pg_replication_slots).
//
// All queries are compile-time constants against base-license catalog views.
type Maintenance struct{}

// The 2^31 transaction-ID ceiling. PostgreSQL forces a shutdown well before
// this, but age relative to it is the universally understood wraparound metric.
const xidCeiling = 2147483648.0

// wraparoundSQL reports the database-wide frozen-XID age, the two most at-risk
// tables, and the effective autovacuum_freeze_max_age so the service can rank
// headroom without hard-coding the setting.
const wraparoundSQL = `WITH s AS (
    SELECT setting::bigint AS freeze_max_age
    FROM pg_catalog.pg_settings WHERE name = 'autovacuum_freeze_max_age'
)
SELECT 'database' AS kind, datname AS object,
       age(datfrozenxid)::bigint AS xid_age,
       (SELECT freeze_max_age FROM s) AS freeze_max_age
FROM pg_catalog.pg_database
WHERE datallowconn
UNION ALL
SELECT 'table' AS kind,
       quote_ident(n.nspname) || '.' || quote_ident(c.relname) AS object,
       age(c.relfrozenxid)::bigint AS xid_age,
       (SELECT freeze_max_age FROM s) AS freeze_max_age
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r','m','t')
  AND c.relfrozenxid <> 0
  AND n.nspname NOT IN ('pg_catalog','information_schema')
ORDER BY xid_age DESC
LIMIT 25`

// bloatSQL uses live/dead tuple counters (cheap, always available) rather than
// the heavier page-estimation query, so it works on a monitoring account with
// only pg_stat access.
const bloatSQL = `SELECT quote_ident(schemaname) || '.' || quote_ident(relname) AS object,
       n_live_tup AS live_tuples,
       n_dead_tup AS dead_tuples,
       CASE WHEN n_live_tup + n_dead_tup > 0
            THEN round(100.0 * n_dead_tup / (n_live_tup + n_dead_tup), 1)
            ELSE 0 END AS dead_ratio_pct,
       COALESCE(last_autovacuum, last_vacuum) AS last_vacuum
FROM pg_catalog.pg_stat_user_tables
WHERE n_dead_tup > 1000
ORDER BY n_dead_tup DESC
LIMIT 20`

// slotSQL reports replication slots and, for inactive ones, how much WAL they
// are pinning relative to the current insert position.
const slotSQL = `SELECT slot_name, slot_type, active,
       COALESCE(pg_wal_lsn_diff(pg_current_wal_insert_lsn(), restart_lsn), 0)::bigint AS retained_bytes
FROM pg_catalog.pg_replication_slots
ORDER BY retained_bytes DESC
LIMIT 50`

// Thresholds. Wraparound is scored against both the operator's freeze setting
// (autovacuum should have acted) and the hard 2^31 ceiling.
const (
	bloatWarnRatio       = 20.0 // % dead tuples
	bloatCriticalRatio   = 40.0
	bloatMinDeadTuples   = 100000 // ignore small churny tables below this many dead rows
	slotWarnBytes        = 1 << 30 // 1 GiB retained by an inactive slot
	slotCriticalBytes    = 8 << 30 // 8 GiB
	wraparoundWarnFrac   = 0.90    // fraction of freeze_max_age consumed
	wraparoundCritFrac   = 1.10    // past freeze_max_age → autovacuum is behind
	wraparoundCeilingCap = 0.80    // fraction of the 2^31 hard ceiling → always critical
)

func (Maintenance) Maintenance(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile) (observability.MaintenanceData, error) {
	now := time.Now().UTC()
	data := observability.MaintenanceData{ProfileID: p.ID, Engine: "postgres", Findings: []observability.MaintenanceFinding{}}

	// ---- 1. transaction-ID wraparound (fail-closed: a hard error aborts, since
	// wraparound is the most severe risk and must not be silently skipped) ----
	rows, err := q.SystemQuery(ctx, p.ID, wraparoundSQL)
	if err != nil {
		return data, fmt.Errorf("collect PostgreSQL wraparound age: %w", err)
	}
	data.Checks++
	for _, row := range rows {
		age := observability.Number(row, "xid_age")
		freezeMax := observability.Number(row, "freeze_max_age")
		object := observability.Text(row, "object")
		kind := observability.Text(row, "kind")
		if age <= 0 {
			continue
		}
		fracFreeze := 0.0
		if freezeMax > 0 {
			fracFreeze = age / freezeMax
		}
		fracCeiling := age / xidCeiling
		severity := ""
		switch {
		case fracCeiling >= wraparoundCeilingCap || fracFreeze >= wraparoundCritFrac:
			severity = "critical"
		case fracFreeze >= wraparoundWarnFrac:
			severity = "warning"
		}
		if severity == "" {
			continue
		}
		data.Findings = append(data.Findings, observability.MaintenanceFinding{
			Category:  "wraparound",
			Object:    kind + " " + object,
			Detail:    fmt.Sprintf("frozen XID age %.0f, autovacuum_freeze_max_age %.0f (2^31 대비 %.0f%%)", age, freezeMax, fracCeiling*100),
			Metric:    "xid_age",
			Value:     age,
			Threshold: freezeMax,
			Recommendation: "VACUUM (FREEZE) 를 변경계획으로 수행하세요. 임박 시 autovacuum_freeze_max_age·autovacuum_vacuum_cost_limit 재조정을 검토하세요.",
			Severity:    severity,
			CollectedAt: now,
		})
	}

	// ---- 2. table bloat (dead-tuple ratio) — soft: a permission error here is
	// downgraded to a limitation so wraparound findings still return ----
	if rows, err := q.SystemQuery(ctx, p.ID, bloatSQL); err != nil {
		data.Limitations = append(data.Limitations, "테이블 블로트(pg_stat_user_tables) 수집을 건너뜀: "+err.Error())
	} else {
		data.Checks++
		for _, row := range rows {
			dead := observability.Number(row, "dead_tuples")
			ratio := observability.Number(row, "dead_ratio_pct")
			if dead < bloatMinDeadTuples {
				continue
			}
			severity := ""
			switch {
			case ratio >= bloatCriticalRatio:
				severity = "critical"
			case ratio >= bloatWarnRatio:
				severity = "warning"
			}
			if severity == "" {
				continue
			}
			// Bloat is never as urgent as wraparound; cap it at warning so a
			// churny table cannot mask a real wraparound-critical instance.
			if severity == "critical" {
				severity = "warning"
			}
			lastVac := observability.Text(row, "last_vacuum")
			detail := fmt.Sprintf("dead tuples %.0f / live %.0f (%.1f%%)", dead, observability.Number(row, "live_tuples"), ratio)
			if lastVac != "" {
				detail += ", 마지막 VACUUM " + lastVac
			}
			data.Findings = append(data.Findings, observability.MaintenanceFinding{
				Category:       "bloat",
				Object:         observability.Text(row, "object"),
				Detail:         detail,
				Metric:         "dead_ratio_pct",
				Value:          ratio,
				Threshold:      bloatWarnRatio,
				Recommendation: "VACUUM (ANALYZE) 또는 대용량 테이블은 pg_repack 을 변경계획으로 수행하세요. autovacuum 스케일 팩터 조정도 검토하세요.",
				Severity:       severity,
				CollectedAt:    now,
			})
		}
	}

	// ---- 3. inactive replication slots retaining WAL — soft ----
	if rows, err := q.SystemQuery(ctx, p.ID, slotSQL); err != nil {
		data.Limitations = append(data.Limitations, "복제 슬롯(pg_replication_slots) 수집을 건너뜀: "+err.Error())
	} else {
		data.Checks++
		for _, row := range rows {
			active := observability.Text(row, "active")
			if active == "true" || active == "1" {
				continue
			}
			retained := observability.Number(row, "retained_bytes")
			severity := ""
			switch {
			case retained >= slotCriticalBytes:
				severity = "critical"
			case retained >= slotWarnBytes:
				severity = "warning"
			}
			if severity == "" {
				continue
			}
			data.Findings = append(data.Findings, observability.MaintenanceFinding{
				Category:       "replication_slot",
				Object:         observability.Text(row, "slot_name"),
				Detail:         fmt.Sprintf("비활성 %s 슬롯이 WAL %.0f bytes 를 붙잡고 있습니다", observability.Text(row, "slot_type"), retained),
				Metric:         "retained_bytes",
				Value:          retained,
				Threshold:      slotWarnBytes,
				Recommendation: "소비자가 사라진 슬롯이면 SELECT pg_drop_replication_slot(...) 을 변경계획으로 제거하세요. WAL 디스크 포화로 인한 정지를 예방합니다.",
				Severity:       severity,
				CollectedAt:    now,
			})
		}
	}

	return data, nil
}
