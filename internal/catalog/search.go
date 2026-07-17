package catalog

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

type SearchRequest struct {
	Question       string   `json:"question"`
	TopK           int      `json:"top_k,omitempty"`
	Schemas        []string `json:"schemas,omitempty"`
	IncludeColumns bool     `json:"include_columns,omitempty"`
	MaxColumns     int      `json:"max_columns,omitempty"`
}

type SearchResponse struct {
	Question        string              `json:"question"`
	Results         []SearchResult      `json:"results"`
	Excluded        []ExcludedCandidate `json:"excluded,omitempty"`
	Tokens          []string            `json:"expanded_tokens,omitempty"`
	GlossaryMatches map[string][]string `json:"glossary_matches,omitempty"`
	Summary         CatalogSummary      `json:"catalog_summary"`
}

type SearchResult struct {
	Table          string        `json:"table"`
	Schema         string        `json:"schema"`
	Name           string        `json:"name"`
	LogicalName    string        `json:"logical_name,omitempty"`
	Description    string        `json:"description,omitempty"`
	Domain         string        `json:"domain,omitempty"`
	Grain          string        `json:"grain,omitempty"`
	RowCount       int64         `json:"row_count,omitempty"`
	Freshness      string        `json:"freshness,omitempty"`
	Score          float64       `json:"score"`
	Reasons        []string      `json:"reasons,omitempty"`
	MatchedTerms   []string      `json:"matched_terms,omitempty"`
	WhyInclude     string        `json:"why_include,omitempty"`
	MatchedColumns []ColumnMatch `json:"matched_columns,omitempty"`
	PrimaryKeys    []string      `json:"primary_keys,omitempty"`
	ForeignKeys    []string      `json:"foreign_keys,omitempty"`
	PolicyHints    []string      `json:"policy_hints,omitempty"`
}

type ExcludedCandidate struct {
	Table  string  `json:"table"`
	Score  float64 `json:"score"`
	Reason string  `json:"reason"`
}

type ColumnMatch struct {
	Name        string  `json:"name"`
	LogicalName string  `json:"logical_name,omitempty"`
	DataType    string  `json:"data_type,omitempty"`
	Description string  `json:"description,omitempty"`
	Score       float64 `json:"score"`
	CodeDict    string  `json:"code_dict,omitempty"`
	IsPK        bool    `json:"is_pk,omitempty"`
	IsFK        bool    `json:"is_fk,omitempty"`
}

type CatalogSummary struct {
	TableCount    int            `json:"table_count"`
	ColumnCount   int            `json:"column_count"`
	RelationCount int            `json:"relation_count"`
	SampleCount   int            `json:"sample_count"`
	Schemas       map[string]int `json:"schemas"`
	LoadedAt      string         `json:"loaded_at"`
	DataDir       string         `json:"data_dir,omitempty"`
}

