package postgres

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"sqlon/internal/dbconn"
	"sqlon/internal/observability"
)

// The checks in this file find what fills a PostgreSQL data volume before it
// is full: WAL that cannot be recycled (a failing or slow archiver, a slot
// whose consumer stopped or fell behind) and transactions that keep VACUUM
// from reclaiming dead rows. Each one is a cause, not a symptom — by the time
// the volume is at 95% they have usually been visible for hours.

// serverProbeSQL reads the settings the checks need through pg_settings
// (which hides rows this role may not see instead of raising an error) and
// lists the directory functions it may execute. Nothing here can fail on
// privileges, so it never feeds the profile's circuit breaker.
const serverProbeSQL = `SELECT name, setting, COALESCE(unit, '') AS unit FROM pg_catalog.pg_settings
WHERE name IN ('server_version_num', 'archive_mode', 'archive_command', 'archive_library', 'max_wal_size',
               'wal_keep_size', 'wal_keep_segments', 'wal_segment_size', 'max_slot_wal_keep_size', 'autovacuum')
UNION ALL
SELECT '__listable', COALESCE(string_agg(p.proname, ','), ''), ''
FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
WHERE n.nspname = 'pg_catalog' AND p.pronargs = 0
  AND p.proname IN ('pg_ls_waldir', 'pg_ls_archive_statusdir') AND has_function_privilege(p.oid, 'EXECUTE')
UNION ALL
SELECT '__in_recovery', CASE WHEN pg_is_in_recovery() THEN 'on' ELSE 'off' END, ''`

// slotSQL13 adds wal_status/safe_wal_size (PostgreSQL 13+). The LSN base is
// chosen by CASE because pg_current_wal_insert_lsn() raises on a standby.
const slotSQL13 = `SELECT slot_name, slot_type, active,
       COALESCE(wal_status, '') AS wal_status,
       COALESCE(safe_wal_size, -1)::double precision AS safe_wal_size,
       COALESCE(pg_wal_lsn_diff(CASE WHEN pg_is_in_recovery() THEN pg_last_wal_replay_lsn() ELSE pg_current_wal_insert_lsn() END, restart_lsn), 0)::bigint AS retained_bytes
FROM pg_catalog.pg_replication_slots
ORDER BY retained_bytes DESC
LIMIT 50`

const archiverStatusSQL = `SELECT archived_count, failed_count,
       COALESCE(last_failed_wal, '') AS last_failed_wal,
       COALESCE(EXTRACT(EPOCH FROM (now() - last_archived_time)), -1)::double precision AS since_archived_s,
       (last_failed_time IS NOT NULL AND (last_archived_time IS NULL OR last_failed_time > last_archived_time)) AS failing
FROM pg_catalog.pg_stat_archiver`

const (
	walDirSizePart   = `(SELECT COALESCE(SUM(size), 0) FROM pg_catalog.pg_ls_waldir())::double precision AS wal_bytes`
	readyFilesPart   = `(SELECT COUNT(*) FROM pg_catalog.pg_ls_archive_statusdir() WHERE name LIKE '%.ready')::double precision AS ready_files`
	vacuumBlockerSQL = `SELECT 'session' AS source, pid::text AS object, COALESCE(usename::text, '') AS owner, COALESCE(state, '') AS state,
       COALESCE(EXTRACT(EPOCH FROM (now() - xact_start)), 0)::double precision AS open_seconds
FROM pg_catalog.pg_stat_activity
WHERE xact_start IS NOT NULL AND pid <> pg_backend_pid() AND backend_type = 'client backend'
UNION ALL
SELECT 'prepared', gid, COALESCE(owner::text, ''), 'prepared',
       EXTRACT(EPOCH FROM (now() - prepared))::double precision
FROM pg_catalog.pg_prepared_xacts
ORDER BY open_seconds DESC
LIMIT 10`
)

const (
	gib                     = float64(1 << 30)
	archiveBacklogWarnBytes = 1 * gib
	archiveBacklogCritBytes = 8 * gib
	activeSlotWarnBytes     = 8 * gib  // a connected consumer this far behind still pins WAL
	walOversizeWarnFactor   = 2.0      // pg_wal above 2x its configured budget
	walOversizeCritFactor   = 4.0      // ... above 4x
	walOversizeMinExcess    = 1 * gib  // ignore small installations' noise
	walOversizeCritExcess   = 8 * gib  //
	archiveFailCritAfter    = 600.0    // seconds without a successful archive
	sessionXactWarnSeconds  = 3600.0   // 1h open transaction holds back VACUUM
	sessionXactCritSeconds  = 6 * 3600 //
	preparedWarnSeconds     = 1800.0   // forgotten two-phase commits never end on their own
	preparedCritSeconds     = 6 * 3600 //
	defaultWALSegmentBytes  = 16 << 20
)

