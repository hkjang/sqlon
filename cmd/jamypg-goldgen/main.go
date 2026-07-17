// jamypg-goldgen builds/refreshes the golden evaluation set by selecting
// representative cases from sql_datasets.json: stratified across business
// domain and difficulty, restricted to cases whose tables and columns
// actually exist in the compiled catalog. Hand-curated entries at the top of
// the existing golden file are preserved.
//
// Usage:
//
//	go run ./cmd/jamypg-goldgen -data ./data/metadb -n 80
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"sqlon/internal/catalog"
)

func main() {
	var (
		dataDir string
		outPath string
		total   int
		keep    int
	)
	flag.StringVar(&dataDir, "data", filepath.Join("data", "metadb"), "Path to metadata dataset directory")
	flag.StringVar(&outPath, "out", "", "Output path (default <data>/golden_queries.json)")
	flag.IntVar(&total, "n", 80, "Target total number of golden cases (including kept curated entries)")
	flag.IntVar(&keep, "keep", 5, "Number of leading entries of the existing golden file to preserve as curated")
	flag.Parse()
	if outPath == "" {
		outPath = filepath.Join(dataDir, "golden_queries.json")
	}

	cat, err := catalog.Load(dataDir)
	if err != nil {
		log.Fatalf("load catalog: %v", err)
	}

	var curated []catalog.GoldenQuery
	if b, err := os.ReadFile(outPath); err == nil {
		var existing []catalog.GoldenQuery
		if err := json.Unmarshal(b, &existing); err != nil {
			log.Fatalf("parse existing golden file: %v", err)
		}
		if keep > len(existing) {
			keep = len(existing)
		}
		curated = existing[:keep]
	}
	seenQuestion := map[string]bool{}
	for _, g := range curated {
		seenQuestion[normalize(g.Question)] = true
	}

	// bucket candidate samples by domain x difficulty for stratified picking
	type candidate struct {
		g        catalog.GoldenQuery
		sourceID string
	}
	buckets := map[string][]candidate{}
	skipped := map[string]int{}
	for _, s := range cat.Samples {
		q := strings.TrimSpace(s.Question)
		if q == "" || strings.TrimSpace(s.TargetSQL) == "" || seenQuestion[normalize(q)] {
			skipped["empty_or_duplicate"]++
			continue
		}
		tables, ok := resolveTables(cat, s.TargetTable)
		if !ok || len(tables) == 0 {
			skipped["unresolved_tables"]++
			continue
		}
		columns := resolveColumns(cat, s.TargetColumn, tables)
		g := catalog.GoldenQuery{
			Question:        q,
			ExpectedTables:  tables,
			ExpectedColumns: columns,
			ExpectedMetrics: cat.MetricNamesInQuestion(q),
			Note:            fmt.Sprintf("auto-generated from sql_datasets id=%v domain=%s difficulty=%s", s.ID, s.TargetDomain, s.TargetDifficulty),
		}
		// keep expected_sql only when it passes static validation, so the
		// eval's SQL-validity metric measures our validator against known-good
		// SQL instead of inheriting dataset typos.
		if v := cat.ValidateSQL(catalog.ValidateRequest{SQL: s.TargetSQL}); v.Valid {
			g.ExpectedSQL = s.TargetSQL
		}
		key := strings.TrimSpace(s.TargetDomain) + "|" + strings.TrimSpace(s.TargetDifficulty)
		buckets[key] = append(buckets[key], candidate{g: g, sourceID: fmt.Sprint(s.ID)})
	}

	// deterministic order inside buckets and across bucket keys
	keys := make([]string, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
		sort.SliceStable(buckets[k], func(i, j int) bool { return buckets[k][i].sourceID < buckets[k][j].sourceID })
	}
	sort.Strings(keys)

	want := total - len(curated)
	if want < 0 {
		want = 0
	}
	selected := []catalog.GoldenQuery{}
	for round := 0; len(selected) < want; round++ {
		progressed := false
		for _, k := range keys {
			if len(selected) >= want {
				break
			}
			if round < len(buckets[k]) {
				c := buckets[k][round]
				if seenQuestion[normalize(c.g.Question)] {
					continue
				}
				seenQuestion[normalize(c.g.Question)] = true
				selected = append(selected, c.g)
				progressed = true
			}
		}
		if !progressed {
			break
		}
	}

	out := append(append([]catalog.GoldenQuery{}, curated...), selected...)
	for i := range out {
		out[i].ID = i + 1
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(outPath, append(b, '\n'), 0o644); err != nil {
		log.Fatal(err)
	}
	withSQL := 0
	for _, g := range out {
		if g.ExpectedSQL != "" {
			withSQL++
		}
	}
	log.Printf("wrote %s: %d cases (%d curated + %d generated from %d buckets), %d with validated expected_sql; skipped: %v",
		outPath, len(out), len(curated), len(selected), len(keys), withSQL, skipped)
}

func resolveTables(cat *catalog.Catalog, targetTable string) ([]string, bool) {
	var out []string
	seen := map[string]bool{}
	for _, raw := range strings.Split(targetTable, "|") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		t, ok := cat.ResolveTable(raw)
		if !ok {
			return nil, false
		}
		if !seen[t.FQN] {
			seen[t.FQN] = true
			out = append(out, t.FQN)
		}
	}
	return out, true
}

func resolveColumns(cat *catalog.Catalog, targetColumn string, tableFQNs []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, raw := range strings.Split(targetColumn, "|") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		col := raw
		if i := strings.LastIndex(raw, "."); i >= 0 {
			col = raw[i+1:]
		}
		col = strings.ToUpper(strings.TrimSpace(col))
		if col == "" || seen[col] {
			continue
		}
		for _, fqn := range tableFQNs {
			if t, ok := cat.ResolveTable(fqn); ok && t.ColumnMap[col] != nil {
				seen[col] = true
				out = append(out, col)
				break
			}
		}
		if len(out) >= 6 {
			break
		}
	}
	return out
}

func normalize(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}
