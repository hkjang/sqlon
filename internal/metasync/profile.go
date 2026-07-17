package metasync

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Automated, cost-controlled, privacy-preserving data profiling
// (FR-META-006/007/008). Physical statistics (row/null/distinct counts,
// min/max, top values, format patterns) are computed with bounded cost and
// with strict privacy rules for sensitive columns, and stored as a REVIEWABLE
// profile result — never applied straight to the operational catalog.

// ProfileMode selects the depth/cost trade-off (FR-META-006).
type ProfileMode string

const (
	// ProfileFast: cheap null-presence + type over a tiny sample. Daily use.
	ProfileFast ProfileMode = "fast"
	// ProfileStandard: null ratio, distinct, min/max, top values over a
	// bounded sample. Search/filter mapping.
	ProfileStandard ProfileMode = "standard"
	// ProfileDeep: full-table scan (no sample cap) for accurate distinct/
	// min/max; still privacy-restricted. Approved targets only.
	ProfileDeep ProfileMode = "deep"
)

// sample sizes per mode (0 = no cap, full scan).
func (m ProfileMode) sampleLimit() int {
	switch m {
	case ProfileFast:
		return 2000
	case ProfileDeep:
		return 0
	default:
		return 100_000
	}
}

// ProfileRequest scopes a profiling run (FR-META-007 cost control).
type ProfileRequest struct {
	SourceID    string
	Tables      []string    // FQNs to profile; empty → all tables in the latest snapshot
	Mode        ProfileMode // default standard
	SampleLimit int         // override the mode's sample cap; 0 → mode default
	MaxTables   int         // cap tables per run (default 50); protects the DB
	PIIColumns  []string    // extra sensitive columns: "schema.table.col" or "*.col"
	MinTopFreq  int         // hide top values below this frequency (default 2)
}

// ValueFreq is one value and its count (non-sensitive, code-like columns only).
type ValueFreq struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
}

// ColumnProfile is the computed statistics for one column (FR-META-008 shapes
// what is stored for sensitive columns).
type ColumnProfile struct {
	Schema    string  `json:"schema"`
	Table     string  `json:"table"`
	Column    string  `json:"column"`
	DataType  string  `json:"data_type"`
	Sensitive bool    `json:"sensitive,omitempty"` // restricted stats
	Sampled   int64   `json:"sampled_rows"`
	NonNull   int64   `json:"non_null"`
	NullRatio float64 `json:"null_ratio"`
	Distinct  int64   `json:"distinct_count"`

	// value-bearing stats — OMITTED for sensitive columns
	Min       string      `json:"min,omitempty"`
	Max       string      `json:"max,omitempty"`
	TopValues []ValueFreq `json:"top_values,omitempty"`

	// shape-only stats — always safe to store
	LengthMin     int    `json:"length_min,omitempty"`
	LengthMax     int    `json:"length_max,omitempty"`
	FormatPattern string `json:"format_pattern,omitempty"`

	Provenance Provenance `json:"provenance"`
}

// ProfileResult is a stored, reviewable profiling snapshot.
type ProfileResult struct {
	ProfileID     string          `json:"profile_id"`
	SourceID      string          `json:"source_id"`
	Dialect       string          `json:"dialect"`
	Mode          ProfileMode     `json:"mode"`
	ProfiledAt    time.Time       `json:"profiled_at"`
	SampleLimit   int             `json:"sample_limit"`
	ScannedTables int             `json:"scanned_tables"`
	Columns       []ColumnProfile `json:"columns"`
	Warnings      []string        `json:"warnings,omitempty"`
	Note          string          `json:"note"`
}

// piiNameRE flags column names that are very likely personal/credit
// information, so they are treated as sensitive even without explicit config
// (FR-META-008: 개인정보·신용정보 자동 분류). Operator PII config still adds more.
var piiNameRE = regexp.MustCompile(`(?i)(email|e_mail|phone|mobile|tel|ssn|passwd|password|passwrd|pwd|token|secret|hash|salt|birth|dob|resident|jumin|주민|전화|이메일|비밀|성명|이름|passport|account_no|acct_no|card_no|cvv|iban|ip_addr|\bip\b|addr|address|주소)`)

// codeLikeName flags columns whose top values are safe to keep (code/status/
// type/flag/category columns), so distribution can be shown for them.
var codeLikeName = regexp.MustCompile(`(?i)(_cd$|_code$|_type$|_div$|_flag$|_yn$|status|category|kind|gender|sex|grade|level|state)`)

