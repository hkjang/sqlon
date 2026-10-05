package earlywarning

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"sqlon/internal/change"
	"sqlon/internal/dbconn"
	"sqlon/internal/metasync"
)

// executedStates are the change-plan states in which DDL may have run.
var executedStates = map[change.State]bool{
	change.Executing: true, change.Verifying: true, change.Completed: true, change.Failed: true,
	change.RollbackRequired: true, change.RollingBack: true, change.RolledBack: true,
}

// planTexts returns the lower-cased targets and step commands of change plans
// that executed against the profile since the given time — the record of DDL
// somebody planned and approved.
func planTexts(plans []change.Plan, profileID string, since time.Time) []string {
	var out []string
	for _, p := range plans {
		if p.ProfileID != profileID || !executedStates[p.State] || p.UpdatedAt.Before(since) {
			continue
		}
		out = append(out, strings.ToLower(p.Target))
		for _, s := range p.Steps {
			out = append(out, strings.ToLower(s.Command))
		}
	}
	return out
}

// planned reports whether a change to the table appears in a plan text, by
// bare table name on identifier boundaries.
func planned(texts []string, table string) bool {
	name := strings.ToLower(table)
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Trim(name, `"`+"`")
	if name == "" {
		return false
	}
	re := regexp.MustCompile(`(^|[^a-z0-9_$])` + regexp.QuoteMeta(name) + `([^a-z0-9_$]|$)`)
	for _, t := range texts {
		if re.MatchString(t) {
			return true
		}
	}
	return false
}

func schemaChangeSeverity(c metasync.Change, isPlanned, production bool) string {
	if isPlanned {
		return SevInfo
	}
	sev := SevInfo
	switch c.Severity {
	case metasync.SevBreaking:
		sev = SevCritical
	case metasync.SevHigh:
		sev = SevWarning
	default:
		if production {
			sev = SevWarning
		}
	}
	if !production && sev == SevCritical {
		sev = SevWarning
	}
	return sev
}

var changeKindKo = map[metasync.ChangeKind]string{
	metasync.TableAdded: "테이블 추가", metasync.TableRemoved: "테이블 삭제",
	metasync.ColumnAdded: "컬럼 추가", metasync.ColumnRemoved: "컬럼 삭제",
	metasync.TypeChanged: "타입 변경", metasync.NullChanged: "NULL 허용 변경",
	metasync.KeyChanged: "키 변경", metasync.IndexChanged: "인덱스 변경",
	metasync.ViewSQLChanged: "뷰 정의 변경",
}

// SchemaChange is one detected DDL effect as shown to operators.
type SchemaChange struct {
	Kind     string  `json:"kind"`
	Table    string  `json:"table"`
	Column   string  `json:"column,omitempty"`
	Before   string  `json:"before,omitempty"`
	After    string  `json:"after,omitempty"`
	Planned  bool    `json:"planned"`
	Severity string  `json:"severity"`
	Bytes    float64 `json:"bytes,omitempty"` // current table size when known
}

// schemaConditions diffs the baseline against the current physical schema
// and reports the result as one event: the changes, which of them no
// executed change plan accounts for, and how severe the worst one is.
func schemaConditions(p dbconn.Profile, base, cur *metasync.RawSnapshot, plans []change.Plan, tableBytes map[string]float64) []Condition {
	if base == nil || cur == nil || base.SchemaHash == cur.SchemaHash {
		return nil
	}
	cs := metasync.Diff(base, cur)
	texts := planTexts(plans, p.ID, base.CollectedAt.Add(-time.Hour))
	production := productionLike(p)
	var changes []SchemaChange
	worst, unplanned := "", 0
	for _, c := range cs.Changes {
		if c.Kind == metasync.CommentChanged {
			continue
		}
		isPlanned := planned(texts, c.Table)
		sev := schemaChangeSeverity(c, isPlanned, production)
		if !isPlanned {
			unplanned++
		}
		if Rank(sev) > Rank(worst) {
			worst = sev
		}
		kind := changeKindKo[c.Kind]
		if kind == "" {
			kind = string(c.Kind)
		}
		changes = append(changes, SchemaChange{Kind: kind, Table: c.Table, Column: c.Column, Before: c.Before, After: c.After, Planned: isPlanned, Severity: sev, Bytes: tableBytes[c.Table]})
	}
	if len(changes) == 0 {
		return nil
	}
	sort.SliceStable(changes, func(i, j int) bool {
		if Rank(changes[i].Severity) != Rank(changes[j].Severity) {
			return Rank(changes[i].Severity) > Rank(changes[j].Severity)
		}
		if changes[i].Table != changes[j].Table {
			return changes[i].Table < changes[j].Table
		}
		return changes[i].Column < changes[j].Column
	})
	var lines []string
	for i, c := range changes {
		if i == 12 {
			lines = append(lines, fmt.Sprintf("… 외 %d건", len(changes)-i))
			break
		}
		tag := "계획 외"
		if c.Planned {
			tag = "변경계획"
		}
		line := fmt.Sprintf("[%s] %s %s", tag, c.Kind, c.Table)
		if c.Column != "" {
			line += "." + c.Column
		}
		if c.Before != "" || c.After != "" {
			line += fmt.Sprintf(" (%s → %s)", dash(c.Before), dash(c.After))
		}
		if c.Bytes > 0 {
			line += " · " + humanBytes(c.Bytes)
		}
		lines = append(lines, line)
	}
	title := fmt.Sprintf("스키마 변경 감지: %d건", len(changes))
	if unplanned > 0 {
		title += fmt.Sprintf(" (변경계획 없는 변경 %d건)", unplanned)
	}
	rec := "변경계획에 없는 DDL은 배포 누락·수동 변경·권한 오남용일 수 있습니다. 변경 주체를 확인하고, 삭제·타입 변경이면 의존 쿼리와 애플리케이션 영향을 점검하세요."
	return []Condition{{
		Key:   RuleSchemaChange + ":" + cur.SchemaHash,
		Check: CheckSchema, Rule: RuleSchemaChange, Severity: worst, Object: "schema",
		Title: title, Detail: strings.Join(lines, "\n"), Recommendation: rec,
		Value: float64(len(changes)), Event: true,
		Attributes: map[string]any{"changes": changes, "unplanned": unplanned, "from_hash": base.SchemaHash, "to_hash": cur.SchemaHash, "baseline_at": base.CollectedAt},
	}}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
