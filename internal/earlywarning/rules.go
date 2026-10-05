package earlywarning

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"sqlon/internal/collector"
	"sqlon/internal/dbconn"
	"sqlon/internal/observability"
)

const (
	storageStep     = 10 * time.Minute
	tableStep       = time.Hour
	seriesRetention = 8 * 24 * time.Hour

	gib = float64(1 << 30)

	// A short-window projection counts only when the growth inside it is
	// steady (R² ≥ 0.8). A nightly batch that loads for an hour inside a
	// six-hour window fits a line poorly and must not page anyone; WAL piling
	// up behind a broken archiver fits one almost perfectly.
	shortProjectionMinR2 = 0.8
	surgeMinR2           = 0.6
	surgeFactor          = 3.0
	surgeMinBytesPerDay  = 1 * gib
	surgeMinFraction     = 0.02 // of current usage per day

	tableSurgeMinBytes = 1 * gib
	tableSurgeFactor   = 3.0
	tableShrinkMin     = 1 * gib
	tableShrinkRatio   = 0.5

	tempSpillMinBytes = 5 * gib
	tempSpillFraction = 0.05 // of the storage limit
)

func assetKey(scope, name string) string { return scope + ":" + name }

// volumeScope reports whether a capacity scope is part of the storage
// volume (as opposed to a single table).
func volumeScope(scope string) bool {
	switch scope {
	case collector.ScopeStorage, collector.ScopeCluster, collector.ScopeWAL, collector.ScopeTemp, collector.ScopeLog, collector.ScopeVolume, "database", "tablespace":
		return true
	}
	return false
}

// profileSeries is one profile's capacity history, keyed by asset.
type profileSeries struct {
	Assets  map[string]*Series `json:"assets"`
	dirty   bool
	savedAt time.Time
}

func newProfileSeries() *profileSeries { return &profileSeries{Assets: map[string]*Series{}} }

func (ps *profileSeries) record(at time.Time, capacity []collector.Capacity) {
	for _, c := range capacity {
		step := storageStep
		switch {
		case c.Scope == "table":
			step = tableStep
		case !volumeScope(c.Scope):
			continue
		}
		key := assetKey(c.Scope, c.Name)
		s := ps.Assets[key]
		if s == nil {
			s = newSeries(step)
			ps.Assets[key] = s
		}
		s.Add(at, c.UsedBytes, seriesRetention)
	}
	cutoff := at.Add(-seriesRetention).Unix()
	for key, s := range ps.Assets {
		if last, ok := s.Last(); !ok || last.T < cutoff {
			delete(ps.Assets, key)
		}
	}
	ps.dirty = true
}

func productionLike(p dbconn.Profile) bool {
	return p.Environment == "production" || p.Criticality == "critical"
}

// buildForecasts projects every limited storage asset and the primary one.
func buildForecasts(p dbconn.Profile, snap collector.Snapshot, ps *profileSeries, now time.Time) []Forecast {
	primary := collector.StorageAssetIndex(snap.Capacity)
	warnPct, critPct, warnDays, critDays := p.Capacity.Thresholds()
	declared, _ := p.Capacity.LimitBytes()
	var out []Forecast
	for i, c := range snap.Capacity {
		isPrimary := i == primary
		if !isPrimary && (c.MaxBytes <= 0 || !volumeScope(c.Scope)) {
			continue
		}
		key := assetKey(c.Scope, c.Name)
		f := Forecast{Asset: key, Scope: c.Scope, Name: c.Name, Primary: isPrimary, UsedBytes: c.UsedBytes, Status: "ok"}
		var s *Series
		if ps != nil {
			s = ps.Assets[key]
		}
		short, long := s.Fit(now, shortWindow), s.Fit(now, longWindow)
		f.GrowthShort, f.GrowthLong = &short, &long
		if c.MaxBytes > 0 {
			f.LimitBytes = c.MaxBytes
			f.UsagePercent = round1(c.UsedBytes / c.MaxBytes * 100)
			f.LimitSource = "engine"
			switch {
			case c.Scope == collector.ScopeVolume:
				f.LimitSource = "host"
			case isPrimary && declared > 0 && float64(declared) == c.MaxBytes:
				f.LimitSource = "declared"
			}
			remaining := c.MaxBytes - c.UsedBytes
			if remaining <= 0 {
				zero := 0.0
				f.DaysToFull, f.Basis = &zero, "now"
			} else {
				for _, fit := range []Fit{short, long} {
					if !fit.Valid || fit.BytesPerDay <= 0 || (fit.Window == shortWindow.name && fit.R2 < shortProjectionMinR2) {
						continue
					}
					days := round1(remaining / fit.BytesPerDay)
					if f.DaysToFull == nil || days < *f.DaysToFull {
						d := days
						f.DaysToFull, f.Basis = &d, fit.Window
					}
				}
			}
			if f.DaysToFull != nil {
				at := now.Add(time.Duration(*f.DaysToFull * 24 * float64(time.Hour))).UTC()
				f.FullAt = &at
			}
			switch {
			case f.UsagePercent >= critPct || f.DaysToFull != nil && *f.DaysToFull <= critDays:
				f.Status = SevCritical
			case f.UsagePercent >= warnPct || f.DaysToFull != nil && *f.DaysToFull <= warnDays:
				f.Status = SevWarning
			}
			if !short.Valid && !long.Valid {
				f.Note = "추세 계산에 필요한 이력을 모으는 중입니다 — " + short.InvalidCause
			}
		} else {
			f.Status = "unknown"
			f.Note = "용량 한도가 선언되지 않아 고갈 시점을 계산할 수 없습니다. DB 프로파일의 capacity.storage_limit 에 볼륨 크기를 입력하세요."
		}
		out = append(out, f)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Primary != out[j].Primary {
			return out[i].Primary
		}
		if Rank(out[i].Status) != Rank(out[j].Status) {
			return Rank(out[i].Status) > Rank(out[j].Status)
		}
		return out[i].UsagePercent > out[j].UsagePercent
	})
	return out
}

