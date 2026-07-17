package catalog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// LearnedRule is a validation/search rule promoted from repeated feedback
// patterns. Rules live in data/<set>/learned_rules.json so operators can
// review, edit, or delete them; they are applied on load and after
// learn_from_feedback runs.
type LearnedRule struct {
	Type              string `json:"type"` // recurring_error | table_correction | column_correction
	Code              string `json:"code,omitempty"`
	Table             string `json:"table,omitempty"`
	Column            string `json:"column,omitempty"`
	ReplacementTable  string `json:"replacement_table,omitempty"`
	ReplacementColumn string `json:"replacement_column,omitempty"`
	Message           string `json:"message"`
	Hint              string `json:"hint,omitempty"`
	Occurrences       int    `json:"occurrences"`
	UpdatedAt         string `json:"updated_at,omitempty"`
}

const learnedRulesFile = "learned_rules.json"

func (c *Catalog) loadLearnedRules(dataDir string) {
	path := filepath.Join(dataDir, learnedRulesFile)
	if _, err := os.Stat(path); err != nil {
		return
	}
	var rules []LearnedRule
	if err := readJSON(path, &rules); err != nil {
		c.Issues = append(c.Issues, LoadIssue{Level: "error", Source: learnedRulesFile, Message: err.Error()})
		return
	}
	c.applyLearnedRules(rules)
}

// applyLearnedRules installs rules into the in-memory catalog: table
// mis-selection penalties for search and correction hints for validation.
func (c *Catalog) applyLearnedRules(rules []LearnedRule) {
	c.LearnedRules = rules
	c.FeedbackPenalty = map[string]int{}
	for _, r := range rules {
		if r.Type == "table_correction" && r.Table != "" {
			if t, ok := c.ResolveTable(r.Table); ok {
				c.FeedbackPenalty[t.FQN] += r.Occurrences
			}
		}
	}
}