// Profile runs a profiling pass and stores the result (FR-META-006/007/008).
func (s *Service) Profile(ctx context.Context, req ProfileRequest) (*ProfileResult, error) {
	if req.Mode == "" {
		req.Mode = ProfileStandard
	}
	if req.MaxTables <= 0 {
		req.MaxTables = 50
	}
	if req.MinTopFreq <= 0 {
		req.MinTopFreq = 2
	}
	sampleLimit := req.SampleLimit
	if sampleLimit <= 0 {
		sampleLimit = req.Mode.sampleLimit()
	}

	dialect, err := s.col.q.ProfileDialect(ctx, req.SourceID)
	if err != nil {
		return nil, err
	}

	// resolve target tables from the latest snapshot (structure + types + PII
	// hints). If no snapshot exists yet, collect the structure now.
	snap, _ := s.latest(req.SourceID)
	if snap == nil {
		snap, err = s.col.Collect(ctx, CollectRequest{SourceID: req.SourceID})
		if err != nil {
			return nil, err
		}
	}
	tables := selectTables(snap, req.Tables)
	if len(tables) > req.MaxTables {
		tables = tables[:req.MaxTables]
	}

	res := &ProfileResult{
		ProfileID: newProfileID(), SourceID: req.SourceID, Dialect: dialect,
		Mode: req.Mode, ProfiledAt: s.nowFn(), SampleLimit: sampleLimit,
	}
	q := newQuoter(dialect)
	extraPII := parsePIISpecs(req.PIIColumns)

	for _, t := range tables {
		cols, warn := s.profileTable(ctx, req.SourceID, q, t, req.Mode, sampleLimit, req.MinTopFreq, extraPII, res.ProfiledAt)
		if warn != "" {
			res.Warnings = append(res.Warnings, t.FQN()+": "+warn)
			continue
		}
		res.Columns = append(res.Columns, cols...)
		res.ScannedTables++
	}
	res.Note = "프로파일 결과는 검토 대상 후보입니다. 민감 컬럼은 원본 값·최소/최대·상위값을 저장하지 않고 길이·패턴·건수만 보관합니다. 운영 카탈로그(column_stats)에는 승인 후 반영하세요."
	if err := s.storeProfile(res); err != nil {
		return nil, err
	}
	return res, nil
}

