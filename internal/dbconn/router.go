package dbconn

import (
	"context"
	"sort"
	"strings"
	"time"
)

// Profile routing: with many registered profiles, "which profile can actually
// serve this SQL?" must not be a guess. The router scores each candidate
// profile (already permission-filtered by the caller) on five signals:
//
//  1. capability — does the target DB really contain the referenced tables?
//     Verified against a live information_schema inventory (cached, TTL) via
//     the same pooled connection the executor uses. Strongest signal.
//  2. declared scope — operator-declared routing.schemas on the profile
//     (works even when the DB is unreachable, and lets an operator pin
//     schemas to a profile explicitly).
//  3. dialect — the SQL's dialect must match the profile engine type.
//  4. health — profiles with an open circuit breaker are excluded.
//  5. operator preference — routing.priority (1 = highest) and
//     routing.default (fallback when nothing else matches).
//
// The decision policy mirrors the ambiguous-table rule elsewhere in jamypg:
// route automatically only when there is one clear winner; otherwise return
// the ranked candidates and make the caller (LLM → user) choose explicitly.

const inventoryTTL = 10 * time.Minute

// inventoryEntry caches one profile's schema→tables map.
type inventoryEntry struct {
	sig       string
	fetchedAt time.Time
	tables    map[string]map[string]bool // schema(lower) -> table(lower) -> true
}

