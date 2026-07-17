package catalog

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// MetricDef is a curated business-metric definition. SQL generation must use
// these expressions instead of letting the LLM invent formulas.
type MetricDef struct {
	Name               string   `json:"name"`
	BusinessName       string   `json:"business_name,omitempty"`
	Aliases            []string `json:"aliases,omitempty"`
	Description        string   `json:"description,omitempty"`
	Expression         string   `json:"expression"`
	Aggregation        string   `json:"aggregation,omitempty"`
	Tables             []string `json:"tables,omitempty"`
	Columns            []string `json:"columns,omitempty"`
	AllowedGrains      []string `json:"allowed_grains,omitempty"`
	RecommendedGroupBy []string `json:"recommended_group_by,omitempty"`
	RequiredFilters    []string `json:"required_filters,omitempty"`
	Exclusions         []string `json:"exclusions,omitempty"`
	NullHandling       string   `json:"null_handling,omitempty"`
	DedupKey           string   `json:"dedup_key,omitempty"`
	ExampleSQL         string   `json:"example_sql,omitempty"`
}

// metricSemanticConfidenceThreshold is deliberately conservative: semantic
// matches must cover at least three quarters of a metric label (and every
// token for labels of up to three tokens), within a small token window. Exact
// name, business name, and alias mentions bypass this threshold and rank first.
const metricSemanticConfidenceThreshold = 0.80

// MetricMatchEvidence explains why a dictionary metric matched user text.
// MetricDefinition includes this alongside its backward-compatible
// definitions array so callers can audit non-literal resolutions.
type MetricMatchEvidence struct {
	MetricName   string   `json:"metric_name"`
	Confidence   float64  `json:"confidence"`
	MatchType    string   `json:"match_type"` // exact | exact_phrase | semantic
	MatchedField string   `json:"matched_field"`
	MatchedLabel string   `json:"matched_label"`
	Evidence     []string `json:"evidence"`
}

type resolvedMetric struct {
	Definition MetricDef
	Match      MetricMatchEvidence
	score      float64
	index      int
}

type metricLabelScore struct {
	confidence float64
	matchType  string
	field      string
	label      string
	evidence   []string
}

func loadMetrics(dataDir string) ([]MetricDef, []LoadIssue) {
	path := filepath.Join(dataDir, "metrics.json")
	if _, err := os.Stat(path); err != nil {
		return nil, []LoadIssue{{Level: "warning", Source: "metrics.json", Message: "metrics.json not found; metric lookups fall back to naming-convention inference"}}
	}
	var defs []MetricDef
	if err := readJSON(path, &defs); err != nil {
		return nil, []LoadIssue{{Level: "error", Source: "metrics.json", Message: err.Error()}}
	}
	var issues []LoadIssue
	for _, d := range defs {
		if strings.TrimSpace(d.Name) == "" || strings.TrimSpace(d.Expression) == "" {
			issues = append(issues, LoadIssue{Level: "error", Source: "metrics.json", Message: "metric requires name and expression: " + d.Name})
		}
	}
	return defs, issues
}

// validateMetrics cross-checks metric table/column references against the
// compiled catalog so broken definitions surface at startup.
func (c *Catalog) validateMetrics() {
	for _, m := range c.Metrics {
		for _, tn := range m.Tables {
			t, ok := c.ResolveTable(tn)
			if !ok {
				c.Issues = append(c.Issues, LoadIssue{Level: "error", Source: "metrics.json", Message: "metric '" + m.Name + "' references unknown table", Table: tn})
				continue
			}
			for _, col := range m.Columns {
				cn := cleanIdent(col)
				if strings.Contains(cn, ".") {
					continue // qualified elsewhere
				}
				if t.ColumnMap[cn] == nil {
					c.Issues = append(c.Issues, LoadIssue{Level: "warning", Source: "metrics.json", Message: "metric '" + m.Name + "' column not found in " + t.FQN, Column: cn})
				}
			}
		}
	}
}

