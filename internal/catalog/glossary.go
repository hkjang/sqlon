package catalog

import (
	"os"
	"path/filepath"
	"strings"
)

// Glossary maps business terms to synonyms and physical naming hints so the
// same dictionary drives search, question analysis, SQL generation context,
// and validation hints.
type Glossary struct {
	Entries []GlossaryEntry `json:"entries"`
}

type GlossaryEntry struct {
	Term     string   `json:"term"`
	Synonyms []string `json:"synonyms,omitempty"`
	Category string   `json:"category,omitempty"` // entity | metric | dimension | value | time
	Note     string   `json:"note,omitempty"`
}

// defaultGlossary is used when data/<set>/glossary.json is absent. Operators
// should manage the JSON file; this fallback keeps the server functional.
var defaultGlossary = &Glossary{Entries: []GlossaryEntry{
	{Term: "고객", Synonyms: []string{"회원", "차주", "customer", "cust", "cust_no", "고객번호"}, Category: "entity"},
	{Term: "기준월", Synonyms: []string{"조회월", "base_month", "bs_yr_mon", "yr_mon", "기준연월"}, Category: "time"},
	{Term: "기준일", Synonyms: []string{"조회일", "기준일자", "bs_dt", "stday", "dt", "date"}, Category: "time"},
	{Term: "신용점수", Synonyms: []string{"평점", "점수", "스코어", "score", "scr", "lst_score", "k-score"}, Category: "metric"},
	{Term: "잔액", Synonyms: []string{"balance", "bal", "ln_bal", "대출잔액"}, Category: "metric"},
	{Term: "연체", Synonyms: []string{"delinquency", "dlq", "dlq_yn", "dlq_amt", "연체금액"}, Category: "entity"},
	{Term: "계약", Synonyms: []string{"account", "loan", "acct", "계좌", "대출계좌", "mgt_acct_no", "관리계좌번호"}, Category: "entity"},
	{Term: "개인사업자", Synonyms: []string{"soho", "sole proprietor", "자영업자", "사업자"}, Category: "dimension"},
	{Term: "지역", Synonyms: []string{"시도", "province", "region", "지역코드"}, Category: "dimension"},
	{Term: "상품", Synonyms: []string{"product", "prod", "prod_cd", "상품코드"}, Category: "dimension"},
	{Term: "대출", Synonyms: []string{"loan", "ln", "여신", "대출계좌"}, Category: "entity"},
	{Term: "카드", Synonyms: []string{"card", "신용카드", "카드이용", "use_amt", "이용금액"}, Category: "entity"},
	{Term: "금액", Synonyms: []string{"amt", "amount"}, Category: "metric"},
	{Term: "등급", Synonyms: []string{"grade", "grad", "scr_grad"}, Category: "dimension"},
	{Term: "기관", Synonyms: []string{"회원사", "은행", "agnc", "tx_agnc", "tx_agnc_cd", "거래기관"}, Category: "dimension"},
	{Term: "자산", Synonyms: []string{"asset", "asst", "주택", "자가", "house"}, Category: "entity"},
	{Term: "건수", Synonyms: []string{"count", "cnt", "개수"}, Category: "metric"},
	{Term: "보증", Synonyms: []string{"guarantee", "grnt", "보증계좌"}, Category: "entity"},
}}

func loadGlossary(dataDir string) (*Glossary, []LoadIssue) {
	var issues []LoadIssue
	path := filepath.Join(dataDir, "glossary.json")
	if _, err := os.Stat(path); err != nil {
		return defaultGlossary, []LoadIssue{{
			Level: "warning", Source: "glossary.json",
			Message: "glossary.json not found; using built-in default glossary",
		}}
	}
	g := &Glossary{}
	if err := readJSON(path, g); err != nil {
		issues = append(issues, LoadIssue{Level: "error", Source: "glossary.json", Message: err.Error()})
		return defaultGlossary, issues
	}
	seen := map[string]bool{}
	for _, e := range g.Entries {
		key := strings.ToLower(strings.TrimSpace(e.Term))
		if key == "" {
			issues = append(issues, LoadIssue{Level: "error", Source: "glossary.json", Message: "entry with empty term"})
			continue
		}
		if seen[key] {
			issues = append(issues, LoadIssue{Level: "warning", Source: "glossary.json", Message: "duplicate glossary term: " + e.Term})
		}
		seen[key] = true
	}
	return g, issues
}

// Expand returns the input tokens plus every synonym-group member whose term
// or synonym matches one of the tokens. Matching is bidirectional substring
// so Korean agglutinated tokens still hit ("고객수" -> "고객").
func (g *Glossary) Expand(tokens []string) ([]string, map[string][]string) {
	out := append([]string{}, tokens...)
	matched := map[string][]string{}
	if g == nil {
		return unique(out), matched
	}
	for _, tok := range tokens {
		lt := strings.ToLower(tok)
		if lt == "" {
			continue
		}
		for _, e := range g.Entries {
			group := append([]string{e.Term}, e.Synonyms...)
			hit := false
			for _, term := range group {
				t := strings.ToLower(term)
				if t == "" {
					continue
				}
				if strings.Contains(lt, t) || strings.Contains(t, lt) {
					hit = true
					break
				}
			}
			if hit {
				out = append(out, group...)
				matched[e.Term] = appendUnique(matched[e.Term], tok)
			}
		}
	}
	return unique(out), matched
}

// TermsFor returns the synonym group for a term, or nil.
func (g *Glossary) TermsFor(term string) []string {
	if g == nil {
		return nil
	}
	lt := strings.ToLower(strings.TrimSpace(term))
	for _, e := range g.Entries {
		group := append([]string{e.Term}, e.Synonyms...)
		for _, t := range group {
			if strings.ToLower(t) == lt {
				return group
			}
		}
	}
	return nil
}

func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}
