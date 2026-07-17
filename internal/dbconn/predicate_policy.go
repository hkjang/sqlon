package dbconn

import (
	"fmt"
	"sort"
	"strings"
)

// PredicatePolicy describes one table-scoped row predicate. Condition is
// compiled with the target database grammar; AllowedColumns must contain the
// columns that the condition is allowed to reference.
type PredicatePolicy struct {
	PolicyID       string
	Schema         string
	Table          string
	Condition      string
	AllowedColumns []string
}

// PolicyCondition is a parsed, dialect-specific boolean condition. Its AST is
// deliberately kept private so callers cannot mutate a condition after it has
// been validated.
type PolicyCondition struct {
	dialect        string
	raw            string
	allowedColumns map[string]struct{}
	columns        []string
	atoms          []string
	pg             any
	mysql          any
}

// Columns returns the table columns referenced by the compiled condition.
func (c *PolicyCondition) Columns() []string {
	if c == nil {
		return nil
	}
	return append([]string(nil), c.columns...)
}

// CompiledPredicatePolicy is safe to reuse across query validations.
type CompiledPredicatePolicy struct {
	PolicyID  string
	Schema    string
	Table     string
	Condition *PolicyCondition
}

// PredicateMatch reports the policy decision for one direct physical table
// occurrence in one SELECT query block. QueryBlock is one-based and stable for
// a single parse. Alias distinguishes self joins and alias shadowing.
type PredicateMatch struct {
	PolicyID   string `json:"policy_id,omitempty"`
	QueryBlock int    `json:"query_block"`
	Schema     string `json:"schema,omitempty"`
	Table      string `json:"table"`
	Alias      string `json:"alias,omitempty"`
	Condition  string `json:"condition"`
	Matched    bool   `json:"matched"`
	Reason     string `json:"reason"`
}

// PredicateReport contains one decision per policy/table occurrence. Missing
// is repeated for convenience so hard-enforcement callers can fail closed
// without filtering Matches themselves.
type PredicateReport struct {
	Matches []PredicateMatch `json:"matches"`
	Missing []PredicateMatch `json:"missing,omitempty"`
}

// ParsePredicateCondition parses and validates a table-scoped condition using
// the real target-dialect parser. Conditions must reference at least one of
// allowedColumns and may not contain a subquery. A top-level conjunction is
// compiled as an order-independent set of required atoms.
func ParsePredicateCondition(dialect, condition string, allowedColumns []string) (*PolicyCondition, error) {
	dialect = strings.ToLower(strings.TrimSpace(dialect))
	condition = strings.TrimSpace(condition)
	if condition == "" {
		return nil, fmt.Errorf("predicate condition is empty")
	}
	allowed := make(map[string]struct{}, len(allowedColumns))
	for _, col := range allowedColumns {
		if col = strings.TrimSpace(col); col != "" {
			allowed[strings.ToLower(col)] = struct{}{}
		}
	}
	if len(allowed) == 0 {
		return nil, fmt.Errorf("predicate condition requires at least one allowed column")
	}

	var (
		c   *PolicyCondition
		err error
	)
	switch dialect {
	case "postgres":
		c, err = parsePostgresPolicyCondition(condition, allowed)
	case "mysql":
		c, err = parseMySQLPolicyCondition(condition, allowed, false)
	case "mariadb":
		c, err = parseMySQLPolicyCondition(condition, allowed, true)
	default:
		return nil, fmt.Errorf("unsupported dialect for predicate policy: %s", dialect)
	}
	if err != nil {
		return nil, err
	}
	c.dialect = dialect
	c.raw = condition
	c.allowedColumns = allowed
	sort.Strings(c.columns)
	sort.Strings(c.atoms)
	return c, nil
}

// ValidatePredicateCondition is the validation-only form of
// ParsePredicateCondition.
func ValidatePredicateCondition(dialect, condition string, allowedColumns []string) error {
	_, err := ParsePredicateCondition(dialect, condition, allowedColumns)
	return err
}

// RenderConditionForAlias parses a condition and safely qualifies every
// validated column through the dialect AST. The alias is treated as an
// identifier, never interpolated into SQL text.
func RenderConditionForAlias(dialect, condition, alias string, allowedColumns []string) (string, error) {
	c, err := ParsePredicateCondition(dialect, condition, allowedColumns)
	if err != nil {
		return "", err
	}
	return c.RenderForAlias(alias)
}

// RenderForAlias returns the normalized condition with every table column
// bound to alias.
func (c *PolicyCondition) RenderForAlias(alias string) (string, error) {
	if c == nil {
		return "", fmt.Errorf("predicate condition is nil")
	}
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return "", fmt.Errorf("predicate alias is empty")
	}
	switch c.dialect {
	case "postgres":
		return renderPostgresPolicyCondition(c, alias)
	case "mysql", "mariadb":
		return renderMySQLPolicyCondition(c, alias, c.dialect == "mariadb")
	default:
		return "", fmt.Errorf("unsupported dialect for predicate rendering: %s", c.dialect)
	}
}

