package catalog

import "time"

// PrepareContext runs the deterministic front half of the NL2SQL pipeline in
// one call — analyze → search → metric dictionary → compact schema context →
// join paths → SQL skeleton — and returns a single bundle. Everything here is
// pure and read-only; it exists so an LLM client (or the /admin/ask screen)
// does not have to orchestrate 6-8 calls itself and cannot skip a step. The
// only thing left for the model is to fill the skeleton's /* SLOT */ markers
// and then call validate_sql.
//
// Clarification gate (mixed mode): the server judges whether the question is
// answerable as asked. Blocking ambiguities (undefined metric, near-tie table
// or filter-column choice, too-vague question) WITHHOLD the skeleton and
// return status "needs_clarification" with structured re-questions — ask the
// user, then re-call with `clarifications` ({id: answer|option-key}).
// Advisory items (e.g. missing time range) apply a safe default and only
// annotate the bundle.
func (c *Catalog) PrepareContext(question string, tables []string, limit int, now time.Time, clarifications map[string]string) map[string]any {
	return c.PrepareFollowup(question, "", "", tables, limit, now, clarifications)
}

// PrepareFollowup is PrepareContext with conversational context: when the
// user refines a previous answer ("그중 서울만", "이걸 월별로"), pass the prior
// question (and optionally its SQL) so search/metrics/time detection see the
// full intent. Short follow-ups skip the too_vague gate — brevity is natural
// in a refinement turn.
func (c *Catalog) PrepareFollowup(question, prevQuestion, prevSQL string, tables []string, limit int, now time.Time, clarifications map[string]string) map[string]any {
	followup := prevQuestion != ""
	if followup {
		// merged intent drives the pipeline; the original stays in the output
		question = prevQuestion + " — 후속 요청: " + question
		if clarifications == nil {
			clarifications = map[string]string{}
		}
		if _, ok := clarifications["too_vague"]; !ok {
			clarifications["too_vague"] = prevQuestion // context answers the vagueness
		}
	}
	// resolve prior answers into the working question so search/metric/time
	// detection all see them
	initialSearch := c.SearchSchema(SearchRequest{Question: question, TopK: 5, IncludeColumns: true, MaxColumns: 10})
	pending := c.DetectClarifications(question, initialSearch, now, clarifications)
	working := ResolveClarifications(question, pending, clarifications)
	search := initialSearch
	if working != question {
		search = c.SearchSchema(SearchRequest{Question: working, TopK: 5, IncludeColumns: true, MaxColumns: 10})
		pending = c.DetectClarifications(working, search, now, clarifications)
	}

	// One-round guarantee: once the caller has answered a round, any ambiguity
	// newly surfaced by the merged question is demoted to advisory — otherwise
	// each answer could unlock a fresh blocking item and the loop never ends.
	if len(clarifications) > 0 {
		for i := range pending {
			if pending[i].Severity == SeverityBlocking {
				pending[i].Severity = SeverityAdvisory
				if pending[i].Default == "" && len(pending[i].Options) > 0 {
					pending[i].Default = pending[i].Options[0].Label
				}
			}
		}
	}

	if HasBlocking(pending) && len(tables) == 0 {
		// withheld: no skeleton/schema context, so SQL generation cannot
		// proceed on a guess. (An explicit `tables` override is treated as the
		// caller taking responsibility for table choice.)
		blocking := []Clarification{}
		advisory := []Clarification{}
		for _, cl := range pending {
			if cl.Severity == SeverityBlocking {
				blocking = append(blocking, cl)
			} else {
				advisory = append(advisory, cl)
			}
		}
		return map[string]any{
			"status":         "needs_clarification",
			"question":       question,
			"confidence":     ClarificationConfidence(pending),
			"clarifications": blocking,
			"advisory":       advisory,
			"next_step": "SQL을 생성하지 마세요. clarifications의 질문을 사용자에게 그대로 전달해 답을 받은 뒤, " +
				"prepare_sql_context(question, clarifications={id: 답변 또는 옵션 key})로 다시 호출하세요. " +
				"options가 있으면 함께 제시하고 recommended를 기본 제안으로 표시하세요.",
		}
	}

	analysis := c.AnalyzeQuestion(AnalyzeRequest{Question: working})

	// selected tables: caller-supplied, else graph-expanded hybrid ranking
	// (seed search → 1-hop join expansion → re-rank), which picks joinable
	// neighbors and value-evidence tables a flat search order would miss.
	selected := append([]string{}, tables...)
	var retrieval RetrievalResult
	if len(selected) == 0 {
		retrieval = c.RetrieveContext(working, 6)
		for _, tc := range retrieval.Tables {
			selected = append(selected, tc.Table)
		}
		// fallback: if graph retrieval found nothing, use raw search order
		if len(selected) == 0 {
			for _, r := range search.Results {
				selected = append(selected, r.Table)
			}
		}
	}

	// dictionary metric definitions for every metric term in the question
	metricDefs := []MetricDef{}
	seen := map[string]bool{}
	for _, name := range c.MetricNamesInQuestion(working) {
		if seen[name] {
			continue
		}
		seen[name] = true
		if defs := c.LookupMetrics(name); len(defs) > 0 {
			metricDefs = append(metricDefs, defs[0])
		}
	}

	schemaCtx := c.SchemaContext(working, selected, 24)

	joinPaths := map[string]any{}
	if len(selected) >= 2 {
		if jp, err := c.GetJoinPaths(JoinPathRequest{Tables: selected}); err == nil {
			joinPaths = jp
		}
	}

	skeleton := c.BuildSQLSkeleton(working, selected, limit, now)

	// expected outputs to forward into validate_sql (name-drift avoidance)
	expected, _ := analysis["expected_output_columns"].([]string)

	out := map[string]any{
		"status":                  "ready",
		"question":                question,
		"analysis":                analysis,
		"selected_tables":         selected,
		"search_candidates":       search.Results,
		"ranked_tables":           retrieval.Tables, // graph-expanded hybrid ranking w/ per-signal provenance
		"value_evidence":          retrieval.Values,
		"retrieval_trace":         retrieval.Trace,
		"excluded_candidates":     search.Excluded,
		"metrics":                 metricDefs,
		"schema_context":          schemaCtx,
		"join_paths":              joinPaths,
		"skeleton":                skeleton,
		"expected_output_columns": expected,
		"metric_names":            c.MetricNamesInQuestion(working),
		"confidence":              ClarificationConfidence(pending),
		"next_step": "skeleton.skeleton_sql의 /* SLOT */ 주석만 채워 하나의 SELECT(대상 DB 방언)를 완성한 뒤, " +
			"validate_sql(sql, metrics=metric_names, expected_outputs=expected_output_columns)로 검증하고 " +
			"(선택) explain_sql로 위험을 확인하세요. 실행은 run_sql_safely(sql, profile).",
		"rules": []string{
			"이 번들에 있는 테이블·컬럼·조인 조건·지표 계산식만 사용하세요.",
			"조인 ON 절은 join_paths / skeleton에서만 가져오고 임의로 만들지 마세요.",
			"지표는 metrics의 expression을 그대로 사용하세요.",
			"PII 컬럼은 출력하지 말고, 항상 row 한계를 두세요.",
		},
	}
	if working != question {
		out["resolved_question"] = working
		out["applied_clarifications"] = clarifications
	}
	if followup {
		out["followup"] = true
		if prevSQL != "" {
			out["previous_sql"] = prevSQL
			out["followup_hint"] = "previous_sql을 출발점으로 삼아 후속 요청만 반영해 수정하세요(가능하면 구조 유지). 수정 결과도 반드시 validate_sql로 검증합니다."
		}
	}
	if len(pending) > 0 {
		// advisory leftovers: proceed with defaults but surface assumptions
		out["advisory"] = pending
		assumptions := []string{}
		for _, cl := range pending {
			if cl.Default != "" {
				assumptions = append(assumptions, cl.Question+" → 기본값: "+cl.Default)
			}
		}
		if len(assumptions) > 0 {
			out["assumptions"] = assumptions
		}
	}
	return out
}