func (s *Service) profileTable(ctx context.Context, sourceID string, q quoter, t TableAsset, mode ProfileMode, sampleLimit, minTopFreq int, extraPII map[string]bool, now time.Time) ([]ColumnProfile, string) {
	if len(t.Columns) == 0 {
		return nil, "no columns"
	}
	sampleExpr := q.sample(t.Schema, t.Name, sampleLimit)

	// one aggregate pass over the sample computes row count + per-column
	// non-null and distinct counts (FR-META-007: single scan, bounded).
	sel := []string{"COUNT(*) AS __rows"}
	for i, c := range t.Columns {
		qc := q.ident(c.Name)
		sel = append(sel, fmt.Sprintf("COUNT(%s) AS c%d_nn", qc, i))
		if mode != ProfileFast {
			sel = append(sel, fmt.Sprintf("COUNT(DISTINCT %s) AS c%d_d", qc, i))
			if isOrderable(c.DataType) && !isSensitive(t, c, extraPII) {
				sel = append(sel, fmt.Sprintf("MIN(CAST(%s AS CHAR(64))) AS c%d_min", qc, i))
				sel = append(sel, fmt.Sprintf("MAX(CAST(%s AS CHAR(64))) AS c%d_max", qc, i))
			}
		}
	}
	aggQ := "SELECT " + strings.Join(sel, ", ") + " FROM " + sampleExpr
	rows, err := s.col.q.SystemQuery(ctx, sourceID, aggQ)
	if err != nil || len(rows) == 0 {
		if err != nil {
			return nil, sanitize(err.Error())
		}
		return nil, "no rows"
	}
	agg := rows[0]
	sampled := asInt64(agg["__rows"])

	var out []ColumnProfile
	for i, c := range t.Columns {
		sensitive := isSensitive(t, c, extraPII)
		nn := asInt64(agg[fmt.Sprintf("c%d_nn", i)])
		cp := ColumnProfile{
			Schema: t.Schema, Table: t.Name, Column: c.Name, DataType: c.DataType,
			Sensitive: sensitive, Sampled: sampled, NonNull: nn,
			Provenance: Provenance{
				Confidence: profileConfidence(mode, sampled), Generator: GenStatistics,
				GeneratedAt: now, ReviewStatus: StatusDiscovered,
				Evidence: []string{string(mode) + " profiling over " + itoa(int(sampled)) + " sampled rows"},
			},
		}
		if sampled > 0 {
			cp.NullRatio = round4(float64(sampled-nn) / float64(sampled))
		}
		if mode != ProfileFast {
			cp.Distinct = asInt64(agg[fmt.Sprintf("c%d_d", i)])
			if !sensitive && isOrderable(c.DataType) {
				cp.Min = asString(agg[fmt.Sprintf("c%d_min", i)])
				cp.Max = asString(agg[fmt.Sprintf("c%d_max", i)])
			}
		}
		out = append(out, cp)
	}

	// second pass, standard/deep only: top values for low-cardinality,
	// non-sensitive, code-like columns; plus string length/pattern shape for
	// every column (safe even when sensitive).
	if mode != ProfileFast {
		for i := range out {
			c := t.Columns[i]
			cp := &out[i]
			// top values for non-sensitive columns only. Very low cardinality
			// (<=20 distinct) is itself a strong "categorical/code column"
			// signal, so show distributions regardless of name; a code-like
			// name widens the threshold to 50.
			lowCard := cp.Distinct > 0 && cp.Distinct <= 20
			codeName := cp.Distinct > 0 && cp.Distinct <= 50 && codeLikeName.MatchString(c.Name)
			if !cp.Sensitive && (lowCard || codeName) {
				cp.TopValues = s.topValues(ctx, sourceID, q, t, c.Name, sampleExpr, minTopFreq)
			}
			// string shape (length range + format pattern) — safe for all,
			// including sensitive columns (no raw values retained)
			if isStringType(c.DataType) {
				s.stringShape(ctx, sourceID, q, t, c.Name, sampleExpr, cp)
			}
		}
	}
	return out, ""
}

// topValues returns the highest-frequency values (>= minFreq) for a code-like
// column, hiding low-frequency values (FR-META-008: 소규모 값 숨김).
func (s *Service) topValues(ctx context.Context, sourceID string, q quoter, t TableAsset, col, sampleExpr string, minFreq int) []ValueFreq {
	qc := q.ident(col)
	query := fmt.Sprintf("SELECT %s AS v, COUNT(*) AS n FROM %s WHERE %s IS NOT NULL GROUP BY %s ORDER BY n DESC LIMIT 20",
		qc, sampleExpr, qc, qc)
	rows, err := s.col.q.SystemQuery(ctx, sourceID, query)
	if err != nil {
		return nil
	}
	var out []ValueFreq
	for _, r := range rows {
		n := asInt64(r["n"])
		if int(n) < minFreq {
			continue
		}
		v := asString(r["v"])
		if len(v) > 64 {
			v = v[:64]
		}
		out = append(out, ValueFreq{Value: v, Count: n})
		if len(out) >= 10 {
			break
		}
	}
	return out
}

// stringShape computes length min/max and a coarse format pattern from sample
// values WITHOUT storing raw values (FR-META-008: 원본 대신 길이·패턴만).
func (s *Service) stringShape(ctx context.Context, sourceID string, q quoter, t TableAsset, col, sampleExpr string, cp *ColumnProfile) {
	qc := q.ident(col)
	query := fmt.Sprintf("SELECT %s AS v FROM %s WHERE %s IS NOT NULL LIMIT 500", qc, sampleExpr, qc)
	rows, err := s.col.q.SystemQuery(ctx, sourceID, query)
	if err != nil {
		return
	}
	lmin, lmax := -1, 0
	patterns := map[string]int{}
	for _, r := range rows {
		v := asString(r["v"])
		l := len([]rune(v))
		if lmin < 0 || l < lmin {
			lmin = l
		}
		if l > lmax {
			lmax = l
		}
		patterns[charClassPattern(v)]++
	}
	if lmin >= 0 {
		cp.LengthMin, cp.LengthMax = lmin, lmax
	}
	cp.FormatPattern = dominantPattern(patterns)
}

