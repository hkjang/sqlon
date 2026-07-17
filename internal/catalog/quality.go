package catalog

import (
	"regexp"
	"sort"
	"strings"
)

// Metadata quality scoring and the release gate (FR-META-023/024/025). Unlike
// get_catalog_health (which reports load errors/warnings), this scores every
// table across measurable quality dimensions, assigns an A–E grade, aggregates
// by schema/domain, and evaluates the blocking conditions that must stop a
// catalog release.

// QualityDimension weights (sum = 100). Only dimensions computable from the
// compiled catalog are scored; the rest of the spec's dimensions (user trust,
// lineage) attach once those subsystems exist.
type dimensionScore struct {
	Completeness float64 `json:"completeness"` // logical name/description/domain coverage
	Consistency  float64 `json:"consistency"`  // physical/logical type & PK/FK sanity
	Relationship float64 `json:"relationship"` // PK present + join-graph connectivity
	Profiling    float64 `json:"profiling"`    // column stats coverage
	MetricLink   float64 `json:"metric_link"`  // referenced by a defined metric
	Usability    float64 `json:"usability"`    // used in samples/feedback/search text
	Security     float64 `json:"security"`     // PII-likely columns are classified
}

// TableQuality is one table's quality assessment.
type TableQuality struct {
	Table       string         `json:"table"`
	Schema      string         `json:"schema"`
	Domain      string         `json:"domain,omitempty"`
	Score       float64        `json:"score"` // 0..100
	Grade       string         `json:"grade"` // A..E
	Dimensions  dimensionScore `json:"dimensions"`
	Issues      []string       `json:"issues,omitempty"`
	Suggestions []string       `json:"suggestions,omitempty"`
}

// QualityReport is the whole-catalog assessment.
type QualityReport struct {
	OverallScore float64            `json:"overall_score"`
	OverallGrade string             `json:"overall_grade"`
	TableCount   int                `json:"table_count"`
	GradeCounts  map[string]int     `json:"grade_counts"`
	BySchema     map[string]float64 `json:"by_schema"`
	ByDomain     map[string]float64 `json:"by_domain"`
	Tables       []TableQuality     `json:"tables"`
	Improve      []TableQuality     `json:"top_improvement_targets"`
	Weights      map[string]float64 `json:"dimension_weights"`
}

var dimensionWeights = map[string]float64{
	"completeness": 25, "consistency": 15, "relationship": 20,
	"profiling": 15, "metric_link": 5, "usability": 10, "security": 10,
}

// piiLikelyRE flags columns whose NAME suggests personal/credit data, used by
// the security dimension to detect UNCLASSIFIED sensitive columns (a column
// that looks like PII but has pii=false). Mirrors the profiler's classifier.
var piiLikelyRE = regexp.MustCompile(`(?i)(email|e_mail|phone|mobile|ssn|passwd|password|pwd|token|secret|birth|dob|resident|jumin|주민|전화|이메일|성명|이름|passport|card_no|cvv|iban|addr|address|주소)`)

// QualityReport computes per-table quality and aggregates (FR-META-023/024).
func (c *Catalog) QualityReport() QualityReport {
	rep := QualityReport{
		GradeCounts: map[string]int{}, BySchema: map[string]float64{},
		ByDomain: map[string]float64{}, Weights: dimensionWeights,
	}
	metricTables := c.metricTableSet()
	usedTables := c.usageTableSet()

	schemaSum := map[string]float64{}
	schemaN := map[string]int{}
	domainSum := map[string]float64{}
	domainN := map[string]int{}
	var total float64

	for _, t := range c.Tables {
		tq := c.scoreTableQuality(t, metricTables, usedTables)
		rep.Tables = append(rep.Tables, tq)
		rep.GradeCounts[tq.Grade]++
		total += tq.Score
		schemaSum[t.Schema] += tq.Score
		schemaN[t.Schema]++
		if t.Domain != "" {
			domainSum[t.Domain] += tq.Score
			domainN[t.Domain]++
		}
	}
	rep.TableCount = len(rep.Tables)
	if rep.TableCount > 0 {
		rep.OverallScore = round(total / float64(rep.TableCount))
	}
	rep.OverallGrade = grade(rep.OverallScore)
	for s, sum := range schemaSum {
		rep.BySchema[s] = round(sum / float64(schemaN[s]))
	}
	for d, sum := range domainSum {
		rep.ByDomain[d] = round(sum / float64(domainN[d]))
	}
	sort.Slice(rep.Tables, func(i, j int) bool {
		if rep.Tables[i].Score == rep.Tables[j].Score {
			return rep.Tables[i].Table < rep.Tables[j].Table
		}
		return rep.Tables[i].Score < rep.Tables[j].Score // worst first
	})
	// top improvement targets: the lowest-scoring tables with concrete fixes
	for _, t := range rep.Tables {
		if t.Score < 75 && len(t.Suggestions) > 0 {
			rep.Improve = append(rep.Improve, t)
		}
		if len(rep.Improve) >= 10 {
			break
		}
	}
	return rep
}

