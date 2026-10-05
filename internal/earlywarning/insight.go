package earlywarning

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"sqlon/internal/dbconn"
)

// ---- capacity planning ----

// CapacityPlan answers "how big must this volume be to last N more days,
// and by when must it change" for every storage asset of a database.
type CapacityPlan struct {
	ProfileID   string      `json:"profile_id"`
	TargetDays  float64     `json:"target_days"`
	GeneratedAt time.Time   `json:"generated_at"`
	Assets      []AssetPlan `json:"assets"`
	Summary     string      `json:"summary"`
}

type AssetPlan struct {
	Asset        string  `json:"asset"`
	Label        string  `json:"label"`
	UsedBytes    float64 `json:"used_bytes"`
	LimitBytes   float64 `json:"limit_bytes,omitempty"`
	LimitSource  string  `json:"limit_source,omitempty"`
	GrowthPerDay float64 `json:"growth_bytes_per_day"`
	GrowthBasis  string  `json:"growth_basis"` // 7d | 6h | none
	// NeededBytes is the usage expected at the horizon; RecommendedBytes
	// keeps that usage under the warning threshold.
	NeededBytes      float64    `json:"needed_bytes"`
	RecommendedBytes float64    `json:"recommended_bytes"`
	ShortfallBytes   float64    `json:"shortfall_bytes"`
	WarnCrossAt      *time.Time `json:"warn_cross_at,omitempty"` // usage reaches the warning threshold
	FullAt           *time.Time `json:"full_at,omitempty"`
	Verdict          string     `json:"verdict"` // ok | resize | unknown
	Advice           string     `json:"advice"`
	Scenarios        []Scenario `json:"scenarios"`
}

type Scenario struct {
	Name         string   `json:"name"`
	GrowthPerDay float64  `json:"growth_bytes_per_day"`
	DaysToFull   *float64 `json:"days_to_full,omitempty"`
}

// PlanCapacity builds the plan from the latest forecasts.
func (e *Engine) PlanCapacity(p dbconn.Profile, targetDays float64) CapacityPlan {
	if targetDays <= 0 {
		targetDays = 90
	}
	e.mu.Lock()
	fcs := append(append([]Forecast{}, e.volumes[p.ID]...), e.forecasts[p.ID]...)
	now := e.now()
	e.mu.Unlock()
	warnPct, _, _, _ := p.Capacity.Thresholds()
	plan := CapacityPlan{ProfileID: p.ID, TargetDays: targetDays, GeneratedAt: now, Assets: []AssetPlan{}}
	var resize, unknown []string
	for _, f := range fcs {
		if !f.Primary && f.LimitBytes <= 0 {
			continue
		}
		a := AssetPlan{Asset: f.Asset, Label: assetLabel(f.Scope, f.Name), UsedBytes: f.UsedBytes, LimitBytes: f.LimitBytes, LimitSource: f.LimitSource, GrowthBasis: "none", Verdict: "unknown"}
		switch {
		case f.GrowthLong != nil && f.GrowthLong.Valid:
			a.GrowthPerDay, a.GrowthBasis = f.GrowthLong.BytesPerDay, f.GrowthLong.Window
		case f.GrowthShort != nil && f.GrowthShort.Valid:
			a.GrowthPerDay, a.GrowthBasis = f.GrowthShort.BytesPerDay, f.GrowthShort.Window
		}
		growth := math.Max(a.GrowthPerDay, 0)
		a.NeededBytes = a.UsedBytes + growth*targetDays
		a.RecommendedBytes = math.Ceil(a.NeededBytes/(warnPct/100)/gib) * gib
		for _, sc := range []struct {
			name string
			g    float64
		}{{"현재 추세", growth}, {"추세 2배", growth * 2}, {"추세 3배 (급증)", growth * 3}} {
			s := Scenario{Name: sc.name, GrowthPerDay: sc.g}
			if a.LimitBytes > 0 && sc.g > 0 {
				d := round1(math.Max(0, a.LimitBytes-a.UsedBytes) / sc.g)
				s.DaysToFull = &d
			}
			a.Scenarios = append(a.Scenarios, s)
		}
		overWarn := a.LimitBytes > 0 && a.UsedBytes >= a.LimitBytes*warnPct/100
		switch {
		case overWarn:
			// already past the warning threshold: no trend is needed to know
			a.ShortfallBytes = math.Max(0, a.RecommendedBytes-a.LimitBytes)
			a.Verdict = "resize"
			a.Advice = fmt.Sprintf("이미 경고 임계(%.0f%%)를 넘었습니다 (%.1f%%). 지금 원인(WAL·슬롯·급증 테이블)을 줄이거나 %s 이상으로 증설하세요.", warnPct, a.UsedBytes/a.LimitBytes*100, humanBytes(a.RecommendedBytes))
			resize = append(resize, a.Label)
		case a.GrowthBasis == "none":
			a.Advice = "추세를 계산할 이력이 아직 없습니다 (6시간 추세는 2시간, 7일 추세는 24시간 이상 수집 후)."
			unknown = append(unknown, a.Label)
		case a.LimitBytes <= 0:
			a.Advice = fmt.Sprintf("한도가 없어 시점은 계산할 수 없습니다. %0.f일 뒤 예상 사용량은 %s 이므로 볼륨은 %s 이상이어야 합니다.", targetDays, humanBytes(a.NeededBytes), humanBytes(a.RecommendedBytes))
		default:
			if growth > 0 {
				full := now.Add(time.Duration(math.Max(0, a.LimitBytes-a.UsedBytes) / growth * 24 * float64(time.Hour)))
				a.FullAt = &full
				warnAt := now.Add(time.Duration(math.Max(0, a.LimitBytes*warnPct/100-a.UsedBytes) / growth * 24 * float64(time.Hour)))
				a.WarnCrossAt = &warnAt
			}
			a.ShortfallBytes = math.Max(0, a.RecommendedBytes-a.LimitBytes)
			if a.ShortfallBytes > 0 {
				a.Verdict = "resize"
				when := "지금"
				if a.WarnCrossAt != nil && a.WarnCrossAt.After(now) {
					when = a.WarnCrossAt.In(displayLocation()).Format("2006-01-02") + " 전"
				}
				a.Advice = fmt.Sprintf("%0.f일을 버티려면 %s 이상(현재 %s, %s 부족)이 필요합니다. 경고 임계(%.0f%%) 도달 %s에 증설하거나 증가 원인을 줄이세요.", targetDays, humanBytes(a.RecommendedBytes), humanBytes(a.LimitBytes), humanBytes(a.ShortfallBytes), warnPct, when)
				resize = append(resize, a.Label)
			} else {
				a.Verdict = "ok"
				a.Advice = fmt.Sprintf("현재 추세라면 %0.f일 동안 경고 임계(%.0f%%) 아래입니다.", targetDays, warnPct)
			}
		}
		plan.Assets = append(plan.Assets, a)
	}
	switch {
	case len(plan.Assets) == 0:
		plan.Summary = "용량 데이터가 아직 없습니다."
	case len(resize) > 0:
		plan.Summary = fmt.Sprintf("%0.f일 안에 증설이 필요한 자산: %s", targetDays, strings.Join(resize, ", "))
	case len(unknown) > 0:
		plan.Summary = "아직 추세를 계산할 수 없는 자산: " + strings.Join(unknown, ", ") + " — 이력이 쌓인 뒤 다시 계산하세요."
	default:
		plan.Summary = fmt.Sprintf("%0.f일 동안 증설이 필요한 자산이 없습니다.", targetDays)
	}
	return plan
}