func assetLabel(scope, name string) string {
	switch scope {
	case collector.ScopeStorage:
		return "저장공간 전체 점유량"
	case "database":
		return "데이터베이스 " + name
	case "tablespace":
		return "테이블스페이스 " + name
	case collector.ScopeVolume:
		return "디스크 " + name
	}
	return scope + " " + name
}

// footprintBreakdown names what occupies the volume, which usually points
// straight at the cause ("WAL 180 GiB" means a slot or the archiver).
func footprintBreakdown(snap collector.Snapshot) string {
	labels := []struct{ scope, label string }{{collector.ScopeCluster, "데이터"}, {collector.ScopeWAL, "WAL"}, {collector.ScopeTemp, "임시파일"}, {collector.ScopeLog, "로그"}}
	var parts []string
	for _, l := range labels {
		for _, c := range snap.Capacity {
			if c.Scope == l.scope {
				label := l.label
				if c.Name == "binlog" {
					label = "binlog"
				}
				parts = append(parts, label+" "+humanBytes(c.UsedBytes))
			}
		}
	}
	return strings.Join(parts, " · ")
}

const capacityAdvice = "WAL(MySQL은 binlog) 비중이 크면 예방 점검의 복제 슬롯·WAL 아카이브 항목이나 binlog 보존 기간(binlog_expire_logs_seconds)을 먼저 확인하세요. 데이터가 원인이면 급증한 테이블을 정리(보관 정책·파티션 분리)하고, 추세가 정상이라면 고갈 예상일 전에 볼륨을 증설하세요."