// LookupMetrics resolves dictionary metrics through the same deterministic
// resolver used for question detection and schema-search metric boosts.
// Exact labels rank before semantic token/glossary matches.
func (c *Catalog) LookupMetrics(term string) []MetricDef {
	matches := c.resolveMetricMatches(term)
	out := make([]MetricDef, 0, len(matches))
	for _, match := range matches {
		out = append(out, match.Definition)
	}
	return out
}

// MetricNamesInQuestion returns dictionary metric names resolved from the
// question, including high-confidence glossary/token-overlap matches.
func (c *Catalog) MetricNamesInQuestion(question string) []string {
	matches := c.resolveMetricMatches(question)
	out := make([]string, 0, len(matches))
	for _, match := range matches {
		out = append(out, match.Definition.Name)
	}
	return out
}

// resolveMetricMatches is the single source of truth for metric resolution.
// It combines curated labels, glossary synonym groups, and local token
// overlap without external models or non-deterministic state.
func (c *Catalog) resolveMetricMatches(text string) []resolvedMetric {
	inputTokens := tokenize(text)
	if len(inputTokens) == 0 {
		return nil
	}
	inputNormalized := strings.Join(inputTokens, " ")
	matches := make([]resolvedMetric, 0)
	for i, metric := range c.Metrics {
		labels := []struct {
			field string
			value string
		}{{"name", metric.Name}, {"business_name", metric.BusinessName}}
		for _, alias := range metric.Aliases {
			labels = append(labels, struct {
				field string
				value string
			}{"alias", alias})
		}

		var best metricLabelScore
		for _, label := range labels {
			score := c.scoreMetricLabel(inputTokens, inputNormalized, label.field, label.value)
			if score.confidence > best.confidence {
				best = score
			}
		}
		if best.confidence == 0 {
			continue
		}
		matches = append(matches, resolvedMetric{
			Definition: metric,
			Match: MetricMatchEvidence{
				MetricName:   metric.Name,
				Confidence:   round(best.confidence),
				MatchType:    best.matchType,
				MatchedField: best.field,
				MatchedLabel: best.label,
				Evidence:     best.evidence,
			},
			score: best.confidence,
			index: i,
		})
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].score == matches[j].score {
			return matches[i].index < matches[j].index
		}
		return matches[i].score > matches[j].score
	})
	return matches
}

func (c *Catalog) scoreMetricLabel(inputTokens []string, inputNormalized, field, label string) metricLabelScore {
	labelTokens := tokenize(label)
	if len(labelTokens) == 0 {
		return metricLabelScore{}
	}
	labelNormalized := strings.Join(labelTokens, " ")
	fieldPenalty := map[string]float64{"name": 0, "business_name": 0.01, "alias": 0.02}[field]
	if inputNormalized == labelNormalized {
		return metricLabelScore{
			confidence: 1.0 - fieldPenalty,
			matchType:  "exact",
			field:      field,
			label:      label,
			evidence:   []string{"exact " + field + " label: " + label},
		}
	}
	if metricPhrasePresent(inputTokens, labelTokens) {
		return metricLabelScore{
			confidence: 0.97 - fieldPenalty,
			matchType:  "exact_phrase",
			field:      field,
			label:      label,
			evidence:   []string{"exact " + field + " phrase: " + label},
		}
	}

	inputPositions := make([][]int, len(labelTokens))
	directTokens := make([]string, 0, len(labelTokens))
	for labelIndex, labelToken := range labelTokens {
		for inputIndex, inputToken := range inputTokens {
			if labelToken == inputToken {
				inputPositions[labelIndex] = append(inputPositions[labelIndex], inputIndex)
			}
		}
		if len(inputPositions[labelIndex]) > 0 {
			directTokens = appendUnique(directTokens, labelToken)
		}
	}
	glossaryEvidence := addMetricGlossaryMatches(c.Glossary, labelTokens, inputTokens, inputPositions)

	matched := 0
	for _, positions := range inputPositions {
		if len(positions) > 0 {
			matched++
		}
	}
	required := (3*len(labelTokens) + 3) / 4 // ceil(3/4 * label tokens)
	if len(labelTokens) > 1 && required < 2 {
		required = 2
	}
	if matched < required {
		return metricLabelScore{}
	}
	// A one-token semantic match is allowed only through an explicit glossary
	// relation; direct one-token mentions were handled as exact phrases above.
	if len(labelTokens) == 1 && (len(glossaryEvidence) == 0 || len([]rune(labelTokens[0])) < 2) {
		return metricLabelScore{}
	}
	span := minimumMetricTokenSpan(inputPositions, matched)
	if span == 0 || span > len(labelTokens)+1 {
		return metricLabelScore{}
	}

	coverage := float64(matched) / float64(len(labelTokens))
	directRatio := float64(len(directTokens)) / float64(len(labelTokens))
	confidence := 0.72 + 0.12*coverage + 0.04*directRatio
	if len(glossaryEvidence) > 0 {
		confidence += 0.02
	}
	switch field {
	case "name":
		confidence += 0.02
	case "business_name":
		confidence += 0.01
	}
	if confidence < metricSemanticConfidenceThreshold {
		return metricLabelScore{}
	}
	if confidence > 0.94 {
		confidence = 0.94
	}
	evidence := make([]string, 0, len(glossaryEvidence)+3)
	if len(directTokens) > 0 {
		evidence = append(evidence, "direct tokens: "+strings.Join(directTokens, ", "))
	}
	evidence = append(evidence, glossaryEvidence...)
	evidence = append(evidence,
		fmt.Sprintf("token overlap: %d/%d", matched, len(labelTokens)),
		fmt.Sprintf("token window: %d", span),
	)
	return metricLabelScore{
		confidence: confidence,
		matchType:  "semantic",
		field:      field,
		label:      label,
		evidence:   evidence,
	}
}