func (c *Catalog) SearchSchema(req SearchRequest) SearchResponse {
	if req.TopK <= 0 {
		req.TopK = 8
	}
	if req.MaxColumns <= 0 {
		req.MaxColumns = 8
	}
	baseTokens := tokenize(req.Question)
	tokens, glossaryHits := c.Glossary.Expand(baseTokens)
	schemaFilter := map[string]bool{}
	for _, s := range req.Schemas {
		if v := cleanIdent(s); v != "" {
			schemaFilter[v] = true
		}
	}
	metricTables := c.metricsInQuestion(req.Question)

	results := make([]SearchResult, 0, len(c.Tables))
	for _, t := range c.Tables {
		if len(schemaFilter) > 0 && !schemaFilter[t.Schema] {
			continue
		}
		tableScore, reasons, matchedTerms := c.scoreTable(req.Question, tokens, t)
		if boost, sampleReason := c.sampleBoostForTable(req.Question, tokens, t); boost > 0 {
			tableScore += boost
			reasons = appendReason(reasons, sampleReason)
		}
		if n := c.FeedbackUsage[t.FQN]; n > 0 {
			boost := math.Min(12, 3*math.Log2(float64(1+n)))
			tableScore += boost
			reasons = appendReason(reasons, fmt.Sprintf("past successful SQL used this table %d time(s)", n))
		}
		if n := c.FeedbackPenalty[t.FQN]; n > 0 {
			penalty := math.Min(10, 2.5*float64(n))
			tableScore -= penalty
			reasons = appendReason(reasons, fmt.Sprintf("penalized: past feedback corrected this table away %d time(s)", n))
		}
		if names := metricTables[t.FQN]; len(names) > 0 {
			tableScore += 12
			reasons = appendReason(reasons, "metric dictionary defines '"+strings.Join(names, ", ")+"' on this table")
			matchedTerms = append(matchedTerms, names...)
		}
		matches := scoreColumns(tokens, t, req.MaxColumns)
		for _, m := range matches {
			tableScore += math.Min(m.Score, 8) * 0.35
		}
		if tableScore <= 0 {
			continue
		}
		res := SearchResult{
			Table:        t.FQN,
			Schema:       t.Schema,
			Name:         t.Name,
			LogicalName:  t.LogicalName,
			Description:  t.Description,
			Domain:       t.Domain,
			Grain:        t.Grain,
			RowCount:     t.RowCount,
			Freshness:    t.Freshness,
			Score:        round(tableScore),
			Reasons:      reasons,
			MatchedTerms: unique(matchedTerms),
			PrimaryKeys:  t.PrimaryKeys,
			ForeignKeys:  t.ForeignKeys,
			PolicyHints:  c.policyHints(t),
		}
		if req.IncludeColumns {
			res.MatchedColumns = matches
		}
		results = append(results, res)
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score == results[j].Score {
			return results[i].Table < results[j].Table
		}
		return results[i].Score > results[j].Score
	})
	// join-connectivity boost: reward candidates connected to other strong
	// candidates so isolated lookalike tables sink.
	pool := len(results)
	if pool > req.TopK*2 {
		pool = req.TopK * 2
	}
	inPool := map[string]bool{}
	for i := 0; i < pool; i++ {
		inPool[results[i].Table] = true
	}
	for i := 0; i < pool; i++ {
		for _, edge := range c.Adjacency[results[i].Table] {
			if edge.To != results[i].Table && inPool[edge.To] {
				results[i].Score = round(results[i].Score + 4)
				results[i].Reasons = appendReason(results[i].Reasons, "joinable to another candidate table ("+edge.To+")")
				break
			}
		}
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score == results[j].Score {
			return results[i].Table < results[j].Table
		}
		return results[i].Score > results[j].Score
	})
	var excluded []ExcludedCandidate
	if len(results) > req.TopK {
		for _, r := range results[req.TopK:] {
			if len(excluded) >= 5 {
				break
			}
			excluded = append(excluded, ExcludedCandidate{
				Table:  r.Table,
				Score:  r.Score,
				Reason: "score below top_k cutoff; matched weaker signals (" + strings.Join(trimReasons(r.Reasons, 2), "; ") + ")",
			})
		}
		results = results[:req.TopK]
	}
	for i := range results {
		results[i].WhyInclude = whyInclude(results[i])
	}
	return SearchResponse{
		Question:        req.Question,
		Results:         results,
		Excluded:        excluded,
		Tokens:          tokens,
		GlossaryMatches: glossaryHits,
		Summary:         c.Summary(),
	}
}

// expandTokens applies the shared glossary; kept as a method so every stage
// (search, analyze, metrics, feedback) expands terms identically.
func (c *Catalog) expandTokens(tokens []string) []string {
	out, _ := c.Glossary.Expand(tokens)
	return out
}

