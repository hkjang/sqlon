package catalog

import "strings"

// defaultFilterPresent performs a structural token comparison inside a SQL
// predicate clause. The previous implementation only looked for the first
// word of the configured condition anywhere in the SQL text. Consequently a
// column mentioned in SELECT (or a predicate with a different literal) could
// satisfy a mandatory filter. This scanner keeps literal/operator tokens,
// ignores comments, tolerates aliases and formatting, and only accepts a
// match whose active clause is WHERE, ON, or HAVING.
//
// This is intentionally a policy matcher, not the executor's read-only SQL
// parser. Execution still goes through dbconn's dialect AST guard.
func defaultFilterPresent(sqlText, condition string, ref tableRefInternal) bool {
	query := canonicalPredicateTokens(sqlText)
	want := canonicalPredicateTokens(condition)
	if len(query) == 0 || len(want) == 0 || len(want) > len(query) {
		return false
	}
	for start := 0; start+len(want) <= len(query); start++ {
		if !tokensEqual(query[start:start+len(want)], want) {
			continue
		}
		if !inPredicateClause(query, start) {
			continue
		}
		if filterColumnsBelongToRef(query[start:start+len(want)], want, ref) {
			return true
		}
	}
	return false
}

type predicateToken struct {
	text      string
	kind      byte   // i=identifier, s=string, n=number, o=operator/punctuation
	qualifier string // qualifier removed from alias.column during canonicalization
}

func tokensEqual(got, want []predicateToken) bool {
	for i := range want {
		if got[i].text != want[i].text || got[i].kind != want[i].kind {
			return false
		}
	}
	return true
}

func filterColumnsBelongToRef(got, want []predicateToken, ref tableRefInternal) bool {
	allowed := map[string]bool{
		strings.ToUpper(ref.Alias):      true,
		strings.ToUpper(ref.Table.Name): true,
		strings.ToUpper(ref.Table.FQN):  true,
	}
	for i, tok := range want {
		if tok.kind != 'i' || ref.Table.ColumnMap[tok.text] == nil {
			continue
		}
		q := strings.ToUpper(got[i].qualifier)
		if q != "" && !allowed[q] {
			return false
		}
	}
	return true
}

func inPredicateClause(tokens []predicateToken, before int) bool {
	clause := ""
	for i := 0; i < before; i++ {
		if tokens[i].kind != 'i' {
			continue
		}
		switch tokens[i].text {
		case "WHERE", "ON", "HAVING":
			clause = tokens[i].text
		case "SELECT", "FROM", "GROUP", "ORDER", "LIMIT", "OFFSET", "FETCH", "UNION", "JOIN", "RETURNING":
			clause = tokens[i].text
		}
	}
	return clause == "WHERE" || clause == "ON" || clause == "HAVING"
}

func canonicalPredicateTokens(sqlText string) []predicateToken {
	raw := scanPredicateTokens(sqlText)
	out := make([]predicateToken, 0, len(raw))
	for i := 0; i < len(raw); {
		// Collapse schema.table.column (or alias.column) to the final name,
		// retaining the removed qualifier for table-binding checks.
		if raw[i].kind == 'i' && i+2 < len(raw) && raw[i+1].text == "." && raw[i+2].kind == 'i' {
			parts := []string{raw[i].text, raw[i+2].text}
			j := i + 3
			for j+1 < len(raw) && raw[j].text == "." && raw[j+1].kind == 'i' {
				parts = append(parts, raw[j+1].text)
				j += 2
			}
			out = append(out, predicateToken{
				text:      parts[len(parts)-1],
				kind:      'i',
				qualifier: strings.Join(parts[:len(parts)-1], "."),
			})
			i = j
			continue
		}
		tok := raw[i]
		if tok.text == "!=" {
			tok.text = "<>"
		}
		// A trailing semicolon is not part of a predicate definition.
		if tok.text != ";" {
			out = append(out, tok)
		}
		i++
	}
	return out
}

