package dbconn

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// SQL execution guard (GO-SQL-001..010). This is the last line of defense in
// front of the database; the richer metadata validation (catalog tables,
// columns, PII, joins) happens earlier in validate_sql. Checks run against a
// comment-stripped, string-literal-masked form so `/* UPDATE */` in a comment
// or 'DELETE' in a literal neither blocks nor bypasses anything.

// builtin denied keywords common to all dialects: DML, DDL, stored routines,
// privileges, transactions, session/state changes (GO-SQL-002..007).
// Dialect-specific additions (pg_sleep, LOAD_FILE, OUTFILE, ...) come from
// Dialect.DeniedExtras and are merged per profile.
var deniedKeywords = []string{
	"INSERT", "UPDATE", "DELETE", "MERGE",
	"CREATE", "ALTER", "DROP", "TRUNCATE", "RENAME",
	"BEGIN", "DECLARE", "EXEC", "EXECUTE", "CALL", "PREPARE",
	"GRANT", "REVOKE",
	"COMMIT", "ROLLBACK", "SAVEPOINT",
	"LOCK", "SET", "USE",
}

var deniedPrefixes = []string{}

var selectWithRE = regexp.MustCompile(`(?is)^\s*(SELECT|WITH)\b`)

// ValidateReadOnlySQL enforces the read-only policy. extraDenied comes from
// the profile policy and extends the built-in list.
func ValidateReadOnlySQL(sqlText string, extraDenied []string) error {
	if strings.TrimSpace(sqlText) == "" {
		return errors.New("sql is empty")
	}
	masked := stripSQL(sqlText)
	if !selectWithRE.MatchString(masked) {
		return errors.New("only SELECT or WITH statements are allowed")
	}
	// multiple statements: any semicolon outside comments/strings that is
	// followed by more content (a single trailing ; is tolerated and trimmed
	// at execution time)
	if i := strings.Index(masked, ";"); i >= 0 {
		if strings.TrimSpace(masked[i+1:]) != "" {
			return errors.New("multiple statements are not allowed")
		}
	}
	upper := strings.ToUpper(masked)
	for _, kw := range deniedKeywordsMerged(extraDenied) {
		if strings.HasSuffix(kw, "_") || strings.HasSuffix(kw, ".") || strings.HasSuffix(kw, "$") {
			if strings.Contains(upper, kw) {
				return fmt.Errorf("denied SQL keyword detected: %s", kw)
			}
			continue
		}
		if regexp.MustCompile(`\b` + regexp.QuoteMeta(kw) + `\b`).MatchString(upper) {
			return fmt.Errorf("denied SQL keyword detected: %s", kw)
		}
	}
	return nil
}

func deniedKeywordsMerged(extra []string) []string {
	out := append([]string{}, deniedKeywords...)
	out = append(out, deniedPrefixes...)
	for _, kw := range extra {
		kw = strings.ToUpper(strings.TrimSpace(kw))
		if kw != "" {
			out = append(out, kw)
		}
	}
	return out
}

// stripSQL blanks comments and single-quoted string literals while keeping
// everything else verbatim, so keyword checks can't be fooled or false-
// triggered by comment/literal content.
func stripSQL(sql string) string {
	var b strings.Builder
	b.Grow(len(sql))
	inSingle, inLine, inBlock := false, false, false
	for i := 0; i < len(sql); i++ {
		ch := sql[i]
		var next byte
		if i+1 < len(sql) {
			next = sql[i+1]
		}
		switch {
		case inLine:
			if ch == '\n' {
				inLine = false
				b.WriteByte(ch)
			} else {
				b.WriteByte(' ')
			}
		case inBlock:
			if ch == '*' && next == '/' {
				inBlock = false
				b.WriteString("  ")
				i++
			} else {
				b.WriteByte(' ')
			}
		case inSingle:
			if ch == '\'' {
				if next == '\'' { // escaped quote
					b.WriteString("  ")
					i++
				} else {
					inSingle = false
					b.WriteByte(' ')
				}
			} else {
				b.WriteByte(' ')
			}
		case ch == '-' && next == '-':
			inLine = true
			b.WriteString("  ")
			i++
		case ch == '/' && next == '*':
			inBlock = true
			b.WriteString("  ")
			i++
		case ch == '\'':
			inSingle = true
			b.WriteByte(' ')
		default:
			b.WriteByte(ch)
		}
	}
	return b.String()
}