// metricsInQuestion maps table FQN -> high-confidence metric resolutions.
// It intentionally delegates to the same resolver as LookupMetrics and
// MetricNamesInQuestion so retrieval, context, and validation cannot disagree.
func (c *Catalog) metricsInQuestion(question string) map[string][]string {
	out := map[string][]string{}
	for _, match := range c.resolveMetricMatches(question) {
		for _, tn := range match.Definition.Tables {
			if t, ok := c.ResolveTable(tn); ok {
				out[t.FQN] = appendUnique(out[t.FQN], match.Definition.Name)
			}
		}
	}
	return out
}

func whyInclude(r SearchResult) string {
	if len(r.Reasons) == 0 {
		return ""
	}
	return "포함 사유: " + strings.Join(trimReasons(r.Reasons, 3), "; ")
}

func (c *Catalog) sampleBoostForTable(question string, tokens []string, t *Table) (float64, string) {
	best := 0.0
	for _, sample := range c.Samples {
		target := strings.ToUpper(sample.TargetTable)
		if !strings.Contains(target, t.FQN) && !strings.Contains(target, t.Name) {
			continue
		}
		text := strings.ToLower(strings.Join([]string{sample.Question, sample.TargetDomain, sample.TargetIntent, sample.TargetColumn}, " "))
		score := 0.0
		for _, tok := range tokens {
			if strings.Contains(text, strings.ToLower(tok)) {
				score += 1.2
			}
		}
		if question != "" && strings.Contains(strings.ToLower(sample.Question), strings.ToLower(question)) {
			score += 20
		}
		if score > best {
			best = score
		}
	}
	if best == 0 {
		return 0, ""
	}
	if best > 24 {
		best = 24
	}
	return best, "few-shot example target table match"
}

func (c *Catalog) SchemaContext(question string, tableNames []string, maxColumns int) map[string]any {
	if maxColumns <= 0 {
		maxColumns = 24
	}
	if len(tableNames) == 0 && question != "" {
		search := c.SearchSchema(SearchRequest{Question: question, TopK: 5, IncludeColumns: true, MaxColumns: maxColumns})
		for _, r := range search.Results {
			tableNames = append(tableNames, r.Table)
		}
	}
	tokens := c.expandTokens(tokenize(question))
	items := []map[string]any{}
	var resolved []*Table
	for _, name := range tableNames {
		t, ok := c.ResolveTable(name)
		if !ok {
			items = append(items, map[string]any{"table": name, "error": "table not found"})
			continue
		}
		resolved = append(resolved, t)
		cols, excludedCols := c.columnsForContext(t, tokens, maxColumns)
		item := map[string]any{
			"table":        t.FQN,
			"logical_name": t.LogicalName,
			"description":  t.Description,
			"domain":       t.Domain,
			"grain":        t.Grain,
			"primary_keys": t.PrimaryKeys,
			"foreign_keys": t.ForeignKeys,
			"policy_hints": c.policyHints(t),
			"indexes":      t.Indexes,
			"columns":      cols,
		}
		if t.RowCount > 0 {
			item["row_count"] = t.RowCount
		}
		if t.Freshness != "" {
			item["freshness"] = t.Freshness
		}
		if dfs := c.DefaultFiltersFor(t.FQN); len(dfs) > 0 {
			item["required_filters"] = dfs
		}
		if len(excludedCols) > 0 {
			item["excluded_columns"] = map[string]any{
				"count":  len(excludedCols),
				"names":  capList(excludedCols, 10),
				"reason": "low relevance to question; raise max_columns_per_table or call get_column_stats to inspect",
			}
		}
		items = append(items, item)
	}
	// mandatory join conditions between the selected tables so the LLM never
	// invents its own ON clauses.
	joins := []map[string]any{}
	prohibited := []map[string]any{}
	for i := 0; i < len(resolved); i++ {
		for j := i + 1; j < len(resolved); j++ {
			if fj, bad := c.IsForbiddenJoin(resolved[i].FQN, resolved[j].FQN); bad {
				prohibited = append(prohibited, map[string]any{
					"from": resolved[i].FQN, "to": resolved[j].FQN, "reason": fj.Reason,
				})
				continue
			}
			p := c.findJoinPath(resolved[i].FQN, resolved[j].FQN, 3)
			if p.Found && p.Depth > 0 {
				joins = append(joins, map[string]any{
					"from":       p.From,
					"to":         p.To,
					"depth":      p.Depth,
					"confidence": p.Confidence,
					"edges":      p.Edges,
				})
			}
		}
	}
	// dictionary metrics referenced by the question
	metricDefs := []MetricDef{}
	seenMetric := map[string]bool{}
	for _, names := range c.metricsInQuestion(question) {
		for _, n := range names {
			if seenMetric[n] {
				continue
			}
			seenMetric[n] = true
			for _, m := range c.Metrics {
				if m.Name == n {
					metricDefs = append(metricDefs, m)
				}
			}
		}
	}
	timeRanges := ParseTimeExpressions(question, time.Now())
	timeHints := []map[string]any{}
	for _, t := range resolved {
		for _, col := range t.Columns {
			if col.SemanticType == "" {
				continue
			}
			for _, tr := range timeRanges {
				if cond := RenderTimeCondition(col, tr, ""); cond != "" {
					timeHints = append(timeHints, map[string]any{
						"table": t.FQN, "column": col.Name, "semantic_type": col.SemanticType,
						"expression": tr.Expression, "condition": cond,
					})
				}
			}
		}
	}
	res := map[string]any{
		"question":        question,
		"dialect":         c.Dialect,
		"tables":          items,
		"join_conditions": joins,
		"metrics":         metricDefs,
		"time_conditions": timeHints,
		"rules": []string{
			"Use only tables and columns listed in this context; never invent identifiers.",
			"Use schema-qualified table names.",
			"Join only via join_conditions; if a needed join is missing, call get_join_paths instead of guessing.",
			"Use metric expressions from `metrics` verbatim when the question maps to a defined metric.",
			"Apply each table's policy_hints (operator-configured validity/soft-delete/point-in-time filters) exactly as given; do not invent equivalent filters for columns not listed there.",
			"Columns flagged pii=true must not appear in SELECT output; aggregate or omit them.",
			"Always bound exploratory queries with LIMIT n.",
		},
	}
	if len(prohibited) > 0 {
		res["forbidden_joins"] = prohibited
	}
	return res
}