func capacityConditions(p dbconn.Profile, snap collector.Snapshot, forecasts []Forecast) []Condition {
	warnPct, critPct, warnDays, critDays := p.Capacity.Thresholds()
	breakdown := footprintBreakdown(snap)
	var out []Condition
	var primary *Forecast
	for i := range forecasts {
		f := forecasts[i]
		if f.Primary {
			primary = &forecasts[i]
		}
		label := assetLabel(f.Scope, f.Name)
		detailTail := ""
		if f.Primary && breakdown != "" {
			detailTail = " · 구성: " + breakdown
		}
		source := "엔진 보고 한도"
		switch f.LimitSource {
		case "declared":
			source = "선언 한도"
		case "host":
			source = "디스크 실측(df) 한도"
		}
		check := CheckCapacity
		if f.Scope == collector.ScopeVolume {
			check = CheckHostDisk
		}
		if f.LimitBytes > 0 {
			if sev := thresholdSeverity(f.UsagePercent, warnPct, critPct); sev != "" {
				out = append(out, Condition{
					Key: RuleCapacityUsage + ":" + f.Asset, Check: check, Rule: RuleCapacityUsage, Severity: sev, Object: f.Asset,
					Title:          fmt.Sprintf("%s 사용률 %.1f%%", label, f.UsagePercent),
					Detail:         fmt.Sprintf("사용 %s / %s %s%s", humanBytes(f.UsedBytes), source, humanBytes(f.LimitBytes), detailTail),
					Recommendation: capacityAdvice, Value: f.UsagePercent, Threshold: warnPct,
				})
			}
			if f.DaysToFull != nil {
				days := *f.DaysToFull
				sev := ""
				switch {
				case days <= critDays:
					sev = SevCritical
				case days <= warnDays:
					sev = SevWarning
				}
				if sev != "" {
					title := fmt.Sprintf("%s 고갈 예측: 약 %s 후 가득 참", label, humanDays(days))
					detail := fmt.Sprintf("남은 공간 %s", humanBytes(math.Max(0, f.LimitBytes-f.UsedBytes)))
					if f.Basis == "now" {
						title = fmt.Sprintf("%s %s 도달 (%.1f%%)", label, source, f.UsagePercent)
						detail = fmt.Sprintf("사용 %s / %s %s", humanBytes(f.UsedBytes), source, humanBytes(f.LimitBytes))
					} else {
						fit := f.GrowthLong
						if f.Basis == shortWindow.name {
							fit = f.GrowthShort
						}
						if fit != nil && fit.Valid {
							detail += fmt.Sprintf(" · 증가 %s/일 (최근 %s 추세, R² %.2f)", humanBytes(fit.BytesPerDay), fit.Window, fit.R2)
						}
						if f.FullAt != nil {
							detail += " · 예상 시점 " + f.FullAt.In(displayLocation()).Format("2006-01-02 15:04 MST")
						}
					}
					out = append(out, Condition{
						Key: RuleCapacityForecast + ":" + f.Asset, Check: check, Rule: RuleCapacityForecast, Severity: sev, Object: f.Asset,
						Title: title, Detail: detail + detailTail,
						Recommendation: capacityAdvice, Value: days, Threshold: warnDays,
						Attributes: map[string]any{"days_to_full": days, "basis": f.Basis},
					})
				}
			}
		}
	}
	if primary != nil {
		f := *primary
		if f.LimitBytes == 0 {
			out = append(out, Condition{
				Key: RuleLimitUndeclared, Check: CheckCapacity, Rule: RuleLimitUndeclared, Severity: SevInfo, Object: f.Asset,
				Title:          "용량 한도 미선언 — 고갈 예측 불가",
				Detail:         fmt.Sprintf("현재 %s 사용 중. %s 은(는) SQL로 디스크 여유 공간을 알려주지 않으므로 볼륨 크기를 선언해야 고갈 시점을 계산합니다.", humanBytes(f.UsedBytes), p.Type),
				Recommendation: "DB 프로파일 설정의 \"저장공간 한도\" 에 데이터 볼륨 크기(예: 500GiB)를 입력하세요.",
			})
		}
		short, long := f.GrowthShort, f.GrowthLong
		if short != nil && long != nil && short.Valid && long.Valid && short.R2 >= surgeMinR2 &&
			short.BytesPerDay >= surgeFactor*math.Max(long.BytesPerDay, 0) &&
			short.BytesPerDay >= math.Max(surgeMinBytesPerDay, surgeMinFraction*f.UsedBytes) {
			out = append(out, Condition{
				Key: RuleGrowthSurge + ":" + f.Asset, Check: CheckCapacity, Rule: RuleGrowthSurge, Severity: SevWarning, Object: f.Asset,
				Title:          fmt.Sprintf("저장공간 증가 속도 급증: %s/일", humanBytes(short.BytesPerDay)),
				Detail:         fmt.Sprintf("최근 6시간 증가 속도가 7일 추세(%s/일)의 %.1f배입니다%s", humanBytes(long.BytesPerDay), short.BytesPerDay/math.Max(long.BytesPerDay, 1), prefixed(" · 구성: ", breakdown)),
				Recommendation: "어떤 구성요소가 늘고 있는지 확인하세요. WAL이면 슬롯·아카이브, 데이터면 급증 테이블 경보를, 임시파일이면 대형 정렬·해시 쿼리를 보세요.",
				Value:          short.BytesPerDay, Threshold: surgeFactor * math.Max(long.BytesPerDay, 0),
			})
		}
	}
	limit := 0.0
	if primary != nil {
		limit = primary.LimitBytes
	}
	for _, c := range snap.Capacity {
		if c.Scope != collector.ScopeTemp {
			continue
		}
		threshold := math.Max(tempSpillMinBytes, tempSpillFraction*limit)
		if c.UsedBytes >= threshold {
			out = append(out, Condition{
				Key: RuleTempSpill, Check: CheckCapacity, Rule: RuleTempSpill, Severity: SevWarning, Object: assetKey(c.Scope, c.Name),
				Title:          fmt.Sprintf("임시 파일 %s 사용 중", humanBytes(c.UsedBytes)),
				Detail:         "메모리(work_mem)를 넘친 정렬·해시 쿼리가 디스크에 임시 파일을 쓰고 있습니다. 쿼리가 끝나면 사라지지만 그동안 볼륨을 차지합니다.",
				Recommendation: "세션 화면에서 오래 실행 중인 대형 쿼리를 확인하세요. temp_file_limit 로 쿼리당 상한을 두면 한 쿼리가 디스크를 채우는 일을 막습니다.",
				Value:          c.UsedBytes, Threshold: threshold,
			})
		}
	}
	var gaps []string
	for _, e := range snap.Evidence {
		if e.Code == "STORAGE_FOOTPRINT_PARTIAL" {
			gaps = append(gaps, e.Summary)
		}
	}
	if len(gaps) > 0 {
		out = append(out, Condition{
			Key: RuleMonitorPrivilege, Check: CheckCapacity, Rule: RuleMonitorPrivilege, Severity: SevInfo, Object: "privilege",
			Title: fmt.Sprintf("모니터링 권한 부족 — 저장공간 일부 미측정 (%d건)", len(gaps)), Detail: strings.Join(gaps, "\n"),
			Recommendation: "위 GRANT 를 모니터링 계정에 적용하세요 — 읽기 전용 모니터링 권한입니다.",
		})
	}
	return out
}

