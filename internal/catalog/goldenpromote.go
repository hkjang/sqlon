package catalog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Feedback → golden-set promotion (improvement #2). Approved, successful,
// executed feedback records are surfaced as golden-query candidates and, once
// promoted, appended to golden_queries.json so the evaluation set grows from
// real production traffic. Promotion is an explicit operator act (admin), and
// only trust-boundary-approved feedback is ever eligible — the same fail-closed
// rule the retrieval/learning path uses (see FeedbackEligible).

// GoldenCandidate is a proposed golden query derived from one feedback record.
type GoldenCandidate struct {
	FeedbackID      string   `json:"feedback_id"`
	Question        string   `json:"question"`
	ExpectedSQL     string   `json:"expected_sql"`
	ExpectedTables  []string `json:"expected_tables,omitempty"`
	ExpectedColumns []string `json:"expected_columns,omitempty"`
	RecordedAt      string   `json:"recorded_at,omitempty"`
	ResultRows      *int64   `json:"result_rows,omitempty"`
	Reason          string   `json:"reason,omitempty"`
}

// SuggestGoldenFromFeedback returns golden-query candidates built from approved,
// successful, executed feedback that is not already represented in the golden
// set (deduped by normalized question and SQL).
func (c *Catalog) SuggestGoldenFromFeedback(limit int) map[string]any {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	existingQ, existingSQL := c.goldenDedupIndex()

	recs := c.scanApprovedSuccessFeedback()
	var out []GoldenCandidate
	seen := map[string]bool{}
	for _, rec := range recs {
		sql := strings.TrimSpace(rec.FinalSQL)
		if sql == "" {
			sql = strings.TrimSpace(rec.GeneratedSQL)
		}
		q := strings.TrimSpace(rec.Question)
		if sql == "" || q == "" {
			continue
		}
		nq, ns := normQuestion(q), normSQL(sql)
		if existingQ[nq] || existingSQL[ns] || seen[nq] || seen[ns] {
			continue // already a golden query or already proposed this run
		}
		seen[nq], seen[ns] = true, true
		out = append(out, GoldenCandidate{
			FeedbackID:      rec.ID,
			Question:        q,
			ExpectedSQL:     sql,
			ExpectedTables:  rec.Tables,
			ExpectedColumns: rec.Columns,
			RecordedAt:      rec.RecordedAt,
			ResultRows:      rec.ResultRows,
			Reason:          "approved + successful + executed feedback, not yet in golden set",
		})
		if len(out) >= limit {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].RecordedAt > out[j].RecordedAt })
	return map[string]any{
		"candidates":   out,
		"count":        len(out),
		"how_to_apply": "promote_golden_queries(feedback_ids=[...])로 golden_queries.json에 추가하고 재기동/리로드하세요.",
		"note":         "승인·성공·실행된 피드백만 대상입니다. 승격은 명시적 관리자 행위이며 골든셋 중복은 질문/SQL 정규화로 제외합니다.",
	}
}

