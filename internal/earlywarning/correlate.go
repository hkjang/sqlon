package earlywarning

import (
	"sort"
	"strings"
)

// Alerts that fire together are often one problem seen from several
// angles: a failing archive_command keeps WAL, pg_wal outgrows its budget,
// the volume's forecast collapses, and finally the database stops. Grouping
// them by known cause → effect relations points the operator at the one
// thing to fix and lets notifications name the probable cause.

// effects lists, per rule, the rules it can cause.
var effects = map[string][]string{
	"maint_wal_archive":      {"maint_wal_retention", RuleCapacityForecast, RuleCapacityUsage, RuleGrowthSurge},
	"maint_replication_slot": {"maint_wal_retention", RuleCapacityForecast, RuleCapacityUsage, RuleGrowthSurge},
	"maint_wal_retention":    {RuleCapacityForecast, RuleCapacityUsage, RuleGrowthSurge},
	"maint_vacuum_blocker":   {"maint_bloat", "maint_wraparound", RuleTableSurge, RuleCapacityForecast, RuleCapacityUsage, RuleGrowthSurge},
	"maint_bloat":            {RuleCapacityForecast, RuleCapacityUsage, RuleGrowthSurge},
	RuleTempSpill:            {RuleCapacityForecast, RuleCapacityUsage, RuleGrowthSurge},
	RuleTableSurge:           {RuleCapacityForecast, RuleCapacityUsage, RuleGrowthSurge},
	RuleGrowthSurge:          {RuleCapacityForecast, RuleCapacityUsage},
	RuleCapacityUsage:        {RuleCollectionDown},
	RuleCapacityForecast:     {RuleCollectionDown},
}

// configEffects: a configuration risk causes symptoms only through the
// setting it is about. autovacuum=off lets dead rows pile up; an unlimited
// max_slot_wal_keep_size is a latent risk — the slot that pins WAL is the
// cause, not the setting that failed to stop it.
var configEffects = map[string][]string{
	"autovacuum": {"maint_bloat", "maint_wraparound", RuleTableSurge, RuleCapacityForecast, RuleCapacityUsage, RuleGrowthSurge},
}

func causes(cause, effect *Alert) bool {
	if cause.ProfileID != effect.ProfileID || cause.Key == effect.Key {
		return false
	}
	list := effects[cause.Rule]
	if cause.Rule == "maint_config_risk" {
		list = configEffects[cause.Object]
	}
	for _, r := range list {
		if r == effect.Rule {
			// only a volume that is actually full stops the database
			if effect.Rule == RuleCollectionDown {
				return cause.Rule == RuleCapacityUsage && cause.Value >= 98 || cause.Rule == RuleCapacityForecast && cause.Value <= 0.05
			}
			return true
		}
	}
	return false
}

// Incident is a group of firing alerts on one database linked by cause and
// effect. Roots have no firing cause inside the group.
type Incident struct {
	ProfileID string   `json:"profile_id"`
	Severity  string   `json:"severity"`
	Summary   string   `json:"summary"`
	RootIDs   []string `json:"root_ids"`
	AlertIDs  []string `json:"alert_ids"`
	Roots     []string `json:"roots"` // titles
}

// correlate groups firing alerts into incidents of two or more.
func correlate(firing []Alert) []Incident {
	n := len(firing)
	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	hasCause := make([]bool, n)
	linked := make([]bool, n)
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			if i != j && causes(&firing[i], &firing[j]) {
				parent[find(i)] = find(j)
				hasCause[j], linked[i], linked[j] = true, true, true
			}
		}
	}
	groups := map[int][]int{}
	for i := 0; i < n; i++ {
		if linked[i] {
			groups[find(i)] = append(groups[find(i)], i)
		}
	}
	var out []Incident
	for _, members := range groups {
		inc := Incident{ProfileID: firing[members[0]].ProfileID}
		sort.SliceStable(members, func(a, b int) bool { return chainOrder(firing[members[a]].Rule) < chainOrder(firing[members[b]].Rule) })
		var chain []string
		seen := map[string]bool{}
		for _, i := range members {
			a := firing[i]
			inc.AlertIDs = append(inc.AlertIDs, a.ID)
			if Rank(a.Severity) > Rank(inc.Severity) {
				inc.Severity = a.Severity
			}
			if !hasCause[i] {
				inc.RootIDs = append(inc.RootIDs, a.ID)
				inc.Roots = append(inc.Roots, a.Title)
			}
			label := ruleLabel(a.Rule)
			if !seen[label] {
				seen[label] = true
				chain = append(chain, label)
			}
		}
		inc.Summary = "원인 추정: " + strings.Join(chain, " → ")
		out = append(out, inc)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if Rank(out[i].Severity) != Rank(out[j].Severity) {
			return Rank(out[i].Severity) > Rank(out[j].Severity)
		}
		return out[i].ProfileID < out[j].ProfileID
	})
	return out
}

// chainOrder places causes before effects in summaries.
func chainOrder(rule string) int {
	order := []string{"maint_config_risk", "maint_vacuum_blocker", "maint_wal_archive", "maint_replication_slot", RuleTempSpill, RuleTableSurge, "maint_bloat", "maint_wraparound", "maint_wal_retention", RuleGrowthSurge, RuleCapacityForecast, RuleCapacityUsage, RuleCollectionDown}
	for i, r := range order {
		if r == rule {
			return i
		}
	}
	return len(order)
}

func ruleLabel(rule string) string {
	switch rule {
	case "maint_wal_archive":
		return "WAL 아카이브 실패·적체"
	case "maint_replication_slot":
		return "복제 슬롯의 WAL 보존"
	case "maint_wal_retention":
		return "pg_wal 과다"
	case "maint_vacuum_blocker":
		return "VACUUM 차단 트랜잭션"
	case "maint_bloat":
		return "테이블 블로트"
	case "maint_wraparound":
		return "wraparound 임박"
	case "maint_config_risk":
		return "디스크 보호 설정 위험"
	case RuleTempSpill:
		return "임시 파일 폭증"
	case RuleTableSurge:
		return "테이블 급증"
	case RuleGrowthSurge:
		return "증가 속도 급증"
	case RuleCapacityForecast:
		return "고갈 예측"
	case RuleCapacityUsage:
		return "사용률 임계 초과"
	case RuleCollectionDown:
		return "DB 정지(관측 중단)"
	}
	return rule
}

// causeOf maps each symptom alert ID to its incident's summary.
func causeOf(incidents []Incident) map[string]string {
	out := map[string]string{}
	for _, inc := range incidents {
		roots := map[string]bool{}
		for _, id := range inc.RootIDs {
			roots[id] = true
		}
		for _, id := range inc.AlertIDs {
			if !roots[id] {
				out[id] = inc.Summary
			}
		}
	}
	return out
}