type serverEnv struct {
	ok         bool
	version    int
	inRecovery bool
	settings   map[string]setting
	listable   map[string]bool
}

type setting struct{ value, unit string }

func (e serverEnv) has(name string) bool { _, ok := e.settings[name]; return ok }
func (e serverEnv) text(name string) string {
	return strings.TrimSpace(e.settings[name].value)
}

// bytes converts a size setting using its pg_settings unit ("B", "kB",
// "8kB", "MB", "16MB", "GB"). Negative values (-1 = unlimited) pass through.
func (e serverEnv) bytes(name string) (float64, bool) {
	s, ok := e.settings[name]
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s.value), 64)
	if err != nil {
		return 0, false
	}
	if v < 0 {
		return v, true
	}
	return v * unitBytes(s.unit), true
}

func unitBytes(unit string) float64 {
	unit = strings.TrimSpace(unit)
	i := 0
	for i < len(unit) && unit[i] >= '0' && unit[i] <= '9' {
		i++
	}
	mult := 1.0
	if i > 0 {
		mult, _ = strconv.ParseFloat(unit[:i], 64)
	}
	switch unit[i:] {
	case "kB":
		return mult * 1024
	case "MB":
		return mult * (1 << 20)
	case "GB":
		return mult * (1 << 30)
	case "TB":
		return mult * (1 << 40)
	default: // "B" or ""
		return mult
	}
}

func probeServer(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile, data *observability.MaintenanceData) serverEnv {
	env := serverEnv{settings: map[string]setting{}, listable: map[string]bool{}}
	rows, err := q.SystemQuery(ctx, p.ID, serverProbeSQL)
	if err != nil {
		data.Limitations = append(data.Limitations, "서버 설정(pg_settings) 조회를 건너뜀 — WAL·아카이브 점검을 생략합니다: "+err.Error())
		return env
	}
	env.ok = true
	for _, row := range rows {
		name := observability.Text(row, "name")
		value := observability.Text(row, "setting")
		switch name {
		case "__listable":
			for _, fn := range strings.Split(value, ",") {
				if fn = strings.TrimSpace(fn); fn != "" {
					env.listable[fn] = true
				}
			}
		case "__in_recovery":
			env.inRecovery = value == "on"
		default:
			env.settings[name] = setting{value: value, unit: observability.Text(row, "unit")}
		}
	}
	env.version, _ = strconv.Atoi(env.text("server_version_num"))
	return env
}