// PromoteGolden appends the chosen feedback-derived candidates to
// golden_queries.json (backed up). The caller reloads the catalog afterwards.
func (c *Catalog) PromoteGolden(feedbackIDs []string, now time.Time) map[string]any {
	want := map[string]bool{}
	for _, id := range feedbackIDs {
		if s := strings.TrimSpace(id); s != "" {
			want[s] = true
		}
	}
	if len(want) == 0 {
		return map[string]any{"error": "feedback_ids is required"}
	}

	// recompute candidates so we promote exactly what SuggestGolden would show
	sugg, _ := c.SuggestGoldenFromFeedback(200)["candidates"].([]GoldenCandidate)
	byID := map[string]GoldenCandidate{}
	for _, g := range sugg {
		byID[g.FeedbackID] = g
	}

	path := filepath.Join(c.DataDir, "golden_queries.json")
	var list []map[string]any
	if err := readJSONFileAs(path, &list); err != nil {
		return map[string]any{"error": "failed to read golden_queries.json: " + err.Error()}
	}
	existingQ, existingSQL := c.goldenDedupIndex()

	// next numeric id
	maxID := int64(0)
	for _, g := range list {
		if f, ok := g["id"].(float64); ok && int64(f) > maxID {
			maxID = int64(f)
		}
	}

	applied, unknown, skipped := 0, []string{}, []string{}
	for id := range want {
		cand, ok := byID[id]
		if !ok {
			unknown = append(unknown, id)
			continue
		}
		if existingQ[normQuestion(cand.Question)] || existingSQL[normSQL(cand.ExpectedSQL)] {
			skipped = append(skipped, id)
			continue
		}
		maxID++
		row := map[string]any{
			"id":              maxID,
			"question":        cand.Question,
			"expected_tables": cand.ExpectedTables,
			"expected_sql":    cand.ExpectedSQL,
			"note":            "promoted from feedback " + cand.FeedbackID + " at " + now.UTC().Format(time.RFC3339),
		}
		if len(cand.ExpectedColumns) > 0 {
			row["expected_columns"] = cand.ExpectedColumns
		}
		list = append(list, row)
		existingQ[normQuestion(cand.Question)] = true
		existingSQL[normSQL(cand.ExpectedSQL)] = true
		applied++
	}
	if applied == 0 {
		return map[string]any{"applied": 0, "unknown_ids": unknown, "skipped_ids": skipped,
			"note": "추가된 골든 질의가 없습니다(중복 또는 대상 없음)."}
	}
	backup, err := writeJSONFile(c.DataDir, "golden_queries.json", list)
	if err != nil {
		return map[string]any{"error": "failed to write golden_queries.json: " + err.Error()}
	}
	res := map[string]any{"applied": applied, "backup": backup, "total_golden": len(list),
		"note": "golden_queries.json에 추가했습니다. 카탈로그 리로드 후 run_evaluation으로 확인하세요."}
	if len(unknown) > 0 {
		res["unknown_ids"] = unknown
	}
	if len(skipped) > 0 {
		res["skipped_ids"] = skipped
	}
	return res
}

// ---- helpers ----

// goldenDedupIndex indexes existing golden queries by normalized question and
// SQL so promotion never duplicates.
func (c *Catalog) goldenDedupIndex() (map[string]bool, map[string]bool) {
	q := map[string]bool{}
	sql := map[string]bool{}
	path := filepath.Join(c.DataDir, "golden_queries.json")
	var list []GoldenQuery
	if err := readJSONFileAs(path, &list); err != nil {
		return q, sql
	}
	for _, g := range list {
		if g.Question != "" {
			q[normQuestion(g.Question)] = true
		}
		if g.ExpectedSQL != "" {
			sql[normSQL(g.ExpectedSQL)] = true
		}
	}
	return q, sql
}

// scanApprovedSuccessFeedback reads the feedback dir for records that crossed
// the trust boundary (approved+trusted, in-scope) AND represent a successful
// executed outcome — the only records worth turning into golden truth.
func (c *Catalog) scanApprovedSuccessFeedback() []FeedbackRecord {
	dir := filepath.Join(c.DataDir, "feedback")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []FeedbackRecord
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
			if !c.FeedbackEligible(rec) {
				continue // fail-closed: only trust-approved records
			}
			if !strings.EqualFold(rec.Outcome, "success") && !strings.EqualFold(rec.Outcome, "corrected") {
				continue
			}
			if rec.Executed != nil && !*rec.Executed {
				continue
			}
			out = append(out, rec)
		}
		_ = f.Close()
	}
	return out
}

var wsRE = strings.NewReplacer("\t", " ", "\n", " ", "\r", " ")

func normQuestion(s string) string {
	return collapseSpaces(strings.ToLower(wsRE.Replace(s)))
}

func normSQL(s string) string {
	s = strings.ToLower(wsRE.Replace(s))
	s = strings.TrimRight(strings.TrimSpace(s), ";")
	return collapseSpaces(s)
}

func collapseSpaces(s string) string {
	var b strings.Builder
	prevSpace := false
	for _, r := range s {
		if r == ' ' {
			if !prevSpace {
				b.WriteRune(r)
			}
			prevSpace = true
			continue
		}
		prevSpace = false
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}