// ---- alert drill-down ----

// AlertDetail is everything known about one alert: its incident, what
// else is firing on the database, how often it happened before, and for
// storage alerts the forecast and recent series.
type AlertDetail struct {
	Alert    Alert     `json:"alert"`
	Incident *Incident `json:"incident,omitempty"`
	Related  []Alert   `json:"related"`
	Past     []Alert   `json:"past"` // earlier occurrences of the same key
	Forecast *Forecast `json:"forecast,omitempty"`
	Series   []Point   `json:"series,omitempty"` // [unix, bytes], ≤ 200 points
	Silenced *Silence  `json:"silenced,omitempty"`
	Flapping bool      `json:"flapping"`
}

// Explain returns the detail of one alert.
func (e *Engine) Explain(id string) (AlertDetail, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	var target *Alert
	for _, a := range e.st.Alerts {
		if a.ID == id {
			target = a
		}
	}
	if target == nil {
		return AlertDetail{}, false
	}
	d := AlertDetail{Alert: *target, Related: []Alert{}, Past: []Alert{}}
	var firing []Alert
	for _, a := range e.st.Alerts {
		switch {
		case a.State == StateFiring:
			firing = append(firing, *a)
			if a.ProfileID == target.ProfileID && a.ID != target.ID {
				d.Related = append(d.Related, *a)
			}
		case a.Key == target.Key && a.ID != target.ID:
			d.Past = append(d.Past, *a)
		}
	}
	sort.SliceStable(d.Past, func(i, j int) bool { return d.Past[i].FirstSeen.After(d.Past[j].FirstSeen) })
	if len(d.Past) > 10 {
		d.Past = d.Past[:10]
	}
	for _, inc := range correlate(firing) {
		for _, aid := range inc.AlertIDs {
			if aid == target.ID {
				c := inc
				d.Incident = &c
			}
		}
	}
	for _, s := range e.activeSilencesLocked(now) {
		if s.matches(target, now) {
			c := s
			d.Silenced = &c
		}
	}
	d.Flapping = e.st.flapCounts(now)[target.Key] >= flapThreshold
	for _, f := range append(append([]Forecast{}, e.volumes[target.ProfileID]...), e.forecasts[target.ProfileID]...) {
		if f.Asset == target.Object {
			c := f
			d.Forecast = &c
			if ps := e.series[target.ProfileID]; ps != nil {
				if s := ps.Assets[f.Asset]; s != nil {
					pts := s.Points
					if len(pts) > 200 {
						pts = pts[len(pts)-200:]
					}
					d.Series = append([]Point{}, pts...)
				}
			}
		}
	}
	return d, true
}

// Incidents returns the current incidents for the given profiles.
func (e *Engine) Incidents(profileIDs map[string]bool) []Incident {
	e.mu.Lock()
	defer e.mu.Unlock()
	var firing []Alert
	for _, a := range e.st.Alerts {
		if a.State == StateFiring && (profileIDs == nil || profileIDs[a.ProfileID]) {
			firing = append(firing, *a)
		}
	}
	return correlate(firing)
}