// LearnFromFeedback scans feedback/*.jsonl AND the DB execution audit
// (audit/query-*.jsonl) for repeated failure patterns and promotes those seen
// at least minOccurrences times into learned rules:
//   - recurring_error: same validation error code+table+column keeps recurring
//   - table_correction: corrected SQL consistently swaps table A for table B
//   - column_correction: corrected SQL consistently swaps column X for Y
//   - slow_query: queries touching a table are repeatedly slow (>=5s)
//   - recurring_exec_error: executions touching a table repeatedly fail with
//     the same ORA/TIMEOUT code
//
// The result is persisted to learned_rules.json and hot-applied.
func (c *Catalog) LearnFromFeedback(minOccurrences int) (map[string]any, error) {
	if minOccurrences <= 0 {
		minOccurrences = 3
	}
	dir := filepath.Join(c.DataDir, "feedback")
	entries, err := os.ReadDir(dir)
	if err != nil {
		entries = nil // feedback 없음 — 실행 감사(audit/query-*)만으로도 학습 가능
	}
	type errKey struct{ code, table, column string }
	type swapKey struct{ wrong, right string }
	errCounts := map[errKey]int{}
	tableSwaps := map[swapKey]int{}
	colSwaps := map[swapKey]int{} // keys are "TABLE.COLUMN"
	scanned := 0
	trustedScanned := 0
	skippedUnreviewed := 0
	duplicatesSkipped := 0
	seenFeedback := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		f, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
		for sc.Scan() {
			var rec FeedbackRecord
			if json.Unmarshal(sc.Bytes(), &rec) != nil {
				continue
			}
			scanned++
			if !c.FeedbackEligible(rec) {
				skippedUnreviewed++
				continue
			}
			if seenFeedback[rec.Fingerprint] {
				duplicatesSkipped++
				continue
			}
			seenFeedback[rec.Fingerprint] = true
			trustedScanned++
			for _, k := range extractErrorKeys(rec.Errors) {
				errCounts[errKey{k[0], k[1], k[2]}]++
			}
			if strings.EqualFold(rec.Outcome, "corrected") && rec.GeneratedSQL != "" && rec.FinalSQL != "" {
				genT := c.sqlTables(rec.GeneratedSQL)
				finT := c.sqlTables(rec.FinalSQL)
				wrongT := diffSet(genT, finT)
				rightT := diffSet(finT, genT)
				if len(wrongT) == 1 && len(rightT) == 1 {
					tableSwaps[swapKey{wrongT[0], rightT[0]}]++
				}
				wrongC := diffSet(c.sqlColumns(rec.GeneratedSQL, genT), c.sqlColumns(rec.FinalSQL, finT))
				rightC := diffSet(c.sqlColumns(rec.FinalSQL, finT), c.sqlColumns(rec.GeneratedSQL, genT))
				if len(wrongC) == 1 && len(rightC) == 1 {
					colSwaps[swapKey{wrongC[0], rightC[0]}]++
				}
			}
		}
		f.Close()
	}

	slowCounts, slowSumMs, execErrCounts := c.scanQueryAudit()

	now := time.Now().Format(time.RFC3339)
	var rules []LearnedRule
	for table, n := range slowCounts {
		if n < minOccurrences {
			continue
		}
		avg := slowSumMs[table] / int64(n)
		rules = append(rules, LearnedRule{
			Type: "slow_query", Table: table,
			Message:     fmt.Sprintf("queries touching %s were slow (>=5s) %d time(s), avg %dms", table, n, avg),
			Hint:        table + " 조회가 반복적으로 느립니다. 기간/파티션 조건과 인덱스 컬럼(explain_sql의 suggestions)을 보강하고, 실행 전 explain_sql로 플랜을 확인하세요.",
			Occurrences: n, UpdatedAt: now,
		})
	}
	for k, n := range execErrCounts {
		if n < minOccurrences {
			continue
		}
		rules = append(rules, LearnedRule{
			Type: "recurring_exec_error", Code: k.code, Table: k.table,
			Message:     fmt.Sprintf("executions touching %s failed with %s %d time(s)", k.table, k.code, n),
			Hint:        errCodeHint(k.code),
			Occurrences: n, UpdatedAt: now,
		})
	}
	for k, n := range errCounts {
		if n < minOccurrences {
			continue
		}
		rules = append(rules, LearnedRule{
			Type: "recurring_error", Code: k.code, Table: k.table, Column: k.column,
			Message:     fmt.Sprintf("validation error %s recurred %d time(s) on %s %s", k.code, n, k.table, k.column),
			Hint:        "이 패턴이 반복되고 있습니다. SQL 생성 시 해당 테이블/컬럼 사용 전에 get_schema_context로 재확인하세요.",
			Occurrences: n, UpdatedAt: now,
		})
	}
	for k, n := range tableSwaps {
		if n < minOccurrences {
			continue
		}
		rules = append(rules, LearnedRule{
			Type: "table_correction", Table: k.wrong, ReplacementTable: k.right,
			Message:     fmt.Sprintf("past corrections replaced table %s with %s %d time(s)", k.wrong, k.right, n),
			Hint:        k.wrong + " 대신 " + k.right + " 사용을 우선 검토하세요.",
			Occurrences: n, UpdatedAt: now,
		})
	}
	for k, n := range colSwaps {
		if n < minOccurrences {
			continue
		}
		wrongTable, wrongCol := splitQualified(k.wrong)
		_, rightCol := splitQualified(k.right)
		rules = append(rules, LearnedRule{
			Type: "column_correction", Table: wrongTable, Column: wrongCol, ReplacementColumn: rightCol,
			Message:     fmt.Sprintf("past corrections replaced column %s with %s %d time(s)", k.wrong, k.right, n),
			Hint:        k.wrong + " 대신 " + k.right + " 사용을 우선 검토하세요.",
			Occurrences: n, UpdatedAt: now,
		})
	}
	sort.SliceStable(rules, func(i, j int) bool {
		if rules[i].Occurrences != rules[j].Occurrences {
			return rules[i].Occurrences > rules[j].Occurrences
		}
		return rules[i].Message < rules[j].Message
	})

	path := filepath.Join(c.DataDir, learnedRulesFile)
	b, err := json.MarshalIndent(rules, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return nil, err
	}
	c.applyLearnedRules(rules)
	return map[string]any{
		"rules":              rules,
		"scanned":            scanned,
		"trusted_scanned":    trustedScanned,
		"skipped_unreviewed": skippedUnreviewed,
		"duplicates_skipped": duplicatesSkipped,
		"promoted":           len(rules),
		"min_occurrences":    minOccurrences,
		"path":               path,
		"note":               "only trusted + operator-approved, in-scope feedback is learned; rules are hot-applied to search and validation.",
	}, nil
}

