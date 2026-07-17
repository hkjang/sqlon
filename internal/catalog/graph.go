package catalog

import (
	"sort"
)

// Graph-expanded retrieval unifies the signals that already live in the
// catalog — lexical/semantic search, the join graph, business terms, metric
// definitions, value samples, and feedback usage — into one hybrid-ranked
// context, following the GraphRAG "seed → expand → re-rank" design. It is pure
// and read-only; nothing here queries a DB.

// retrieval re-ranking weights (design §4), tuned against the golden set with
// EvaluateRetrieval: semantic rank must stay dominant — with flat weights the
// auxiliary signals reordered seeds and dropped table recall@5 from 0.95 to
// 0.70. Auxiliary signals act as tie-breakers and neighbor discovery, not as
// primary ranking.
const (
	wSemantic = 0.55 // rank-decayed search position (dominant)
	wLexical  = 0.10 // exact table/column/term string hits
	wProxim   = 0.08 // graph proximity: seed vs join-expanded
	wJoin     = 0.09 // joinable to another candidate table
	wValue    = 0.10 // a question literal maps to a column here
	wUsage    = 0.04 // past successful-feedback usage prior
	wFresh    = 0.04 // freshness / not penalized
)

// TableCandidate is one ranked table with its per-signal provenance.
type TableCandidate struct {
	Table       string             `json:"table"`
	LogicalName string             `json:"logical_name,omitempty"`
	Domain      string             `json:"domain,omitempty"`
	Grain       string             `json:"grain,omitempty"`
	Score       float64            `json:"score"`
	Origin      string             `json:"origin"` // seed | join_expanded
	Signals     map[string]float64 `json:"signals"`
	Columns     []string           `json:"candidate_columns,omitempty"`
	Reasons     []string           `json:"reasons,omitempty"`
}

// RetrievalResult is the /retrieve-style bundle: the ranked table context plus
// the value/term/join evidence and a trace of what fed the ranking.
type RetrievalResult struct {
	Question  string                  `json:"question"`
	Tables    []TableCandidate        `json:"tables"`
	JoinPaths map[string]any          `json:"join_paths,omitempty"`
	Terms     []string                `json:"business_terms,omitempty"`
	Metrics   []string                `json:"metrics,omitempty"`
	Values    []FilterColumnCandidate `json:"value_evidence,omitempty"`
	Trace     map[string]any          `json:"trace"`
}