// checkSlots replaces the inactive-only slot scan. Besides WAL pinned by an
// abandoned slot it reports a connected consumer that fell far behind (the
// usual logical-decoding/CDC failure) and, on 13+, slots PostgreSQL itself
// marks as about to be or already invalidated. Returns the slot count.
func checkSlots(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile, env serverEnv, data *observability.MaintenanceData, now time.Time) int {
	query := slotSQL
	if env.version >= 130000 {
		query = slotSQL13
	}
	rows, err := q.SystemQuery(ctx, p.ID, query)
	if err != nil {
		data.Limitations = append(data.Limitations, "복제 슬롯(pg_replication_slots) 수집을 건너뜀: "+err.Error())
		return 0
	}
	data.Checks++
	for _, row := range rows {
		name := observability.Text(row, "slot_name")
		kind := observability.Text(row, "slot_type")
		active := isTrue(observability.Text(row, "active"))
		retained := observability.Number(row, "retained_bytes")
		walStatus := observability.Text(row, "wal_status")
		finding := observability.MaintenanceFinding{Category: "replication_slot", Object: name, Metric: "retained_bytes", Value: retained, Threshold: slotWarnBytes, CollectedAt: now}
		switch {
		case walStatus == "lost":
			finding.Severity = "warning"
			finding.Detail = fmt.Sprintf("%s 슬롯이 이미 무효화(lost)되었습니다 — 필요한 WAL이 지워져 이 슬롯의 소비자는 더 이상 이어받을 수 없습니다", kind)
			finding.Recommendation = "소비자를 재동기화(새 슬롯으로 재구성)하고, 무효화된 슬롯은 pg_drop_replication_slot(...) 으로 변경계획을 통해 제거하세요."
		case !active && retained >= slotCriticalBytes:
			finding.Severity = "critical"
			finding.Detail = fmt.Sprintf("비활성 %s 슬롯이 WAL %s 를 붙잡고 있습니다", kind, humanBytes(retained))
			finding.Recommendation = "소비자가 사라진 슬롯이면 SELECT pg_drop_replication_slot(...) 을 변경계획으로 제거하세요. WAL 디스크 포화로 인한 정지를 예방합니다."
		case !active && retained >= slotWarnBytes:
			finding.Severity = "warning"
			finding.Detail = fmt.Sprintf("비활성 %s 슬롯이 WAL %s 를 붙잡고 있습니다", kind, humanBytes(retained))
			finding.Recommendation = "소비자가 사라진 슬롯이면 SELECT pg_drop_replication_slot(...) 을 변경계획으로 제거하세요. WAL 디스크 포화로 인한 정지를 예방합니다."
		case active && retained >= activeSlotWarnBytes:
			finding.Severity = "warning"
			finding.Detail = fmt.Sprintf("연결된 %s 슬롯의 소비자가 WAL %s 만큼 뒤처져 있습니다 — 따라잡을 때까지 그만큼 디스크를 차지합니다", kind, humanBytes(retained))
			finding.Recommendation = "소비자(스탠바이·CDC/Debezium·논리 복제 구독자)의 처리 지연 원인을 확인하세요. 회복이 어렵다면 max_slot_wal_keep_size 로 슬롯이 붙잡을 수 있는 WAL 상한을 두세요."
			finding.Threshold = activeSlotWarnBytes
		case walStatus == "unreserved":
			finding.Severity = "warning"
			finding.Detail = fmt.Sprintf("%s 슬롯이 max_slot_wal_keep_size 한도를 넘었습니다(unreserved) — 다음 체크포인트에서 무효화될 수 있습니다", kind)
			finding.Recommendation = "소비자가 빨리 따라잡지 못하면 슬롯이 무효화됩니다. 소비자 상태를 먼저 확인하세요."
		default:
			continue
		}
		data.Findings = append(data.Findings, finding)
	}
	if env.ok && len(rows) > 0 {
		if keep, ok := env.bytes("max_slot_wal_keep_size"); ok && keep < 0 {
			data.Findings = append(data.Findings, observability.MaintenanceFinding{
				Category: "config_risk", Object: "max_slot_wal_keep_size",
				Detail: fmt.Sprintf("복제 슬롯 %d개가 있고 max_slot_wal_keep_size=-1(무제한)입니다 — 소비자가 멈추면 슬롯이 디스크가 찰 때까지 WAL을 붙잡습니다", len(rows)),
				Metric: "max_slot_wal_keep_size", Value: keep,
				Recommendation: "볼륨 여유에 맞춰 max_slot_wal_keep_size 를 설정하세요(예: 볼륨의 20%). 한도를 넘은 슬롯은 무효화되지만 DB 전체 정지는 막습니다.",
				Severity:       "info", CollectedAt: now,
			})
		}
	}
	return len(rows)
}

