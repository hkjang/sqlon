package mcp

import (
	"sort"
	"strings"
	"time"

	"sqlon/internal/catalog"
)

// PII exposure report. A column flagged pii=true in the catalog is masked
// automatically in every query result (PIIColumnNames → maskPIIResult). The
// real risk is a column that LOOKS like personal data but is not tagged, so it
// is returned in the clear. This report cross-references the catalog against a
// Korean+English sensitive-data heuristic to surface:
//   - protected: tagged PII columns (auto-masked)
//   - exposed:   heuristic PII columns that are NOT tagged (not masked)
// It reads only catalog metadata — it never queries a database or reads values.

// piiPattern is one sensitive-data category with the substrings that identify
// it in a column name, logical name, or synonym (all matched lower-cased).
type piiPattern struct {
	category string
	needles  []string
}

// Ordered most-specific first so the reported reason is the strongest match.
var piiPatterns = []piiPattern{
	{"주민등록번호(RRN)", []string{"jumin", "rrn", "resident_reg", "ssn", "social_security", "주민", "주민등록"}},
	{"여권번호", []string{"passport", "여권"}},
	{"운전면허", []string{"driver_license", "license_no", "면허"}},
	{"신용카드", []string{"card_no", "cardno", "creditcard", "credit_card", "pan", "카드번호", "카드"}},
	{"계좌번호", []string{"account_no", "acct_no", "accountno", "iban", "계좌", "계좌번호"}},
	{"이메일", []string{"email", "e_mail", "이메일"}},
	{"전화번호", []string{"phone", "mobile", "_tel", "tel_", "phone_no", "cellphone", "전화", "휴대", "핸드폰", "연락처"}},
	{"주소", []string{"address", "addr", "postal", "zipcode", "zip_code", "주소", "우편번호"}},
	{"생년월일", []string{"birth", "dob", "birthday", "생년월일", "생일"}},
	{"성명/이름", []string{"first_name", "last_name", "full_name", "cust_name", "user_name", "성명", "고객명", "이름", "홍길동"}},
}

// piiHeuristic reports whether a column looks like personal data and why. It
// checks the physical name, logical name, semantic type, and synonyms.
func piiHeuristic(col *catalog.Column) (bool, string) {
	if col == nil {
		return false, ""
	}
	hay := strings.ToLower(strings.Join([]string{
		col.Name, col.LogicalName, col.SemanticType, strings.Join(col.Synonyms, " "),
	}, " "))
	if strings.Contains(strings.ToLower(col.SemanticType), "pii") {
		return true, "semantic_type=PII"
	}
	for _, p := range piiPatterns {
		for _, n := range p.needles {
			if strings.Contains(hay, n) {
				return true, p.category
			}
		}
	}
	return false, ""
}

type piiColumn struct {
	Table       string `json:"table"`
	Column      string `json:"column"`
	LogicalName string `json:"logical_name,omitempty"`
	Category    string `json:"category,omitempty"`
	Masked      bool   `json:"masked"`
}

// piiExposureReport scans a catalog and classifies its PII surface.
func piiExposureReport(cat *catalog.Catalog, source string) map[string]any {
	protected := []piiColumn{}
	exposed := []piiColumn{}
	scanned := 0
	tablesWithPII := map[string]bool{}

	for _, t := range cat.Tables {
		for _, col := range t.Columns {
			scanned++
			heuristic, reason := piiHeuristic(col)
			switch {
			case col.PII:
				// tagged → auto-masked in query results
				category := reason
				if category == "" {
					category = "tagged pii=true"
				}
				protected = append(protected, piiColumn{Table: t.FQN, Column: col.Name, LogicalName: col.LogicalName, Category: category, Masked: true})
				tablesWithPII[t.FQN] = true
			case heuristic:
				// looks like PII but NOT tagged → returned in the clear
				exposed = append(exposed, piiColumn{Table: t.FQN, Column: col.Name, LogicalName: col.LogicalName, Category: reason, Masked: false})
				tablesWithPII[t.FQN] = true
			}
		}
	}
	sortPII(protected)
	sortPII(exposed)

	status := "ok"
	if len(exposed) > 0 {
		status = "exposed_risk"
	}
	return map[string]any{
		"status":         status,
		"catalog_source": source,
		"summary": map[string]any{
			"columns_scanned":     scanned,
			"tagged_pii":          len(protected),
			"exposed_candidates":  len(exposed),
			"tables_with_pii":     len(tablesWithPII),
		},
		"protected":    protected,
		"exposed":      exposed,
		"collected_at": time.Now().UTC(),
		"notice":       "태그된 PII(pii=true) 컬럼은 쿼리 결과에서 자동 마스킹됩니다. exposed 후보는 개인정보로 보이지만 태그되지 않아 평문으로 반환됩니다 — 메타데이터에 pii=true를 지정하면 자동 마스킹됩니다. 테이블 접근 권한은 DB 플릿 설정의 프로파일 grant로 통제하세요. 휴리스틱 기반이므로 오탐/미탐이 있을 수 있습니다.",
	}
}

func sortPII(cols []piiColumn) {
	sort.Slice(cols, func(i, j int) bool {
		if cols[i].Table != cols[j].Table {
			return cols[i].Table < cols[j].Table
		}
		return cols[i].Column < cols[j].Column
	})
}