// RetrieveContext runs the graph-expanded, hybrid-ranked retrieval for a
// question and returns the top-k tables with provenance.
func (c *Catalog) RetrieveContext(question string, topK int) RetrievalResult {
	if topK <= 0 {
		topK = 6
	}
	// Seed search uses the SAME parameters as a plain top-k search — MaxColumns
	// and TopK feed the scoring (column-match boost, join-connectivity pool),
	// so different values would silently reorder seeds vs the baseline.
	search := c.SearchSchema(SearchRequest{Question: question, TopK: topK, IncludeColumns: true})

	// --- seed layer: search hits carry the semantic + lexical signals ---
	cand := map[string]*TableCandidate{}
	maxScore := 0.0
	for _, r := range search.Results {
		if r.Score > maxScore {
			maxScore = r.Score
		}
	}
	if maxScore == 0 {
		maxScore = 1
	}
	for i, r := range search.Results {
		tc := &TableCandidate{
			Table: r.Table, LogicalName: r.LogicalName, Domain: r.Domain, Grain: r.Grain,
			Origin: "seed", Signals: map[string]float64{}, Reasons: append([]string{}, r.Reasons...),
		}
		// rank-decayed semantic: raw scores cluster tightly (e.g. 66 vs 58),
		// so score ratios let small boosts flip order; rank decay keeps the
		// search ordering dominant while still comparable across questions.
		tc.Signals["semantic"] = 1.0 / (1.0 + 0.35*float64(i))
		tc.Signals["score_ratio"] = round(r.Score / maxScore)
		tc.Signals["lexical"] = clamp01(float64(len(r.MatchedTerms)) / 3.0)
		tc.Signals["proximity"] = 1.0 // seeds are at distance 0
		for _, mc := range r.MatchedColumns {
			tc.Columns = appendUnique(tc.Columns, mc.Name)
		}
		cand[r.Table] = tc
	}

	// --- graph expansion: 1-hop join neighbors of the strongest seeds ---
	seedList := make([]string, 0, len(cand))
	for t := range cand {
		seedList = append(seedList, t)
	}
	for _, seed := range seedList {
		if cand[seed].Signals["semantic"] < 0.4 {
			continue // only expand from confident seeds to avoid drift
		}
		for _, edge := range c.Adjacency[seed] {
			nb := edge.To
			if _, ok := cand[nb]; ok {
				cand[nb].Signals["join"] = 1.0 // mutually joinable to a seed
				continue
			}
			t, ok := c.ResolveTable(nb)
			if !ok {
				continue
			}
			tc := &TableCandidate{
				Table: t.FQN, LogicalName: t.LogicalName, Domain: t.Domain, Grain: t.Grain,
				Origin: "join_expanded", Signals: map[string]float64{"proximity": 0.3, "join": 1.0},
				Reasons: []string{"joinable with " + seed},
			}
			cand[nb] = tc
		}
	}

	// --- value linking: question literals → column value evidence ---
	valueTokens := []string{}
	for _, tok := range tokenize(question) {
		if len([]rune(tok)) >= 2 && !numericRE.MatchString(tok) {
			valueTokens = append(valueTokens, tok)
		}
	}
	values := []FilterColumnCandidate{}
	if len(valueTokens) > 0 {
		res := c.FindFilterColumns(valueTokens, nil, 12)
		if list, ok := res["candidates"].([]FilterColumnCandidate); ok {
			values = list
			for _, v := range list {
				if tc, ok := cand[v.Table]; ok {
					tc.Signals["value"] = 1.0
					tc.Columns = appendUnique(tc.Columns, v.Column)
					tc.Reasons = appendUnique(tc.Reasons, "값 증거: '"+v.Value+"' → "+v.Column)
				}
			}
		}
	}

	// --- priors: usage (feedback) and freshness/penalty ---
	for t, tc := range cand {
		if c.FeedbackUsage != nil && c.FeedbackUsage[t] > 0 {
			tc.Signals["usage"] = clamp01(float64(c.FeedbackUsage[t]) / 5.0)
		}
		fresh := 1.0
		if c.FeedbackPenalty != nil && c.FeedbackPenalty[t] > 0 {
			fresh -= clamp01(float64(c.FeedbackPenalty[t]) / 5.0)
		}
		tc.Signals["freshness"] = fresh
	}

	// --- hybrid score ---
	for _, tc := range cand {
		s := tc.Signals
		tc.Score = round(wSemantic*s["semantic"] + wLexical*s["lexical"] + wProxim*s["proximity"] +
			wJoin*s["join"] + wValue*s["value"] + wUsage*s["usage"] + wFresh*s["freshness"])
	}

	// Order-preserving fusion, tuned against the golden set: plain search
	// already ranks this catalog extremely well (table recall@5 = 0.95), and
	// every re-ranking variant we measured regressed it (flat weights → 0.70,
	// rank-decayed → 0.89). So seeds keep their search order verbatim, and the
	// graph layer contributes what search cannot: join-expanded neighbors and
	// value-evidence tables, appended after the seeds (up to 2). The hybrid
	// Score stays on every candidate as provenance for tuning and display.
	ranked := make([]TableCandidate, 0, topK+2)
	seen := map[string]bool{}
	for _, r := range search.Results {
		if len(ranked) >= topK {
			break
		}
		if tc, ok := cand[r.Table]; ok && !seen[r.Table] {
			ranked = append(ranked, *tc)
			seen[r.Table] = true
		}
	}
	// discoveries: best-scored candidates not already included
	extras := make([]TableCandidate, 0, len(cand))
	for _, tc := range cand {
		if !seen[tc.Table] {
			extras = append(extras, *tc)
		}
	}
	sort.SliceStable(extras, func(i, j int) bool {
		if extras[i].Score == extras[j].Score {
			return extras[i].Table < extras[j].Table
		}
		return extras[i].Score > extras[j].Score
	})
	for i := 0; i < len(extras) && i < 2; i++ {
		e := extras[i]
		e.Reasons = appendUnique(e.Reasons, "graph discovery (append-only)")
		ranked = append(ranked, e)
	}

	// enrich column evidence post-ranking (never feeds the ordering): deeper
	// per-table column matching than the seed search attaches by default
	qTokens := c.expandTokens(tokenize(question))
	for i := range ranked {
		if t, ok := c.ResolveTable(ranked[i].Table); ok {
			for _, m := range scoreColumns(qTokens, t, 12) {
				ranked[i].Columns = appendUnique(ranked[i].Columns, m.Name)
			}
		}
	}

	// join paths among the selected tables
	joinPaths := map[string]any{}
	selected := make([]string, 0, len(ranked))
	for _, tc := range ranked {
		selected = append(selected, tc.Table)
	}
	if len(selected) >= 2 {
		if jp, err := c.GetJoinPaths(JoinPathRequest{Tables: selected}); err == nil {
			joinPaths = jp
		}
	}

	metrics := c.MetricNamesInQuestion(question)
	return RetrievalResult{
		Question:  question,
		Tables:    ranked,
		JoinPaths: joinPaths,
		Terms:     search.Tokens,
		Metrics:   metrics,
		Values:    values,
		Trace: map[string]any{
			"seed_count":      len(seedList),
			"expanded_count":  len(cand) - len(seedList),
			"weights":         map[string]float64{"semantic": wSemantic, "lexical": wLexical, "proximity": wProxim, "join": wJoin, "value": wValue, "usage": wUsage, "freshness": wFresh},
			"value_tokens":    valueTokens,
			"glossary_expand": search.Tokens,
		},
	}
}

// RankedTables returns just the ordered FQNs — used by PrepareContext to pick
// tables via the graph retriever instead of raw search order.
func (c *Catalog) RankedTables(question string, topK int) []string {
	out := []string{}
	for _, tc := range c.RetrieveContext(question, topK).Tables {
		out = append(out, tc.Table)
	}
	return out
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