// checkWALArchiving reports an archiver that is failing or falling behind
// and a pg_wal directory far above its configured budget. While archiving
// fails PostgreSQL keeps every WAL segment, so a broken archive_command fills
// the volume at the database's full write rate.
func checkWALArchiving(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile, env serverEnv, data *observability.MaintenanceData, now time.Time) {
	if !env.ok {
		return
	}
	mode := strings.ToLower(env.text("archive_mode"))
	archiving := (mode == "on" && !env.inRecovery) || mode == "always"
	segment, ok := env.bytes("wal_segment_size")
	if !ok || segment <= 0 {
		segment = defaultWALSegmentBytes
	}

	if archiving {
		if rows, err := q.SystemQuery(ctx, p.ID, archiverStatusSQL); err != nil {
			data.Limitations = append(data.Limitations, "WAL 아카이버(pg_stat_archiver) 수집을 건너뜀: "+err.Error())
		} else if len(rows) > 0 {
			data.Checks++
			row := rows[0]
			if isTrue(observability.Text(row, "failing")) {
				since := observability.Number(row, "since_archived_s")
				severity := "warning"
				if since < 0 || since >= archiveFailCritAfter {
					severity = "critical"
				}
				lastOK := "성공 기록 없음"
				if since >= 0 {
					lastOK = fmt.Sprintf("마지막 성공 %s 전", humanDuration(since))
				}
				data.Findings = append(data.Findings, observability.MaintenanceFinding{
					Category: "wal_archive", Object: "archive_command",
					Detail: fmt.Sprintf("WAL 아카이브가 실패하고 있습니다 (실패 %d회, 마지막 실패 %s, %s) — 성공할 때까지 WAL이 지워지지 않고 쌓입니다", observability.Int(row, "failed_count"), observability.Text(row, "last_failed_wal"), lastOK),
					Metric: "since_archived_seconds", Value: since, Threshold: archiveFailCritAfter,
					Recommendation: "서버 로그에서 archive_command 오류(대상 경로 권한·용량, 네트워크, 백업 도구 상태)를 확인해 복구하세요. 아카이브를 의도적으로 끊었다면 archive_mode 를 끄는 변경계획을 세우세요.",
					Severity:       severity, CollectedAt: now,
				})
			}
		}
		if env.has("archive_command") && env.text("archive_command") == "" && env.text("archive_library") == "" {
			data.Findings = append(data.Findings, observability.MaintenanceFinding{
				Category: "wal_archive", Object: "archive_command",
				Detail:         "archive_mode 가 켜져 있지만 archive_command·archive_library 가 비어 있습니다 — WAL이 아카이브 대기 상태로 계속 쌓입니다",
				Recommendation: "아카이브 명령을 설정하거나, 아카이브가 필요 없다면 archive_mode=off 로 바꾸는 변경계획을 세우세요(재시작 필요).",
				Severity:       "warning", CollectedAt: now,
			})
		}
	}

	var parts []string
	if env.listable["pg_ls_waldir"] {
		parts = append(parts, walDirSizePart)
	}
	if archiving && env.listable["pg_ls_archive_statusdir"] {
		parts = append(parts, readyFilesPart)
	}
	if len(parts) == 0 {
		if !env.listable["pg_ls_waldir"] {
			data.Limitations = append(data.Limitations, "pg_wal 디렉터리 크기를 보려면 pg_monitor 역할이 필요합니다 — WAL 과다·아카이브 적체 점검을 생략했습니다.")
		}
		return
	}
	rows, err := q.SystemQuery(ctx, p.ID, "SELECT "+strings.Join(parts, ",\n       "))
	if err != nil {
		data.Limitations = append(data.Limitations, "pg_wal 디렉터리 크기 수집을 건너뜀: "+err.Error())
		return
	}
	if len(rows) == 0 {
		return
	}
	data.Checks++
	row := rows[0]
	if _, ok := columnPresent(row, "ready_files"); ok {
		backlog := observability.Number(row, "ready_files") * segment
		severity := ""
		switch {
		case backlog >= archiveBacklogCritBytes:
			severity = "critical"
		case backlog >= archiveBacklogWarnBytes:
			severity = "warning"
		}
		if severity != "" {
			data.Findings = append(data.Findings, observability.MaintenanceFinding{
				Category: "wal_archive", Object: "archive_status",
				Detail: fmt.Sprintf("아카이브 대기 WAL %.0f개(%s)가 쌓였습니다 — 아카이브가 쓰기 속도를 못 따라가거나 멈췄습니다", observability.Number(row, "ready_files"), humanBytes(backlog)),
				Metric: "archive_backlog_bytes", Value: backlog, Threshold: archiveBacklogWarnBytes,
				Recommendation: "archive_command 의 처리 속도(압축·전송 대상)와 실패 여부를 확인하세요. 적체가 계속 늘면 디스크가 찹니다.",
				Severity:       severity, CollectedAt: now,
			})
		}
	}
	if _, ok := columnPresent(row, "wal_bytes"); !ok {
		return
	}
	walBytes := observability.Number(row, "wal_bytes")
	budget, ok := env.bytes("max_wal_size")
	if !ok || budget <= 0 {
		return
	}
	if keep, ok := env.bytes("wal_keep_size"); ok && keep > 0 {
		budget += keep
	} else if segs, ok := env.bytes("wal_keep_segments"); ok && segs > 0 {
		budget += segs * segment
	}
	excess := walBytes - budget
	severity := ""
	switch {
	case walBytes >= walOversizeCritFactor*budget && excess >= walOversizeCritExcess:
		severity = "critical"
	case walBytes >= walOversizeWarnFactor*budget && excess >= walOversizeMinExcess:
		severity = "warning"
	}
	if severity == "" {
		return
	}
	data.Findings = append(data.Findings, observability.MaintenanceFinding{
		Category: "wal_retention", Object: "pg_wal",
		Detail: fmt.Sprintf("pg_wal 이 %s 로 설정 예산(max_wal_size+wal_keep_size = %s)의 %.1f배입니다 — WAL이 재활용되지 못하고 있습니다", humanBytes(walBytes), humanBytes(budget), walBytes/budget),
		Metric: "wal_bytes", Value: walBytes, Threshold: budget * walOversizeWarnFactor,
		Recommendation: "WAL을 붙잡는 원인을 확인하세요: 비활성·지연 복제 슬롯, 실패하는 archive_command, 지나치게 큰 wal_keep_size, 장시간 실행 중인 체크포인트.",
		Severity:       severity, CollectedAt: now,
	})
}

