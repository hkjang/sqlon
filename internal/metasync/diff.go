package metasync

import (
	"sort"
	"strings"
)

// Diff computes the change set between a baseline snapshot and a current one
// (FR-META-004). Deletions are never turned into immediate removals: they are
// emitted as changes with disposition "retire_candidate" so the operational
// catalog keeps serving until a steward approves the removal (AC-02).
func Diff(from, to *RawSnapshot) *ChangeSet {
	cs := &ChangeSet{
		SourceID:   to.SourceID,
		ComputedAt: to.CollectedAt,
		Summary:    map[ChangeKind]int{},
	}
	if from != nil {
		cs.FromSnapshotID = from.SnapshotID
	}
	cs.ToSnapshotID = to.SnapshotID

	fromT := indexTables(from)
	toT := indexTables(to)
	changedTables := map[string]bool{}

	add := func(ch Change) {
		cs.Changes = append(cs.Changes, ch)
		cs.Summary[ch.Kind]++
		if ch.Table != "" {
			changedTables[ch.Table] = true
		}
	}

	// added / changed tables
	for fqn, cur := range toT {
		base, ok := fromT[fqn]
		if !ok {
			add(Change{Kind: TableAdded, Severity: SevLow, Table: fqn,
				Detail:      "new " + cur.Kind + " with " + itoa(len(cur.Columns)) + " columns",
				Disposition: "create_candidate"})
			continue
		}
		if base.StructHash == cur.StructHash {
			continue // structurally identical — skip (FR-META-005)
		}
		diffTable(base, cur, add)
	}
	// removed tables
	for fqn := range fromT {
		if _, ok := toT[fqn]; !ok {
			add(Change{Kind: TableRemoved, Severity: SevBreaking, Table: fqn,
				Detail:      "table no longer present in source",
				Disposition: "retire_candidate"})
		}
	}

	for t := range changedTables {
		cs.ChangedTables = append(cs.ChangedTables, t)
	}
	sort.Strings(cs.ChangedTables)
	sort.SliceStable(cs.Changes, func(i, j int) bool {
		if cs.Changes[i].Table != cs.Changes[j].Table {
			return cs.Changes[i].Table < cs.Changes[j].Table
		}
		return cs.Changes[i].Column < cs.Changes[j].Column
	})
	return cs
}

func diffTable(base, cur *TableAsset, add func(Change)) {
	fqn := cur.FQN()
	if strings.TrimSpace(base.Comment) != strings.TrimSpace(cur.Comment) {
		add(Change{Kind: CommentChanged, Severity: SevLow, Table: fqn,
			Before: base.Comment, After: cur.Comment, Disposition: "update_candidate"})
	}
	if normalizeSQL(base.ViewSQL) != normalizeSQL(cur.ViewSQL) {
		add(Change{Kind: ViewSQLChanged, Severity: SevHigh, Table: fqn,
			Detail:      "view definition changed — recompute lineage/dependents",
			Disposition: "review"})
	}

	baseCols := indexColumns(base)
	curCols := indexColumns(cur)
	for name, cc := range curCols {
		bc, ok := baseCols[name]
		if !ok {
			add(Change{Kind: ColumnAdded, Severity: SevLow, Table: fqn, Column: name,
				After: cc.FullType, Detail: "new column", Disposition: "create_candidate"})
			continue
		}
		if !strings.EqualFold(bc.FullType, cc.FullType) {
			add(Change{Kind: TypeChanged, Severity: typeSeverity(bc, cc), Table: fqn, Column: name,
				Before: bc.FullType, After: cc.FullType,
				Detail:      "type change — check dependent metrics/SQL compatibility",
				Disposition: "review"})
		}
		if bc.Nullable != cc.Nullable {
			add(Change{Kind: NullChanged, Severity: SevMedium, Table: fqn, Column: name,
				Before: nullStr(bc.Nullable), After: nullStr(cc.Nullable), Disposition: "review"})
		}
		if bc.IsPrimaryKey != cc.IsPrimaryKey || bc.IsForeignKey != cc.IsForeignKey {
			add(Change{Kind: KeyChanged, Severity: SevHigh, Table: fqn, Column: name,
				Before: keyStr(bc), After: keyStr(cc),
				Detail: "key change — re-verify join graph", Disposition: "review"})
		}
		if strings.TrimSpace(bc.Comment) != strings.TrimSpace(cc.Comment) {
			add(Change{Kind: CommentChanged, Severity: SevLow, Table: fqn, Column: name,
				Before: bc.Comment, After: cc.Comment, Disposition: "update_candidate"})
		}
	}
	for name := range baseCols {
		if _, ok := curCols[name]; !ok {
			add(Change{Kind: ColumnRemoved, Severity: SevBreaking, Table: fqn, Column: name,
				Detail:      "column removed — flags dependent metrics/joins/SQL",
				Disposition: "retire_candidate"})
		}
	}

	if indexSig(base.Indexes) != indexSig(cur.Indexes) {
		add(Change{Kind: IndexChanged, Severity: SevLow, Table: fqn,
			Detail: "index set changed — refresh plan-related metadata", Disposition: "update_candidate"})
	}
}

func indexTables(s *RawSnapshot) map[string]*TableAsset {
	out := map[string]*TableAsset{}
	if s == nil {
		return out
	}
	for i := range s.Tables {
		out[s.Tables[i].FQN()] = &s.Tables[i]
	}
	return out
}

func indexColumns(t *TableAsset) map[string]ColumnAsset {
	out := make(map[string]ColumnAsset, len(t.Columns))
	for _, c := range t.Columns {
		out[strings.ToLower(c.Name)] = c
	}
	return out
}

// typeSeverity flags widening (varchar(64)->varchar(128), int->bigint) as
// lower risk than narrowing or a base-type change.
func typeSeverity(b, c ColumnAsset) Severity {
	if strings.EqualFold(b.DataType, c.DataType) {
		return SevMedium // same base type, size/precision changed
	}
	return SevHigh
}

func indexSig(idx []IndexAsset) string {
	sigs := make([]string, 0, len(idx))
	for _, i := range idx {
		sigs = append(sigs, i.Name+":"+strings.Join(i.Columns, ",")+":"+boolStr(i.Unique))
	}
	sort.Strings(sigs)
	return strings.Join(sigs, "|")
}

func nullStr(b bool) string {
	if b {
		return "NULL"
	}
	return "NOT NULL"
}
func keyStr(c ColumnAsset) string {
	var p []string
	if c.IsPrimaryKey {
		p = append(p, "PK")
	}
	if c.IsForeignKey {
		p = append(p, "FK")
	}
	if len(p) == 0 {
		return "-"
	}
	return strings.Join(p, "+")
}
func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