func scanPredicateTokens(s string) []predicateToken {
	var out []predicateToken
	for i := 0; i < len(s); {
		ch := s[i]
		switch {
		case isSQLSpace(ch):
			i++
		case ch == '-' && i+1 < len(s) && s[i+1] == '-':
			i += 2
			for i < len(s) && s[i] != '\n' && s[i] != '\r' {
				i++
			}
		case ch == '#': // MySQL line comment
			i++
			for i < len(s) && s[i] != '\n' && s[i] != '\r' {
				i++
			}
		case ch == '/' && i+1 < len(s) && s[i+1] == '*':
			i += 2
			for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
				i++
			}
			if i+1 < len(s) {
				i += 2
			}
		case ch == '\'':
			start := i
			i++
			for i < len(s) {
				if s[i] == '\\' && i+1 < len(s) {
					i += 2
					continue
				}
				if s[i] == '\'' {
					if i+1 < len(s) && s[i+1] == '\'' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			// String values are case-sensitive policy data. Keep their exact
			// spelling so STATUS='ok' cannot be satisfied by STATUS='OK'.
			out = append(out, predicateToken{text: s[start:i], kind: 's'})
		case ch == '$' && postgresDollarQuoteEnd(s, i) > i:
			end := postgresDollarQuoteEnd(s, i)
			delim := s[i:end]
			closeAt := strings.Index(s[end:], delim)
			if closeAt < 0 {
				// Unterminated input is one opaque token; the dialect parser will
				// reject it later, and it cannot spoof a policy predicate here.
				out = append(out, predicateToken{text: s[i:], kind: 's'})
				return out
			}
			closeAt += end + len(delim)
			out = append(out, predicateToken{text: s[i:closeAt], kind: 's'})
			i = closeAt
		case ch == '"' || ch == '`':
			quote := ch
			i++
			var b strings.Builder
			for i < len(s) {
				if s[i] == quote {
					if i+1 < len(s) && s[i+1] == quote {
						b.WriteByte(quote)
						i += 2
						continue
					}
					i++
					break
				}
				b.WriteByte(s[i])
				i++
			}
			out = append(out, predicateToken{text: strings.ToUpper(b.String()), kind: 'i'})
		case isSQLIdentStart(ch):
			start := i
			i++
			for i < len(s) && isSQLIdentPart(s[i]) {
				i++
			}
			out = append(out, predicateToken{text: strings.ToUpper(s[start:i]), kind: 'i'})
		case ch >= '0' && ch <= '9':
			start := i
			i++
			for i < len(s) && ((s[i] >= '0' && s[i] <= '9') || s[i] == '.') {
				i++
			}
			out = append(out, predicateToken{text: s[start:i], kind: 'n'})
		default:
			start := i
			i++
			if i < len(s) {
				two := s[start : i+1]
				switch two {
				case "<=", ">=", "<>", "!=", "||", "&&", "::", ":=", "->", "=>":
					i++
				}
			}
			out = append(out, predicateToken{text: strings.ToUpper(s[start:i]), kind: 'o'})
		}
	}
	return out
}

func isSQLSpace(ch byte) bool {
	return ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == '\f'
}

func isSQLIdentStart(ch byte) bool {
	return ch == '_' || ch == '$' || (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z')
}

func isSQLIdentPart(ch byte) bool {
	return isSQLIdentStart(ch) || ch == '#' || (ch >= '0' && ch <= '9')
}

// postgresDollarQuoteEnd returns the index just after a valid $tag$ opener,
// or start when the bytes begin an ordinary PostgreSQL parameter/identifier.
func postgresDollarQuoteEnd(s string, start int) int {
	if start >= len(s) || s[start] != '$' {
		return start
	}
	for i := start + 1; i < len(s); i++ {
		switch ch := s[i]; {
		case ch == '$':
			return i + 1
		case ch == '_' || (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (i > start+1 && ch >= '0' && ch <= '9'):
			continue
		default:
			return start
		}
	}
	return start
}