// scanQueryAudit aggregates the DB execution audit (audit/query-*.jsonl,
// written by the connector) into per-table slow-query and error counters.
func (c *Catalog) scanQueryAudit() (slowCounts map[string]int, slowSumMs map[string]int64, errCounts map[execErrKey]int) {
	slowCounts = map[string]int{}
	slowSumMs = map[string]int64{}
	errCounts = map[execErrKey]int{}
	dir := filepath.Join(c.DataDir, "audit")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type auditLine struct {
		Tool  string `json:"tool"`
		Entry struct {
			SQLText   string `json:"sql_text"`
			ElapsedMs int64  `json:"elapsed_ms"`
			Success   bool   `json:"success"`
			ErrorCode string `json:"error_code"`
		} `json:"entry"`
	}
	const slowMs = 5000
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "query-") || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		f, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
		for sc.Scan() {
			var line auditLine
			if json.Unmarshal(sc.Bytes(), &line) != nil || line.Tool != "db:execute" {
				continue
			}
			tables := c.sqlTables(line.Entry.SQLText)
			switch {
			case line.Entry.Success && line.Entry.ElapsedMs >= slowMs:
				for _, t := range tables {
					slowCounts[t]++
					slowSumMs[t] += line.Entry.ElapsedMs
				}
			case !line.Entry.Success && line.Entry.ErrorCode != "" && line.Entry.ErrorCode != "CANCELED":
				for _, t := range tables {
					errCounts[execErrKey{line.Entry.ErrorCode, t}]++
				}
			}
		}
		f.Close()
	}
	return
}

type execErrKey struct{ code, table string }

func errCodeHint(code string) string {
	switch code {
	case "TIMEOUT":
		return "쿼리가 반복적으로 타임아웃됩니다. 기간 조건을 좁히고 explain_sql로 플랜을 확인한 뒤 실행하세요."
	case "PG-42P01", "MY-1146":
		return "테이블/뷰 접근 권한 또는 이름 문제가 반복됩니다. 프로파일 계정의 SELECT 권한과 스키마-한정 이름을 확인하세요."
	case "PG-28P01", "MY-1045":
		return "인증 실패가 반복됩니다. 프로파일의 계정/비밀번호 참조를 확인하세요."
	default:
		return "이 테이블 조회에서 " + code + " 오류가 반복됩니다. 실행 전 접속 테스트와 explain_sql로 원인을 확인하세요."
	}
}