func (c *Catalog) columnsForContext(t *Table, tokens []string, maxColumns int) ([]map[string]any, []string) {
	matches := scoreColumns(tokens, t, maxColumns)
	seen := map[string]bool{}
	out := []map[string]any{}
	add := func(col *Column, score float64) {
		if col == nil || seen[col.Name] || len(out) >= maxColumns {
			return
		}
		seen[col.Name] = true
		entry := map[string]any{
			"name":             col.Name,
			"logical_name":     col.LogicalName,
			"data_type":        col.DataType,
			"length_precision": col.LengthPrecision,
			"nullable":         col.Nullable,
			"is_pk":            col.IsPK,
			"is_fk":            col.IsFK,
			"indexed":          col.Indexed,
			"description":      col.Description,
			"code_dict":        col.CodeDict,
			"score":            round(score),
		}
		if col.PII {
			entry["pii"] = true
		}
		if col.SemanticType != "" {
			entry["semantic_type"] = col.SemanticType
		}
		if len(col.Synonyms) > 0 {
			entry["synonyms"] = col.Synonyms
		}
		if len(col.SampleValues) > 0 {
			entry["sample_values"] = capList(col.SampleValues, 8)
		}
		if col.Stats != nil {
			entry["stats"] = col.Stats
		}
		out = append(out, entry)
	}
	for _, name := range append(append([]string{}, t.PrimaryKeys...), t.ForeignKeys...) {
		add(t.ColumnMap[name], 99)
	}
	// operator-configured policy/well-known columns are always surfaced,
	// since the LLM needs to see them to apply the corresponding filters
	if c.Overrides != nil {
		for _, pair := range c.Overrides.SegmentHistoryColumnPairs {
			add(t.ColumnMap[pair.Start], 80)
			add(t.ColumnMap[pair.End], 80)
		}
		for _, h := range c.Overrides.ValidityFlagColumns {
			add(t.ColumnMap[h], 80)
		}
		for _, h := range c.Overrides.SoftDeleteColumns {
			add(t.ColumnMap[h], 80)
		}
		for _, h := range c.Overrides.WellKnownDateColumns {
			add(t.ColumnMap[h], 80)
		}
		for _, col := range t.Columns {
			for _, prefix := range c.Overrides.ExclusionColumnPrefixes {
				if prefix != "" && strings.HasPrefix(col.Name, prefix) {
					add(col, 80)
				}
			}
		}
	}
	for _, m := range matches {
		add(t.ColumnMap[m.Name], m.Score)
	}
	for _, col := range t.Columns {
		add(col, 0)
	}
	excluded := []string{}
	for _, col := range t.Columns {
		if !seen[col.Name] {
			excluded = append(excluded, col.Name)
		}
	}
	return out, excluded
}