func (c *Catalog) scoreTableQuality(t *Table, metricTables, usedTables map[string]bool) TableQuality {
	tq := TableQuality{Table: t.FQN, Schema: t.Schema, Domain: t.Domain}
	d := &tq.Dimensions

	// completeness: logical name + description at table level, plus column
	// logical-name/description coverage
	compParts := 0.0
	if t.LogicalName != "" {
		compParts += 0.25
	} else {
		tq.Issues = append(tq.Issues, "no logical name")
		tq.Suggestions = append(tq.Suggestions, "add a logical (business) name for "+t.FQN)
	}
	if t.Description != "" {
		compParts += 0.25
	} else {
		tq.Suggestions = append(tq.Suggestions, "add a table description")
	}
	if t.Domain != "" {
		compParts += 0.1
	}
	colNamed, colDesc := 0, 0
	for _, col := range t.Columns {
		if col.LogicalName != "" {
			colNamed++
		}
		if col.Description != "" {
			colDesc++
		}
	}
	if len(t.Columns) > 0 {
		compParts += 0.25 * ratio(colNamed, len(t.Columns))
		compParts += 0.15 * ratio(colDesc, len(t.Columns))
		if colNamed < len(t.Columns) {
			tq.Suggestions = append(tq.Suggestions, "name the remaining "+itoa(len(t.Columns)-colNamed)+" columns")
		}
	}
	d.Completeness = round(compParts * 100)

	// consistency: every column has a data type; PK columns are non-null-ish
	typed := 0
	for _, col := range t.Columns {
		if col.DataType != "" {
			typed++
		}
	}
	consistency := 1.0
	if len(t.Columns) > 0 {
		consistency = ratio(typed, len(t.Columns))
	}
	if typed < len(t.Columns) {
		tq.Issues = append(tq.Issues, itoa(len(t.Columns)-typed)+" columns missing a data type")
	}
	d.Consistency = round(consistency * 100)

	// relationship: has a primary key + is connected in the join graph
	rel := 0.0
	if len(t.PrimaryKeys) > 0 {
		rel += 0.5
	} else {
		tq.Issues = append(tq.Issues, "no primary key")
		tq.Suggestions = append(tq.Suggestions, "declare a primary key (or a preferred join key) for "+t.FQN)
	}
	if len(c.Adjacency[t.FQN]) > 0 {
		rel += 0.5
	} else if len(c.Tables) > 1 {
		tq.Suggestions = append(tq.Suggestions, "no join relations — run suggest_joins to find candidates")
	}
	d.Relationship = round(rel * 100)

	// profiling: column-stats coverage
	statCols := 0
	for _, col := range t.Columns {
		if col.Stats != nil {
			statCols++
		}
	}
	if len(t.Columns) > 0 {
		d.Profiling = round(ratio(statCols, len(t.Columns)) * 100)
	}
	if statCols == 0 && len(t.Columns) > 0 {
		tq.Suggestions = append(tq.Suggestions, "no column statistics — run profile_metadata_assets")
	}

	// metric linkage
	if metricTables[t.FQN] {
		d.MetricLink = 100
	}

	// usability
	if usedTables[t.FQN] {
		d.Usability = 100
	}

	// security: any PII-likely-named column must be classified pii=true
	unclassified := 0
	for _, col := range t.Columns {
		if piiLikelyRE.MatchString(col.Name) && !col.PII {
			unclassified++
		}
	}
	if unclassified == 0 {
		d.Security = 100
	} else {
		d.Security = 0
		tq.Issues = append(tq.Issues, itoa(unclassified)+" PII-likely columns not classified (pii=false)")
		tq.Suggestions = append(tq.Suggestions, "classify sensitive columns via overrides.json pii_columns")
	}

	tq.Score = round(
		d.Completeness*dimensionWeights["completeness"]/100 +
			d.Consistency*dimensionWeights["consistency"]/100 +
			d.Relationship*dimensionWeights["relationship"]/100 +
			d.Profiling*dimensionWeights["profiling"]/100 +
			d.MetricLink*dimensionWeights["metric_link"]/100 +
			d.Usability*dimensionWeights["usability"]/100 +
			d.Security*dimensionWeights["security"]/100)
	tq.Grade = grade(tq.Score)
	return tq
}

// metricTableSet returns FQNs referenced by any dictionary metric.
func (c *Catalog) metricTableSet() map[string]bool {
	out := map[string]bool{}
	for _, m := range c.Metrics {
		for _, tn := range m.Tables {
			if t, ok := c.ResolveTable(tn); ok {
				out[t.FQN] = true
			}
		}
	}
	return out
}

// usageTableSet returns FQNs used in golden samples or successful feedback.
func (c *Catalog) usageTableSet() map[string]bool {
	out := map[string]bool{}
	for fqn, n := range c.FeedbackUsage {
		if n > 0 {
			out[fqn] = true
		}
	}
	for _, s := range c.Samples {
		if t, ok := c.ResolveTable(s.TargetTable); ok {
			out[t.FQN] = true
		}
	}
	return out
}

