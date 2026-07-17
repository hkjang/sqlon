package catalog

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

var numericRE = regexp.MustCompile(`^\d+$`)

// Clarification is a structured re-question the server wants asked before SQL
// generation proceeds. Severity "blocking" withholds the SQL skeleton until the
// user answers; "advisory" applies Default and only annotates the answer.
type Clarification struct {
	ID       string       `json:"id"`
	Severity string       `json:"severity"` // blocking | advisory
	Question string       `json:"question"`
	Options  []ClarOption `json:"options,omitempty"`
	Default  string       `json:"default,omitempty"`
}

type ClarOption struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Recommended bool   `json:"recommended,omitempty"`
}

const (
	SeverityBlocking = "blocking"
	SeverityAdvisory = "advisory"
)

// DetectClarifications inspects a question (plus the already-computed schema
// search) and returns the re-questions the server judges necessary, most
// severe first. `resolved` carries answers from a previous round keyed by
// clarification ID — resolved items are not raised again.
func (c *Catalog) DetectClarifications(question string, search SearchResponse, now time.Time, resolved map[string]string) []Clarification {
	q := strings.ToLower(strings.TrimSpace(question))
	out := []Clarification{}
	skip := func(id string) bool { _, ok := resolved[id]; return ok }

	// 1) too vague: barely any signal to ground table/metric selection on.
	// A bare metric word ("잔액") still blocks — knowing WHAT doesn't say by
	// which dimension, over which period, or in what shape.
	if !skip("too_vague") {
		noTime := !hasTimeRange(question, now)
		if len([]rune(q)) < 10 && noTime {
			out = append(out, Clarification{
				ID: "too_vague", Severity: SeverityBlocking,
				Question: "질문이 짧아 대상을 특정하기 어렵습니다. 무엇을(지표/항목), 어떤 기준으로(기간/차원), 어떤 형태로(목록/집계/추이) 보고 싶은지 알려주세요.",
			})
		}
	}

	// 2) metric term with no dictionary entry: the formula is a guess.
	for _, kw := range []string{"평점", "점수", "잔액", "금액", "비율", "율"} {
		id := "metric:" + kw
		if skip(id) || !strings.Contains(q, kw) || len(c.LookupMetrics(kw)) > 0 {
			continue
		}
		covered := false
		for _, name := range c.MetricNamesInQuestion(question) {
			if strings.Contains(name, kw) {
				covered = true
				break
			}
		}
		if covered {
			continue
		}
		opts := []ClarOption{}
		if def := c.MetricDefinition(kw, 3); def["source"] == "inferred" {
			if cands, ok := def["inferred_candidates"].([]inferredMetric); ok {
				for i, cand := range cands {
					if i >= 3 {
						break
					}
					label := cand.Expression
					if cand.LogicalName != "" {
						label = cand.LogicalName + " — " + cand.Expression
					}
					opts = append(opts, ClarOption{Key: fmt.Sprintf("%c", 'a'+i), Label: label, Recommended: i == 0})
				}
			}
		}
		out = append(out, Clarification{
			ID: id, Severity: SeverityBlocking,
			Question: "지표 '" + kw + "'가 지표 사전에 없습니다. 계산 기준을 확인해주세요.",
			Options:  opts,
		})
	}

	// 3) near-tie between top tables: wrong pick flips the whole answer, so
	// ask instead of guessing — unconditionally (see DetectTableChoiceAmbiguity).
	if !skip("table_choice") {
		if cl, ok := DetectTableChoiceAmbiguity(search); ok {
			out = append(out, cl)
		}
	}

	// 3b) ambiguous filter column: a literal in the question resolves to two or
	// more different columns with near-tie scores — the WHERE clause would be a
	// coin flip, so ask which column the user means.
	if !skip("column_choice") {
		if cl, ok := c.detectColumnAmbiguity(question, search); ok {
			out = append(out, cl)
		}
	}

	// 4) missing time range: safe default exists → advisory, not blocking.
	if !skip("time_range") && !hasTimeRange(question, now) {
		out = append(out, Clarification{
			ID: "time_range", Severity: SeverityAdvisory,
			Question: "기간이 지정되지 않아 최신 기준월/기준일 데이터를 사용합니다. 다른 기간이 필요하면 알려주세요.",
			Default:  "최신 기준월/기준일",
		})
	}

	// blocking first
	for i, cl := range out {
		if cl.Severity == SeverityBlocking && i > 0 {
			out[0], out[i] = out[i], out[0]
			break
		}
	}
	return out
}

// HasBlocking reports whether any clarification requires a user answer.
func HasBlocking(cls []Clarification) bool {
	for _, cl := range cls {
		if cl.Severity == SeverityBlocking {
			return true
		}
	}
	return false
}

// ClarificationConfidence is a coarse 0..1 score for how safely SQL generation
// can proceed without asking.
func ClarificationConfidence(cls []Clarification) float64 {
	conf := 1.0
	for _, cl := range cls {
		if cl.Severity == SeverityBlocking {
			conf -= 0.35
		} else {
			conf -= 0.1
		}
	}
	if conf < 0.05 {
		conf = 0.05
	}
	return conf
}