// learnedRuleWarnings emits warnings when SQL touches a pattern that past
// feedback repeatedly corrected or flagged.
func (c *Catalog) learnedRuleWarnings(masked string, refs []tableRefInternal) []ValidationIssue {
	if len(c.LearnedRules) == 0 {
		return nil
	}
	var out []ValidationIssue
	refHas := func(fqn string) (tableRefInternal, bool) {
		for _, ref := range refs {
			if ref.Table.FQN == fqn {
				return ref, true
			}
		}
		return tableRefInternal{}, false
	}
	for _, r := range c.LearnedRules {
		t, ok := c.ResolveTable(r.Table)
		if !ok {
			continue
		}
		ref, used := refHas(t.FQN)
		if !used {
			continue
		}
		switch r.Type {
		case "table_correction":
			out = append(out, ValidationIssue{
				Level: "warning", Code: "LEARNED_TABLE_CORRECTION",
				Message: r.Message, Table: t.FQN, Hint: r.Hint,
			})
		case "column_correction":
			re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(ref.Alias) + `\s*\.\s*` + regexp.QuoteMeta(r.Column) + `\b`)
			if re.MatchString(masked) {
				out = append(out, ValidationIssue{
					Level: "warning", Code: "LEARNED_COLUMN_CORRECTION",
					Message: r.Message, Table: t.FQN, Column: r.Column, Hint: r.Hint,
				})
			}
		case "recurring_error":
			if r.Column == "" || regexp.MustCompile(`(?i)\b`+regexp.QuoteMeta(r.Column)+`\b`).MatchString(masked) {
				out = append(out, ValidationIssue{
					Level: "warning", Code: "LEARNED_RECURRING_ERROR",
					Message: r.Message, Table: t.FQN, Column: r.Column, Hint: r.Hint,
				})
			}
		case "slow_query":
			out = append(out, ValidationIssue{
				Level: "warning", Code: "LEARNED_SLOW_QUERY",
				Message: r.Message, Table: t.FQN, Hint: r.Hint,
			})
		case "recurring_exec_error":
			out = append(out, ValidationIssue{
				Level: "warning", Code: "LEARNED_EXEC_ERROR",
				Message: r.Message, Table: t.FQN, Hint: r.Hint,
			})
		}
	}
	return out
}

// sqlTables returns catalog-resolved FQNs referenced by FROM/JOIN in the SQL.
func (c *Catalog) sqlTables(sql string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range feedbackTableRE.FindAllStringSubmatch(sql, -1) {
		if t, ok := c.ResolveTable(strings.ReplaceAll(m[1], " ", "")); ok && !seen[t.FQN] {
			seen[t.FQN] = true
			out = append(out, t.FQN)
		}
	}
	sort.Strings(out)
	return out
}

// sqlColumns returns "FQN.COLUMN" for identifiers in the SQL that are real
// columns of the given tables. Alias-agnostic on purpose: corrected SQL often
// renames aliases, so we match by column existence instead.
func (c *Catalog) sqlColumns(sql string, tableFQNs []string) []string {
	seen := map[string]bool{}
	var out []string
	idents := map[string]bool{}
	for _, m := range identRE.FindAllString(maskSQL(sql), -1) {
		idents[cleanIdent(m)] = true
	}
	for _, fqn := range tableFQNs {
		t, ok := c.ResolveTable(fqn)
		if !ok {
			continue
		}
		for _, col := range t.Columns {
			if idents[col.Name] {
				key := t.FQN + "." + col.Name
				if !seen[key] {
					seen[key] = true
					out = append(out, key)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// extractErrorKeys pulls (code, table, column) triples from the stored
// validation_errors payload, accepting either a ValidationResult-shaped
// object or a bare issue array.
func extractErrorKeys(payload any) [][3]string {
	if payload == nil {
		return nil
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	var issues []ValidationIssue
	if err := json.Unmarshal(b, &issues); err != nil {
		var vr struct {
			Errors []ValidationIssue `json:"errors"`
		}
		if err := json.Unmarshal(b, &vr); err != nil {
			return nil
		}
		issues = vr.Errors
	}
	var out [][3]string
	for _, i := range issues {
		if i.Code == "" {
			continue
		}
		out = append(out, [3]string{i.Code, cleanIdent(i.Table), cleanIdent(i.Column)})
	}
	return out
}

func diffSet(a, b []string) []string {
	inB := map[string]bool{}
	for _, v := range b {
		inB[v] = true
	}
	var out []string
	for _, v := range a {
		if !inB[v] {
			out = append(out, v)
		}
	}
	return out
}

func splitQualified(s string) (table, column string) {
	if i := strings.LastIndex(s, "."); i >= 0 {
		return s[:i], s[i+1:]
	}
	return "", s
}