func thresholdSeverity(v, warn, crit float64) string {
	switch {
	case v >= crit:
		return SevCritical
	case v >= warn:
		return SevWarning
	}
	return ""
}

// tableConditions compares each large table's last 24 hours with its own
// history: growth far beyond its usual daily rate (a runaway job, a log table
// without retention, CREATE TABLE AS copies) and sudden shrinkage (TRUNCATE,
// a mistaken mass delete followed by a rewrite).
func tableConditions(snap collector.Snapshot, ps *profileSeries, now time.Time) []Condition {
	if ps == nil {
		return nil
	}
	var out []Condition
	for _, c := range snap.Capacity {
		if c.Scope != "table" {
			continue
		}
		s := ps.Assets[assetKey(c.Scope, c.Name)]
		cur := c.UsedBytes
		if prev, ok := s.At(now.Add(-24*time.Hour), 2*time.Hour); ok {
			growth := cur - prev.V
			if base, ok := s.EarliestBetween(now.Add(-seriesRetention), now.Add(-48*time.Hour)); ok && prev.T > base.T {
				days := float64(prev.T-base.T) / 86400
				daily := (prev.V - base.V) / days
				threshold := math.Max(tableSurgeMinBytes, tableSurgeFactor*math.Max(daily, 0))
				if growth >= threshold {
					out = append(out, Condition{
						Key: RuleTableSurge + ":" + c.Name, Check: CheckTables, Rule: RuleTableSurge, Severity: SevWarning, Object: c.Name,
						Title:          fmt.Sprintf("테이블 급증: %s 24시간 +%s", c.Name, humanBytes(growth)),
						Detail:         fmt.Sprintf("현재 %s. 평소 하루 증가량 %s 의 %.1f배입니다.", humanBytes(cur), humanBytes(math.Max(daily, 0)), growth/math.Max(daily, 1)),
						Recommendation: "적재 배치의 중복 실행·무한 루프, 보관 정책이 없는 로그·이력 테이블, 백업용 복사 테이블(CREATE TABLE AS) 여부를 확인하세요.",
						Value:          growth, Threshold: threshold,
					})
				}
			}
		}
		if peak, ok := s.MaxSince(now.Add(-24 * time.Hour)); ok {
			drop := peak.V - cur
			if drop >= tableShrinkMin && drop >= tableShrinkRatio*peak.V {
				out = append(out, Condition{
					Key: RuleTableShrink + ":" + c.Name, Check: CheckTables, Rule: RuleTableShrink, Severity: SevWarning, Object: c.Name,
					Title:          fmt.Sprintf("테이블 급감: %s %s → %s", c.Name, humanBytes(peak.V), humanBytes(cur)),
					Detail:         "24시간 안에 크기가 절반 이하로 줄었습니다. TRUNCATE, 대량 삭제 후 VACUUM FULL/pg_repack, 파티션 분리가 아니라면 데이터 유실을 의심해야 합니다.",
					Recommendation: "변경계획·배치 이력과 대조해 의도된 정리인지 확인하세요.",
					Value:          drop, Threshold: tableShrinkMin, QuietResolve: true,
				})
			}
		}
	}
	return out
}