type metricPhraseOccurrence struct {
	start int
	width int
}

func metricPhrasePresent(tokens, phrase []string) bool {
	return len(metricPhraseOccurrences(tokens, phrase)) > 0
}

func metricPhraseOccurrences(tokens, phrase []string) []metricPhraseOccurrence {
	if len(tokens) == 0 || len(phrase) == 0 {
		return nil
	}
	var out []metricPhraseOccurrence
	if len(phrase) <= len(tokens) {
		for start := 0; start+len(phrase) <= len(tokens); start++ {
			matched := true
			for offset := range phrase {
				if tokens[start+offset] != phrase[offset] {
					matched = false
					break
				}
			}
			if matched {
				out = append(out, metricPhraseOccurrence{start: start, width: len(phrase)})
			}
		}
	}
	// Korean metric labels are commonly written both spaced and compact
	// ("사용자 수" vs "사용자수"). Match only a whole compact token so a
	// short label cannot fire on an arbitrary substring.
	if len(phrase) > 1 {
		compact := strings.Join(phrase, "")
		for i, token := range tokens {
			if token == compact {
				out = append(out, metricPhraseOccurrence{start: i, width: 1})
			}
		}
	}
	return out
}

func addMetricGlossaryMatches(g *Glossary, labelTokens, inputTokens []string, inputPositions [][]int) []string {
	if g == nil {
		return nil
	}
	var evidence []string
	for _, entry := range g.Entries {
		group := append([]string{entry.Term}, entry.Synonyms...)
		for _, labelMember := range group {
			labelMemberTokens := tokenize(labelMember)
			labelOccurrences := metricPhraseOccurrences(labelTokens, labelMemberTokens)
			if len(labelOccurrences) == 0 {
				continue
			}
			for _, inputMember := range group {
				inputOccurrences := metricPhraseOccurrences(inputTokens, tokenize(inputMember))
				if len(inputOccurrences) == 0 {
					continue
				}
				for _, labelOccurrence := range labelOccurrences {
					for offset := 0; offset < labelOccurrence.width; offset++ {
						labelIndex := labelOccurrence.start + offset
						for _, inputOccurrence := range inputOccurrences {
							inputPositions[labelIndex] = appendUniqueInt(inputPositions[labelIndex], inputOccurrence.start)
						}
					}
				}
				evidence = appendUnique(evidence, "glossary '"+entry.Term+"': "+labelMember+" ↔ "+inputMember)
				break
			}
		}
	}
	return evidence
}