// checkVacuumBlockers reports transactions old enough to stop VACUUM from
// reclaiming dead rows everywhere — the slow way a database outgrows its
// volume. Skipped on a standby, where they do not hold back the primary
// unless hot_standby_feedback is on.
func checkVacuumBlockers(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile, env serverEnv, data *observability.MaintenanceData, now time.Time) {
	if !env.ok || env.inRecovery {
		return
	}
	if strings.EqualFold(env.text("autovacuum"), "off") {
		data.Findings = append(data.Findings, observability.MaintenanceFinding{
			Category: "config_risk", Object: "autovacuum",
			Detail:         "autovacuum 이 꺼져 있습니다 — 수동 VACUUM 이 없으면 dead tuple 이 회수되지 않아 테이블이 계속 커지고 wraparound 위험도 커집니다",
			Recommendation: "autovacuum=on 으로 되돌리는 변경계획을 세우세요.",
			Severity:       "warning", CollectedAt: now,
		})
	}
	rows, err := q.SystemQuery(ctx, p.ID, vacuumBlockerSQL)
	if err != nil {
		data.Limitations = append(data.Limitations, "장기 트랜잭션(pg_stat_activity·pg_prepared_xacts) 수집을 건너뜀: "+err.Error())
		return
	}
	data.Checks++
	for _, row := range rows {
		source := observability.Text(row, "source")
		open := observability.Number(row, "open_seconds")
		warnAt, critAt := sessionXactWarnSeconds, float64(sessionXactCritSeconds)
		if source == "prepared" {
			warnAt, critAt = preparedWarnSeconds, float64(preparedCritSeconds)
		}
		severity := ""
		switch {
		case open >= critAt:
			severity = "critical"
		case open >= warnAt:
			severity = "warning"
		}
		if severity == "" {
			continue
		}
		finding := observability.MaintenanceFinding{
			Category: "vacuum_blocker", Metric: "open_seconds", Value: open, Threshold: warnAt,
			Severity: severity, CollectedAt: now,
		}
		owner := observability.Text(row, "owner")
		if source == "prepared" {
			finding.Object = "prepared " + observability.Text(row, "object")
			finding.Detail = fmt.Sprintf("2단계 커밋 트랜잭션이 %s 째 준비(prepared) 상태입니다 (소유자 %s) — 끝날 때까지 VACUUM 이 이 시점 이후 dead tuple 을 회수하지 못합니다", humanDuration(open), owner)
			finding.Recommendation = "트랜잭션 관리자(애플리케이션)에서 결론을 내리거나, 버려진 것이 확실하면 COMMIT PREPARED / ROLLBACK PREPARED 를 변경계획으로 수행하세요."
		} else {
			finding.Object = "pid " + observability.Text(row, "object")
			finding.Detail = fmt.Sprintf("트랜잭션이 %s 째 열려 있습니다 (상태 %s, 사용자 %s) — 끝날 때까지 VACUUM 이 dead tuple 을 회수하지 못해 테이블이 커집니다", humanDuration(open), observability.Text(row, "state"), owner)
			finding.Recommendation = "애플리케이션의 커밋 누락을 확인하세요. idle_in_transaction_session_timeout 설정과, 필요 시 pg_terminate_backend 를 변경계획으로 검토하세요."
		}
		data.Findings = append(data.Findings, finding)
	}
}

func isTrue(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v == "true" || v == "t" || v == "1" || v == "on"
}

func humanBytes(v float64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f %s", v, units[i])
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

func humanDuration(seconds float64) string {
	d := time.Duration(seconds) * time.Second
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%.1f일", d.Hours()/24)
	case d >= time.Hour:
		return fmt.Sprintf("%.1f시간", d.Hours())
	case d >= time.Minute:
		return fmt.Sprintf("%.0f분", d.Minutes())
	default:
		return fmt.Sprintf("%.0f초", d.Seconds())
	}
}