// inventory returns the profile's live table inventory, cached for
// inventoryTTL and invalidated when the profile definition changes.
func (m *Manager) inventory(ctx context.Context, p Profile) (map[string]map[string]bool, error) {
	sig := profileSignature(p)
	m.mu.Lock()
	if m.inventories == nil {
		m.inventories = map[string]*inventoryEntry{}
	}
	if e, ok := m.inventories[p.ID]; ok && e.sig == sig && time.Since(e.fetchedAt) < inventoryTTL {
		m.mu.Unlock()
		return e.tables, nil
	}
	m.mu.Unlock()

	db, err := m.db(p)
	if err != nil {
		return nil, err
	}
	qctx, cancel := context.WithTimeout(ctx, durationSeconds(p.Policy.ConnectTestTimeoutSeconds))
	defer cancel()
	query := `SELECT table_schema, table_name FROM information_schema.tables
WHERE table_schema NOT IN ('pg_catalog','information_schema','mysql','performance_schema','sys')`
	rows, err := db.QueryContext(qctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tables := map[string]map[string]bool{}
	for rows.Next() {
		var schema, name string
		if err := rows.Scan(&schema, &name); err != nil {
			return nil, err
		}
		schema = strings.ToLower(schema)
		name = strings.ToLower(name)
		if tables[schema] == nil {
			tables[schema] = map[string]bool{}
		}
		tables[schema][name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.inventories[p.ID] = &inventoryEntry{sig: sig, fetchedAt: time.Now(), tables: tables}
	m.mu.Unlock()
	return tables, nil
}

// invalidateInventory forgets a profile's cached inventory (or all when id
// is empty); called alongside pool invalidation.
func (m *Manager) invalidateInventory(id string) {
	if m.inventories == nil {
		return
	}
	if id == "" {
		m.inventories = map[string]*inventoryEntry{}
		return
	}
	delete(m.inventories, id)
}

// RouteCandidate is one scored profile.
type RouteCandidate struct {
	ProfileID string   `json:"profile_id"`
	Name      string   `json:"name,omitempty"`
	Type      string   `json:"type"`
	Score     float64  `json:"score"`
	Coverage  string   `json:"coverage"` // full | partial | declared | none | unknown
	Reasons   []string `json:"reasons,omitempty"`
	Default   bool     `json:"default,omitempty"`
	Priority  int      `json:"priority,omitempty"`
}

// RouteDecision is the router output.
type RouteDecision struct {
	Selected   string           `json:"selected_profile,omitempty"`
	Decisive   bool             `json:"decisive"`
	Reason     string           `json:"reason"`
	Tables     []TableRef       `json:"referenced_tables,omitempty"`
	Dialect    string           `json:"dialect,omitempty"`
	Candidates []RouteCandidate `json:"candidates"`
	Excluded   []RouteCandidate `json:"excluded,omitempty"`
}

// RouteProfile scores the given (permission-filtered) profiles for the SQL
// and decides whether one profile is the clear routing target. dialect is
// the SQL's dialect ("" = don't filter by engine type; table extraction then
// tries postgres first, mysql second).
func (m *Manager) RouteProfile(ctx context.Context, sqlText, dialect string, profiles []Profile) RouteDecision {
	dec := RouteDecision{Dialect: dialect}

	// referenced tables via the dialect AST (never by regex)
	var refs []TableRef
	if strings.TrimSpace(sqlText) != "" {
		parseDialects := []string{dialect}
		if dialect == "" {
			parseDialects = []string{"postgres", "mysql"}
		}
		for _, pd := range parseDialects {
			if r, err := ExtractTables(pd, sqlText); err == nil {
				refs = r
				if dec.Dialect == "" {
					dec.Dialect = pd
				}
				break
			}
		}
	}
	dec.Tables = refs

	for _, p := range profiles {
		p = ApplyDefaults(p)
		cand := RouteCandidate{
			ProfileID: p.ID, Name: p.Name, Type: p.Type,
			Default: p.Routing.Default, Priority: p.Routing.Priority,
			Coverage: "unknown",
		}
		// hard filters
		if dialect != "" && p.Type != dialect {
			cand.Reasons = append(cand.Reasons, "excluded: engine type "+p.Type+" does not match SQL dialect "+dialect)
			dec.Excluded = append(dec.Excluded, cand)
			continue
		}
		if err := m.breakerCheck(p.ID); err != nil {
			cand.Reasons = append(cand.Reasons, "excluded: circuit breaker open (recent failures)")
			dec.Excluded = append(dec.Excluded, cand)
			continue
		}

		// declared scope
		declared := map[string]bool{}
		for _, s := range p.Routing.Schemas {
			declared[strings.ToLower(strings.TrimSpace(s))] = true
		}
		if len(refs) > 0 && len(declared) > 0 {
			hit := 0
			for _, r := range refs {
				if r.Schema != "" && declared[r.Schema] {
					hit++
				}
			}
			switch {
			case hit == len(refs):
				cand.Score += 30
				cand.Coverage = "declared"
				cand.Reasons = append(cand.Reasons, "all referenced schemas are declared on this profile")
			case hit > 0:
				cand.Score += 10
				cand.Reasons = append(cand.Reasons, "some referenced schemas are declared on this profile")
			default:
				cand.Score -= 20
				cand.Reasons = append(cand.Reasons, "declared schemas do not include the referenced schemas")
			}
		}

		// live capability
		if len(refs) > 0 && p.Routing.discoverEnabled() {
			inv, err := m.inventory(ctx, p)
			if err != nil {
				cand.Reasons = append(cand.Reasons, "inventory unavailable: "+sanitizeDBError(err))
			} else {
				found := 0
				for _, r := range refs {
					if hasTable(inv, r) {
						found++
					}
				}
				switch {
				case found == len(refs):
					cand.Score += 50
					cand.Coverage = "full"
					cand.Reasons = append(cand.Reasons, "all referenced tables exist on this database (verified)")
				case found > 0:
					cand.Score += 15
					cand.Coverage = "partial"
					cand.Reasons = append(cand.Reasons, "only some referenced tables exist on this database")
				default:
					cand.Score -= 40
					cand.Coverage = "none"
					cand.Reasons = append(cand.Reasons, "none of the referenced tables exist on this database")
				}
			}
		}

		// operator preference
		if p.Routing.Default {
			cand.Score += 8
			cand.Reasons = append(cand.Reasons, "operator default profile")
		}
		prio := p.Routing.Priority
		if prio < 1 {
			prio = 100
		}
		if prio > 100 {
			prio = 100
		}
		cand.Score += float64(100-prio) * 0.1

		dec.Candidates = append(dec.Candidates, cand)
	}

	sort.SliceStable(dec.Candidates, func(i, j int) bool {
		if dec.Candidates[i].Score == dec.Candidates[j].Score {
			return dec.Candidates[i].ProfileID < dec.Candidates[j].ProfileID
		}
		return dec.Candidates[i].Score > dec.Candidates[j].Score
	})

	// decision: automatic only with one clear winner
	switch {
	case len(dec.Candidates) == 0:
		dec.Reason = "no eligible profile (check dialect, permissions, and breaker state)"
	case len(refs) == 0:
		// nothing to match capability against (e.g. SELECT 1)
		if len(dec.Candidates) == 1 {
			dec.Selected = dec.Candidates[0].ProfileID
			dec.Decisive = true
			dec.Reason = "single eligible profile"
		} else {
			dec.Reason = "SQL references no tables; multiple eligible profiles — specify one explicitly"
		}
	default:
		top := dec.Candidates[0]
		strong := top.Coverage == "full" || top.Coverage == "declared"
		clear := len(dec.Candidates) == 1 ||
			top.Score-dec.Candidates[1].Score >= 10 ||
			(strong && dec.Candidates[1].Coverage != "full" && dec.Candidates[1].Coverage != "declared")
		if strong && clear && top.Score > 0 {
			dec.Selected = top.ProfileID
			dec.Decisive = true
			dec.Reason = "clear winner: " + strings.Join(top.Reasons, "; ")
		} else {
			dec.Reason = "no clear winner — top candidates are tied or unverified; choose explicitly"
		}
	}
	return dec
}

func hasTable(inv map[string]map[string]bool, r TableRef) bool {
	if r.Schema != "" {
		return inv[r.Schema] != nil && inv[r.Schema][r.Name]
	}
	for _, tables := range inv {
		if tables[r.Name] {
			return true
		}
	}
	return false
}