// CompilePredicatePolicy validates a policy once for reuse. Table may be a
// bare name or schema-qualified; an explicit Schema takes precedence.
func CompilePredicatePolicy(dialect string, p PredicatePolicy) (*CompiledPredicatePolicy, error) {
	schema, table := normalizePolicyTable(p.Schema, p.Table)
	if table == "" {
		return nil, fmt.Errorf("predicate policy %q has an empty table", p.PolicyID)
	}
	c, err := ParsePredicateCondition(dialect, p.Condition, p.AllowedColumns)
	if err != nil {
		return nil, fmt.Errorf("predicate policy %q: %w", p.PolicyID, err)
	}
	return &CompiledPredicatePolicy{
		PolicyID:  p.PolicyID,
		Schema:    schema,
		Table:     table,
		Condition: c,
	}, nil
}

// ValidateRequiredPredicates compiles policies and matches them against sql.
// Parse/configuration failures are returned as errors; a missing predicate is
// represented by a report entry with Matched=false so callers can choose warn
// or hard enforcement per policy.
func ValidateRequiredPredicates(dialect, sqlText string, policies []PredicatePolicy) (PredicateReport, error) {
	compiled := make([]*CompiledPredicatePolicy, 0, len(policies))
	for _, p := range policies {
		cp, err := CompilePredicatePolicy(dialect, p)
		if err != nil {
			return PredicateReport{}, err
		}
		compiled = append(compiled, cp)
	}
	return MatchRequiredPredicates(dialect, sqlText, compiled)
}

// MatchRequiredPredicates checks each direct table occurrence in every query
// block. Only positive, top-level WHERE conjuncts count. ON, HAVING, OR, NOT,
// boolean wrappers, and predicates in another UNION/subquery block never
// satisfy a policy.
func MatchRequiredPredicates(dialect, sqlText string, policies []*CompiledPredicatePolicy) (PredicateReport, error) {
	dialect = strings.ToLower(strings.TrimSpace(dialect))
	for _, p := range policies {
		if p == nil || p.Condition == nil {
			return PredicateReport{}, fmt.Errorf("compiled predicate policy is nil")
		}
		if p.Condition.dialect != dialect {
			return PredicateReport{}, fmt.Errorf("predicate policy %q was compiled for %s, not %s", p.PolicyID, p.Condition.dialect, dialect)
		}
	}

	var (
		report PredicateReport
		err    error
	)
	switch dialect {
	case "postgres":
		report, err = matchPostgresRequiredPredicates(sqlText, policies)
	case "mysql":
		report, err = matchMySQLRequiredPredicates(sqlText, policies, false)
	case "mariadb":
		report, err = matchMySQLRequiredPredicates(sqlText, policies, true)
	default:
		return PredicateReport{}, fmt.Errorf("unsupported dialect for predicate policy: %s", dialect)
	}
	if err != nil {
		return PredicateReport{}, err
	}
	for _, m := range report.Matches {
		if !m.Matched {
			report.Missing = append(report.Missing, m)
		}
	}
	return report, nil
}

type policyTableOccurrence struct {
	queryBlock  int
	schema      string
	table       string
	alias       string
	sourceCount int
	where       any
}

func normalizePolicyTable(schema, table string) (string, string) {
	schema = strings.TrimSpace(schema)
	table = strings.TrimSpace(table)
	if schema == "" {
		parts := strings.Split(table, ".")
		if len(parts) > 1 {
			schema = parts[len(parts)-2]
			table = parts[len(parts)-1]
		}
	}
	return strings.ToLower(strings.Trim(schema, "\"`")), strings.ToLower(strings.Trim(table, "\"`"))
}

func policyApplies(p *CompiledPredicatePolicy, occ policyTableOccurrence) bool {
	if !strings.EqualFold(p.Table, occ.table) {
		return false
	}
	return p.Schema == "" || occ.schema == "" || strings.EqualFold(p.Schema, occ.schema)
}

func containsAllAtoms(have []string, want []string) bool {
	counts := make(map[string]int, len(have))
	for _, atom := range have {
		counts[atom]++
	}
	for _, atom := range want {
		if counts[atom] == 0 {
			return false
		}
		counts[atom]--
	}
	return true
}

func matchReason(hasWhere, matched bool) string {
	if matched {
		return "required condition is present as positive top-level WHERE conjuncts"
	}
	if !hasWhere {
		return "query block has no WHERE clause"
	}
	return "required condition is not implied by positive top-level WHERE conjuncts"
}