// ---- release gate (FR-META-025) ----

// GateViolation is one blocking condition.
type GateViolation struct {
	Code     string `json:"code"`
	Severity string `json:"severity"` // block | warn
	Message  string `json:"message"`
	Detail   string `json:"detail,omitempty"`
}

// QualityGateResult reports whether a catalog release is allowed.
type QualityGateResult struct {
	Pass         bool            `json:"pass"`
	OverallScore float64         `json:"overall_score"`
	OverallGrade string          `json:"overall_grade"`
	Violations   []GateViolation `json:"violations"`
	Note         string          `json:"note"`
}

// minReleaseScore is the default overall-score floor for a release. Operators
// can tune it via overrides (future); for now it is a conservative constant.
const minReleaseScore = 60

// QualityGate evaluates the blocking conditions that must stop a catalog
// release (FR-META-025). A release is blocked when any severity=block
// violation is present.
func (c *Catalog) QualityGate() QualityGateResult {
	res := QualityGateResult{Pass: true}
	rep := c.QualityReport()
	res.OverallScore = rep.OverallScore
	res.OverallGrade = rep.OverallGrade

	// 1) required models / reference errors: any load ERROR blocks
	for _, iss := range c.Issues {
		if iss.Level == "error" {
			res.Violations = append(res.Violations, GateViolation{
				Code: "LOAD_ERROR", Severity: "block",
				Message: "catalog load error: " + iss.Message,
				Detail:  strings.TrimSpace(iss.Table + " " + iss.Column),
			})
		}
	}
	// 2) zero tables compiled
	if len(c.Tables) == 0 {
		res.Violations = append(res.Violations, GateViolation{
			Code: "NO_TABLES", Severity: "block", Message: "no tables compiled from metadata"})
	}
	// 3) metric integrity: every dictionary metric must reference resolvable
	// tables (a metric pointing at a dropped table breaks generated SQL)
	for _, m := range c.Metrics {
		for _, tn := range m.Tables {
			if _, ok := c.ResolveTable(tn); !ok {
				res.Violations = append(res.Violations, GateViolation{
					Code: "METRIC_BROKEN", Severity: "block",
					Message: "metric '" + m.Name + "' references a missing table",
					Detail:  tn})
			}
		}
	}
	// 4) preferred (certified) join integrity: an operator-preferred relation
	// whose endpoints no longer resolve is a broken certified join
	for _, r := range c.Relations {
		if !r.Preferred {
			continue
		}
		if _, ok := c.ResolveTable(r.BaseSchema + "." + r.BaseTable); !ok {
			res.Violations = append(res.Violations, GateViolation{
				Code: "JOIN_BROKEN", Severity: "block",
				Message: "certified join references a missing table",
				Detail:  r.BaseSchema + "." + r.BaseTable})
		}
		if _, ok := c.ResolveTable(r.ReferenceSchema + "." + r.ReferenceTable); !ok {
			res.Violations = append(res.Violations, GateViolation{
				Code: "JOIN_BROKEN", Severity: "block",
				Message: "certified join references a missing table",
				Detail:  r.ReferenceSchema + "." + r.ReferenceTable})
		}
	}
	// 5) PII classification: unclassified PII-likely columns are a compliance
	// block (FR-META-025 PII 미분류)
	var unclassified []string
	for _, t := range c.Tables {
		for _, col := range t.Columns {
			if piiLikelyRE.MatchString(col.Name) && !col.PII {
				unclassified = append(unclassified, t.FQN+"."+col.Name)
			}
		}
	}
	if len(unclassified) > 0 {
		sort.Strings(unclassified)
		res.Violations = append(res.Violations, GateViolation{
			Code: "PII_UNCLASSIFIED", Severity: "block",
			Message: itoa(len(unclassified)) + " PII-likely columns are not classified",
			Detail:  strings.Join(capList(unclassified, 10), ", ")})
	}
	// 6) overall quality floor
	if rep.OverallScore < minReleaseScore {
		res.Violations = append(res.Violations, GateViolation{
			Code: "QUALITY_FLOOR", Severity: "block",
			Message: "overall metadata quality below release floor",
			Detail:  "score " + ftoa(rep.OverallScore) + " < " + itoa(minReleaseScore)})
	}

	for _, v := range res.Violations {
		if v.Severity == "block" {
			res.Pass = false
		}
	}
	if res.Pass {
		res.Note = "품질 게이트 통과 — 릴리스 가능"
	} else {
		res.Note = "차단 조건이 있어 릴리스가 금지됩니다. violations를 해소한 뒤 재평가하세요."
	}
	return res
}

// ---- helpers ----

func grade(score float64) string {
	switch {
	case score >= 90:
		return "A"
	case score >= 75:
		return "B"
	case score >= 60:
		return "C"
	case score >= 40:
		return "D"
	default:
		return "E"
	}
}

func ftoa(f float64) string {
	whole := int(f)
	frac := int((f-float64(whole))*10 + 0.5)
	if frac >= 10 {
		whole++
		frac = 0
	}
	return itoa(whole) + "." + itoa(frac)
}