// charClassPattern maps a value to a coarse class signature (digits→9,
// letters→A, other→., capped) so patterns are storable without raw values.
func charClassPattern(v string) string {
	var b strings.Builder
	prev := byte(0)
	run := 0
	flush := func() {
		if prev != 0 {
			b.WriteByte(prev)
			if run > 1 {
				b.WriteString("{" + itoa(run) + "}")
			}
		}
	}
	for _, r := range v {
		var cls byte
		switch {
		case r >= '0' && r <= '9':
			cls = '9'
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			cls = 'A'
		default:
			cls = '.'
		}
		if cls == prev {
			run++
		} else {
			flush()
			prev, run = cls, 1
		}
		if b.Len() > 40 {
			break
		}
	}
	flush()
	return b.String()
}

func dominantPattern(patterns map[string]int) string {
	best, bestN := "", 0
	for p, n := range patterns {
		if n > bestN || (n == bestN && p < best) {
			best, bestN = p, n
		}
	}
	return best
}

// ---- sensitivity ----

func isSensitive(t TableAsset, c ColumnAsset, extraPII map[string]bool) bool {
	if extraPII["*."+strings.ToLower(c.Name)] {
		return true
	}
	if extraPII[strings.ToLower(t.FQN()+"."+c.Name)] {
		return true
	}
	return piiNameRE.MatchString(c.Name)
}

func parsePIISpecs(specs []string) map[string]bool {
	out := map[string]bool{}
	for _, s := range specs {
		out[strings.ToLower(strings.TrimSpace(s))] = true
	}
	return out
}

// ---- type helpers ----

func isOrderable(dataType string) bool {
	dt := strings.ToLower(dataType)
	for _, p := range []string{"int", "numeric", "decimal", "real", "double", "float", "date", "time", "timestamp", "year", "money", "serial", "bigint", "smallint"} {
		if strings.Contains(dt, p) {
			return true
		}
	}
	// bounded char types are orderable and cheap
	for _, p := range []string{"char", "varchar", "character varying", "text"} {
		if strings.Contains(dt, p) {
			return true
		}
	}
	return false
}

func isStringType(dataType string) bool {
	dt := strings.ToLower(dataType)
	return strings.Contains(dt, "char") || strings.Contains(dt, "text") || strings.Contains(dt, "character")
}

func profileConfidence(mode ProfileMode, sampled int64) float64 {
	switch mode {
	case ProfileDeep:
		return 1.0
	case ProfileFast:
		return 0.5
	default:
		if sampled >= 100_000 {
			return 0.8
		}
		return 0.9 // small tables fully sampled under the cap
	}
}

// ---- store ----

func (s *Service) storeProfile(res *ProfileResult) error {
	dir := s.profileDir(res.SourceID)
	if err := mkdirAll(dir); err != nil {
		return err
	}
	return writeJSONFile(dir, sanitizeID(res.ProfileID)+".json", res)
}

func (s *Service) profileDir(sourceID string) string {
	return joinPath(s.dir, "..", "profiles", sanitizeID(sourceID))
}

// Profiles lists stored profile results for a source, newest first (summaries).
func (s *Service) Profiles(sourceID string) ([]ProfileResult, error) {
	var out []ProfileResult
	names, err := listJSON(s.profileDir(sourceID))
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		var r ProfileResult
		if err := readJSONFile(s.profileDir(sourceID), n+".json", &r); err != nil {
			continue
		}
		r.Columns = nil // summary view
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProfiledAt.After(out[j].ProfiledAt) })
	return out, nil
}

func newProfileID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "prof-" + time.Now().UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b[:])
}

func selectTables(snap *RawSnapshot, want []string) []TableAsset {
	if len(want) == 0 {
		// tables only (skip views for profiling by default)
		var out []TableAsset
		for _, t := range snap.Tables {
			if t.Kind == "table" {
				out = append(out, t)
			}
		}
		return out
	}
	wantSet := map[string]bool{}
	for _, w := range want {
		wantSet[strings.ToLower(strings.TrimSpace(w))] = true
	}
	var out []TableAsset
	for _, t := range snap.Tables {
		if wantSet[strings.ToLower(t.FQN())] {
			out = append(out, t)
		}
	}
	return out
}

func round4(f float64) float64 {
	return float64(int64(f*10000+0.5)) / 10000
}

func sanitize(msg string) string {
	msg = strings.ReplaceAll(msg, "\n", " ")
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}