func (c *Catalog) scoreTable(question string, tokens []string, t *Table) (float64, []string, []string) {
	lq := strings.ToLower(question)
	score := 0.0
	reasons := []string{}
	matched := []string{}
	if strings.Contains(lq, strings.ToLower(t.Name)) || strings.Contains(lq, strings.ToLower(t.FQN)) {
		score += 25
		reasons = append(reasons, "explicit table name match")
		matched = append(matched, t.Name)
	}
	if t.LogicalName != "" && strings.Contains(lq, strings.ToLower(t.LogicalName)) {
		score += 20
		reasons = append(reasons, "logical table name match")
		matched = append(matched, t.LogicalName)
	}
	for _, tok := range tokens {
		if tok == "" {
			continue
		}
		lt := strings.ToLower(tok)
		if strings.Contains(strings.ToLower(t.Name), lt) {
			score += 4
			reasons = appendReason(reasons, "table identifier token: "+tok)
			matched = append(matched, tok)
		}
		if t.LogicalName != "" && strings.Contains(strings.ToLower(t.LogicalName), lt) {
			score += 5
			reasons = appendReason(reasons, "logical table token: "+tok)
			matched = append(matched, tok)
		}
		if t.Description != "" && strings.Contains(strings.ToLower(t.Description), lt) {
			score += 3
			reasons = appendReason(reasons, "table description token: "+tok)
			matched = append(matched, tok)
		}
		if t.Domain != "" && strings.Contains(strings.ToLower(t.Domain), lt) {
			score += 6
			reasons = appendReason(reasons, "business domain match: "+tok)
			matched = append(matched, tok)
		}
	}
	// operator-configured keyword→schema hints (see Overrides.SchemaHints);
	// no-op unless the dataset configures them
	hintRules, _ := c.MatchSchemaHints(question)
	for _, rule := range hintRules {
		if contains(rule.Schemas, t.Schema) {
			score += 8
			reasons = appendReason(reasons, "schema hint: "+strings.Join(rule.Keywords, "/"))
		}
	}
	return score, trimReasons(reasons, 8), matched
}