// collectionConditions turns repeated collection failures into an alert —
// while SQLON cannot see a database, none of the other warnings can fire.
func collectionConditions(p dbconn.Profile, result collector.ProfileResult, failures, after int) []Condition {
	if failures < after {
		return nil
	}
	sev := SevWarning
	if productionLike(p) {
		sev = SevCritical
	}
	msg := strings.Join(strings.Fields(result.Error), " ")
	if msg == "" {
		msg = result.ErrorCode
	}
	if r := []rune(msg); len(r) > 240 {
		msg = string(r[:240]) + "…"
	}
	return []Condition{{
		Key: RuleCollectionDown, Check: CheckCollection, Rule: RuleCollectionDown, Severity: sev, Object: "collector",
		Title:          fmt.Sprintf("관측 중단: %d회 연속 수집 실패", failures),
		Detail:         fmt.Sprintf("이 DB는 지금 예방 경보가 동작하지 않습니다. 마지막 오류: [%s] %s", result.ErrorCode, msg),
		Recommendation: "DB 접속(네트워크·계정·비밀번호 참조)과 DB 서버 상태를 확인하세요. 디스크가 가득 차 DB가 멈춘 경우에도 이 경보가 납니다.",
		Value:          float64(failures), Threshold: float64(after),
	}}
}

var maintenanceTitles = map[string]string{
	"wraparound":            "트랜잭션 ID wraparound 임박",
	"bloat":                 "테이블 블로트(dead tuple 적체)",
	"replication_slot":      "복제 슬롯이 WAL을 붙잡고 있음",
	"wal_archive":           "WAL 아카이브 실패·적체",
	"wal_retention":         "pg_wal 과다 보존",
	"vacuum_blocker":        "VACUUM 을 막는 장기 트랜잭션",
	"config_risk":           "디스크 보호 설정 위험",
	"undo_purge_lag":        "InnoDB 퍼지 적체",
	"no_primary_key":        "PK 없는 테이블",
	"tablespace_saturation": "테이블스페이스 포화",
	"expired_account":       "만료 미잠금 계정",
}

func maintenanceConditions(findings []observability.MaintenanceFinding) []Condition {
	out := make([]Condition, 0, len(findings))
	for _, f := range findings {
		title := maintenanceTitles[f.Category]
		if title == "" {
			title = f.Category
		}
		rule := RuleMaintenance + f.Category
		out = append(out, Condition{
			Key: rule + ":" + f.Object, Check: CheckMaintenance, Rule: rule, Severity: f.Severity, Object: f.Object,
			Title: title + ": " + f.Object, Detail: f.Detail, Recommendation: f.Recommendation,
			Value: f.Value, Threshold: f.Threshold,
		})
	}
	return out
}

// supersededCollectorAlerts are the collector alert engine's capacity rules,
// which early-warning replaces with limit-aware, regression-based ones.
var supersededCollectorAlerts = map[string]bool{
	"capacity_saturation": true, "capacity_warning": true, "capacity_critical": true, "capacity_exhaustion_risk": true,
}

func bridgedConditions(alerts []collector.Alert) []Condition {
	var out []Condition
	for _, a := range alerts {
		if supersededCollectorAlerts[a.MetricName] {
			continue
		}
		object := a.MetricName
		key := "workload:" + a.MetricName + ":" + shortHash(a.Message)
		out = append(out, Condition{
			Key: key, Check: CheckWorkload, Rule: a.MetricName, Severity: a.Severity, Object: object,
			Title: a.Message, Value: a.Value, Threshold: a.Threshold, Event: true,
		})
	}
	return out
}

func prefixed(prefix, s string) string {
	if s == "" {
		return ""
	}
	return prefix + s
}

func humanBytes(v float64) string {
	neg := v < 0
	v = math.Abs(v)
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	s := fmt.Sprintf("%.1f %s", v, units[i])
	if i == 0 {
		s = fmt.Sprintf("%.0f %s", v, units[i])
	}
	if neg {
		return "-" + s
	}
	return s
}

func humanDays(days float64) string {
	switch {
	case days < 1.0/24:
		return fmt.Sprintf("%.0f분", math.Max(days*24*60, 0))
	case days < 2:
		return fmt.Sprintf("%.1f시간", days*24)
	default:
		return fmt.Sprintf("%.1f일", days)
	}
}