// ResolveClarifications folds user answers back into the question text so the
// downstream pipeline (search/metrics/time parsing) sees them. Option keys are
// expanded to their labels; free-text answers are appended verbatim.
func ResolveClarifications(question string, cls []Clarification, answers map[string]string) string {
	if len(answers) == 0 {
		return question
	}
	byID := map[string]Clarification{}
	for _, cl := range cls {
		byID[cl.ID] = cl
	}
	extra := []string{}
	for id, ans := range answers {
		ans = strings.TrimSpace(ans)
		if ans == "" {
			continue
		}
		if cl, ok := byID[id]; ok {
			for _, opt := range cl.Options {
				if strings.EqualFold(opt.Key, ans) {
					ans = opt.Label
					break
				}
			}
		}
		extra = append(extra, ans)
	}
	if len(extra) == 0 {
		return question
	}
	return question + " (" + strings.Join(extra, "; ") + ")"
}

// DetectTableChoiceAmbiguity reports whether the top schema-search results
// are a near-tie (any result scoring within 8% of the top score), and if so
// returns a blocking "table_choice" clarification listing every tied
// candidate. Ties are never resolved by silently picking rank #1 — including
// when the candidates share the same (or unset) domain/grain metadata, since
// that is exactly the case of two near-duplicate tables where guessing wrong
// silently returns data from the wrong table. Both DetectClarifications and
// BuildSQLSkeleton's question-only auto-search path call this so neither
// route can bypass the gate.
func DetectTableChoiceAmbiguity(search SearchResponse) (Clarification, bool) {
	if len(search.Results) < 2 {
		return Clarification{}, false
	}
	r1 := search.Results[0]
	if r1.Score <= 0 {
		return Clarification{}, false
	}
	tied := []SearchResult{r1}
	for _, r := range search.Results[1:] {
		if r.Score >= r1.Score*0.92 {
			tied = append(tied, r)
		}
	}
	if len(tied) < 2 {
		return Clarification{}, false
	}
	label := func(r SearchResult) string {
		l := r.Table
		if r.LogicalName != "" {
			l += " (" + r.LogicalName + ")"
		}
		if r.Grain != "" {
			l += " · grain=" + r.Grain
		}
		if r.Domain != "" {
			l += " · domain=" + r.Domain
		}
		if r.Description != "" {
			l += " — " + firstSentence(r.Description)
		}
		return l
	}
	opts := []ClarOption{}
	for i, r := range tied {
		if i >= 4 {
			break
		}
		opts = append(opts, ClarOption{Key: fmt.Sprintf("%c", 'a'+i), Label: label(r), Recommended: i == 0})
	}
	return Clarification{
		ID: "table_choice", Severity: SeverityBlocking,
		Question: "질문에 부합하는 테이블 후보가 여러 개 비슷한 점수로 검색되었습니다. 어느 테이블 기준의 데이터가 필요한가요?",
		Options:  opts,
	}, true
}

// detectColumnAmbiguity finds a question literal whose filter-column
// resolution is a near-tie across different columns, scoped to the top search
// tables so unrelated tables don't inflate the candidate set.
func (c *Catalog) detectColumnAmbiguity(question string, search SearchResponse) (Clarification, bool) {
	tables := []string{}
	for i, r := range search.Results {
		if i >= 3 {
			break
		}
		tables = append(tables, r.Table)
	}
	if len(tables) == 0 {
		return Clarification{}, false
	}
	valueTokens := []string{}
	for _, tok := range tokenize(question) {
		if len([]rune(tok)) >= 2 && !numericRE.MatchString(tok) {
			valueTokens = append(valueTokens, tok)
		}
	}
	if len(valueTokens) == 0 {
		return Clarification{}, false
	}
	res := c.FindFilterColumns(valueTokens, tables, 12)
	cands, _ := res["candidates"].([]FilterColumnCandidate)
	byValue := map[string][]FilterColumnCandidate{}
	for _, cand := range cands {
		byValue[cand.Value] = append(byValue[cand.Value], cand)
	}
	for value, list := range byValue {
		if len(list) < 2 {
			continue
		}
		// near-tie across *different* columns only
		first, second := list[0], list[1]
		if first.Table == second.Table && first.Column == second.Column {
			continue
		}
		// only exact-match-class hits (code_dict label / top_values ≈ 10+)
		// count: weak fuzzy substring matches would fire on every dimension
		// word (e.g. '회원사' hitting unrelated *사유코드 columns).
		if first.Score < 10 || second.Score < first.Score*0.92 {
			continue
		}
		opts := []ClarOption{}
		for i, cand := range list {
			if i >= 3 {
				break
			}
			label := cand.Table + "." + cand.Column
			if cand.LogicalName != "" {
				label += " (" + cand.LogicalName + ")"
			}
			if cand.SuggestedPredicate != "" {
				label += " → " + cand.SuggestedPredicate
			}
			opts = append(opts, ClarOption{Key: fmt.Sprintf("%c", 'a'+i), Label: label, Recommended: i == 0})
		}
		return Clarification{
			ID: "column_choice", Severity: SeverityBlocking,
			Question: "'" + value + "' 값이 서로 다른 컬럼에 비슷한 점수로 매칭됩니다. 어느 컬럼 기준으로 필터링할까요?",
			Options:  opts,
		}, true
	}
	return Clarification{}, false
}

func hasTimeRange(question string, now time.Time) bool {
	for _, tr := range ParseTimeExpressions(question, now) {
		if tr.Start != "" {
			return true
		}
	}
	return false
}

func firstSentence(s string) string {
	if i := strings.IndexAny(s, ".。"); i > 0 {
		return s[:i]
	}
	if len([]rune(s)) > 60 {
		return string([]rune(s)[:60]) + "…"
	}
	return s
}