func scoreColumns(tokens []string, t *Table, limit int) []ColumnMatch {
	matches := []ColumnMatch{}
	for _, col := range t.Columns {
		score := 0.0
		search := strings.ToLower(strings.Join([]string{col.Name, col.LogicalName, col.Description, col.CodeDict, col.CommonCode,
			strings.Join(col.Synonyms, " "), strings.Join(col.SampleValues, " ")}, " "))
		for _, tok := range tokens {
			lt := strings.ToLower(tok)
			if lt == "" {
				continue
			}
			if strings.EqualFold(col.Name, tok) {
				score += 15
			} else if strings.Contains(strings.ToLower(col.Name), lt) {
				score += 5
			}
			if col.LogicalName != "" && strings.Contains(strings.ToLower(col.LogicalName), lt) {
				score += 7
			}
			if col.Description != "" && strings.Contains(strings.ToLower(col.Description), lt) {
				score += 2.5
			}
			if col.CodeDict != "" && strings.Contains(strings.ToLower(col.CodeDict), lt) {
				score += 2
			}
			for _, syn := range col.Synonyms {
				if strings.Contains(strings.ToLower(syn), lt) {
					score += 6
					break
				}
			}
			for _, sv := range col.SampleValues {
				if strings.EqualFold(sv, tok) {
					score += 5
					break
				}
			}
			if strings.Contains(search, lt) {
				score += 0.5
			}
		}
		if col.IsPK {
			score += 0.25
		}
		if col.Indexed {
			score += 0.25
		}
		if score > 0 {
			matches = append(matches, ColumnMatch{
				Name:        col.Name,
				LogicalName: col.LogicalName,
				DataType:    col.DataType,
				Description: col.Description,
				Score:       round(score),
				CodeDict:    col.CodeDict,
				IsPK:        col.IsPK,
				IsFK:        col.IsFK,
			})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].Score == matches[j].Score {
			return matches[i].Name < matches[j].Name
		}
		return matches[i].Score > matches[j].Score
	})
	if limit > 0 && len(matches) > limit {
		matches = matches[:limit]
	}
	return matches
}

func (c *Catalog) SearchSamples(question string, topK int, table string) map[string]any {
	if topK <= 0 {
		topK = 5
	}
	tokens := c.expandTokens(tokenize(question))
	sig := c.IntentSignature(question)
	table = strings.ToUpper(strings.TrimSpace(table))
	type scored struct {
		Sample
		Score         float64  `json:"score"`
		IntentOverlap []string `json:"intent_overlap,omitempty"`
	}
	out := []scored{}
	for _, s := range c.Samples {
		if table != "" && !strings.Contains(strings.ToUpper(s.TargetTable), table) {
			continue
		}
		text := strings.ToLower(strings.Join([]string{s.Question, s.TargetDomain, s.TargetTable, s.TargetColumn, s.TargetIntent}, " "))
		score := 0.0
		for _, tok := range tokens {
			if strings.Contains(text, strings.ToLower(tok)) {
				score += 1
			}
		}
		if strings.Contains(strings.ToLower(s.Question), strings.ToLower(question)) && question != "" {
			score += 10
		}
		// structural similarity: shared intent tokens with the dataset's
		// target_intent rank examples with the same SQL shape higher.
		var overlap []string
		if len(sig) > 0 && s.TargetIntent != "" {
			sampleIntents := map[string]bool{}
			for _, tok := range strings.Split(strings.ToLower(s.TargetIntent), "|") {
				sampleIntents[strings.TrimSpace(tok)] = true
			}
			for _, tok := range sig {
				if sampleIntents[tok] {
					overlap = append(overlap, tok)
				}
			}
			score += 2 * float64(len(overlap))
		}
		if score > 0 {
			out = append(out, scored{Sample: s, Score: round(score), IntentOverlap: overlap})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score == out[j].Score {
			return out[i].Question < out[j].Question
		}
		return out[i].Score > out[j].Score
	})
	if len(out) > topK {
		out = out[:topK]
	}
	return map[string]any{"question": question, "intent_signature": sig, "examples": out}
}

func (c *Catalog) Summary() CatalogSummary {
	schemas := map[string]int{}
	cols := 0
	for _, t := range c.Tables {
		schemas[t.Schema]++
		cols += len(t.Columns)
	}
	return CatalogSummary{
		TableCount:    len(c.Tables),
		ColumnCount:   cols,
		RelationCount: len(c.Relations),
		SampleCount:   len(c.Samples),
		Schemas:       schemas,
		LoadedAt:      c.LoadedAt.Format("2006-01-02T15:04:05Z07:00"),
		DataDir:       c.DataDir,
	}
}