func appendUniqueInt(values []int, value int) []int {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func minimumMetricTokenSpan(positions [][]int, needed int) int {
	type event struct {
		position int
		label    int
	}
	var events []event
	for label, values := range positions {
		for _, position := range values {
			events = append(events, event{position: position, label: label})
		}
	}
	if len(events) == 0 || needed == 0 {
		return 0
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].position == events[j].position {
			return events[i].label < events[j].label
		}
		return events[i].position < events[j].position
	})
	counts := map[int]int{}
	distinct := 0
	best := 0
	left := 0
	for right, current := range events {
		if counts[current.label] == 0 {
			distinct++
		}
		counts[current.label]++
		for distinct >= needed {
			span := events[right].position - events[left].position + 1
			if best == 0 || span < best {
				best = span
			}
			first := events[left]
			counts[first.label]--
			if counts[first.label] == 0 {
				distinct--
			}
			left++
		}
	}
	return best
}

// MetricDefinition returns dictionary definitions first; only when the
// dictionary has no entry does it fall back to naming-convention inference,
// and the two are clearly separated so the caller never confuses curated
// formulas with guesses.
func (c *Catalog) MetricDefinition(metricName string, topK int) map[string]any {
	if topK <= 0 {
		topK = 8
	}
	matches := c.resolveMetricMatches(metricName)
	res := map[string]any{
		"metric_name":          metricName,
		"confidence_threshold": metricSemanticConfidenceThreshold,
		"match_evidence":       []MetricMatchEvidence{},
	}
	if len(matches) > 0 {
		if len(matches) > topK {
			matches = matches[:topK]
		}
		dict := make([]MetricDef, 0, len(matches))
		evidence := make([]MetricMatchEvidence, 0, len(matches))
		for _, match := range matches {
			dict = append(dict, match.Definition)
			evidence = append(evidence, match.Match)
		}
		res["source"] = "dictionary"
		res["definitions"] = dict
		res["match_evidence"] = evidence
		res["note"] = "Curated metric definitions. Use expression, required_filters, and exclusions verbatim; do not invent alternative formulas."
		return res
	}
	res["source"] = "inferred"
	res["definitions"] = []MetricDef{}
	res["inferred_candidates"] = c.inferMetricCandidates(metricName, topK)
	res["note"] = "No dictionary entry found. inferred_candidates are naming-convention guesses over catalog columns; confirm the business formula with the user or an operator before treating any of them as authoritative."
	return res
}

type inferredMetric struct {
	Table       string   `json:"table"`
	Column      string   `json:"column"`
	LogicalName string   `json:"logical_name,omitempty"`
	DataType    string   `json:"data_type,omitempty"`
	Description string   `json:"description,omitempty"`
	Expression  string   `json:"suggested_expression"`
	Notes       []string `json:"notes,omitempty"`
	Score       float64  `json:"score"`
}

func (c *Catalog) inferMetricCandidates(metricName string, topK int) []inferredMetric {
	tokens := c.expandTokens(tokenize(metricName))
	var out []inferredMetric
	for _, t := range c.Tables {
		for _, col := range t.Columns {
			matches := scoreColumns(tokens, &Table{Columns: []*Column{col}}, 1)
			score := 0.0
			if len(matches) > 0 {
				score = matches[0].Score
			}
			if score == 0 && !looksMetricColumn(col.Name) {
				continue
			}
			expr, notes := c.metricExpression(t, col)
			if expr == "" {
				continue
			}
			out = append(out, inferredMetric{
				Table:       t.FQN,
				Column:      col.Name,
				LogicalName: col.LogicalName,
				DataType:    col.DataType,
				Description: col.Description,
				Expression:  expr,
				Notes:       notes,
				Score:       round(score + metricNameBonus(metricName, col)),
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score == out[j].Score {
			if out[i].Table == out[j].Table {
				return out[i].Column < out[j].Column
			}
			return out[i].Table < out[j].Table
		}
		return out[i].Score > out[j].Score
	})
	if len(out) > topK {
		out = out[:topK]
	}
	return out
}