// ResolveTable looks up a table by fully-qualified ("schema.table") or bare
// name. A bare name that matches exactly one table always resolves. A bare
// name that matches the SAME table name in multiple schemas is genuinely
// ambiguous: rather than silently picking one, it resolves only if the
// operator has configured a PreferredSchemaOrder (Overrides) that names one
// of the candidate schemas — otherwise it returns not-found so the caller
// surfaces "table not found/ambiguous" and the LLM must re-specify the
// schema instead of silently querying the wrong one.
func (c *Catalog) ResolveTable(name string) (*Table, bool) {
	n := cleanIdent(strings.ReplaceAll(name, "/", "."))
	if n == "" {
		return nil, false
	}
	if strings.Contains(n, ".") {
		if t := c.Tables[n]; t != nil {
			return t, true
		}
	}
	list := c.ByName[n]
	if len(list) == 1 {
		return list[0], true
	}
	if len(list) > 1 && c.Overrides != nil {
		for _, schema := range c.Overrides.PreferredSchemaOrder {
			for _, t := range list {
				if t.Schema == schema {
					return t, true
				}
			}
		}
	}
	return nil, false
}

func (c *Catalog) policyHints(t *Table) []string {
	var out []string
	if c.Overrides == nil {
		return out
	}
	for _, pair := range c.Overrides.SegmentHistoryColumnPairs {
		if t.ColumnMap[pair.Start] != nil && t.ColumnMap[pair.End] != nil {
			out = append(out, "requires point-in-time filter: "+pair.Start+" <= 기준일 AND "+pair.End+" > 기준일")
		}
	}
	for _, col := range c.Overrides.ValidityFlagColumns {
		if t.ColumnMap[col] != nil {
			out = append(out, "apply COALESCE("+col+", 'Y') <> 'N' unless explicitly querying unused rows")
		}
	}
	for _, col := range c.Overrides.SoftDeleteColumns {
		if t.ColumnMap[col] != nil {
			out = append(out, "apply "+col+" IS NULL unless deleted rows are requested")
		}
	}
	for _, col := range t.Columns {
		for _, prefix := range c.Overrides.ExclusionColumnPrefixes {
			if prefix != "" && strings.HasPrefix(col.Name, prefix) {
				out = append(out, "apply "+col.Name+" IS NULL for analytics exclusion filtering")
			}
		}
	}
	for _, rule := range c.Overrides.SchemaHints {
		if rule.Note != "" && contains(rule.Schemas, t.Schema) {
			out = append(out, rule.Note)
		}
	}
	return out
}

func tokenize(s string) []string {
	replacer := strings.NewReplacer("_", " ", ".", " ", ",", " ", "|", " ", "(", " ", ")", " ", "/", " ", "-", " ")
	s = replacer.Replace(strings.ToLower(s))
	var tokens []string
	var b strings.Builder
	flush := func() {
		if b.Len() == 0 {
			return
		}
		t := stripKoreanSuffix(b.String())
		if len([]rune(t)) >= 1 {
			tokens = append(tokens, t)
		}
		b.Reset()
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '$' || r == '#' {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return unique(tokens)
}

func stripKoreanSuffix(s string) string {
	for _, suf := range []string{"에서는", "에서", "으로", "로", "에게", "까지", "부터", "마다", "별로", "별", "동안", "간", "을", "를", "이", "가", "은", "는", "의", "와", "과", "도", "만"} {
		if strings.HasSuffix(s, suf) && len([]rune(s)) > len([]rune(suf))+1 {
			return strings.TrimSuffix(s, suf)
		}
	}
	return s
}

func appendReason(reasons []string, reason string) []string {
	for _, r := range reasons {
		if r == reason {
			return reasons
		}
	}
	return append(reasons, reason)
}

func trimReasons(reasons []string, n int) []string {
	if len(reasons) > n {
		return reasons[:n]
	}
	return reasons
}

func unique(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func round(v float64) float64 {
	return math.Round(v*100) / 100
}

var yyyyMMddRE = regexp.MustCompile(`\b(20\d{2})[-./]?(0[1-9]|1[0-2])[-./]?([0-2]\d|3[01])\b`)
