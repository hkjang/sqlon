package dbconn

import (
	"sort"
	"strings"
	"testing"
)

func names(rs []RedundantIndex) string {
	var s []string
	for _, r := range rs {
		s = append(s, r.IndexName+"("+r.Kind+"→"+r.CoveredBy+")")
	}
	sort.Strings(s)
	return strings.Join(s, " ")
}

func TestDetectDuplicateKeepsUnique(t *testing.T) {
	idx := []indexMeta{
		{Table: "t", Name: "idx_a", Columns: []string{"a"}, Unique: false},
		{Table: "t", Name: "uq_a", Columns: []string{"a"}, Unique: true},
	}
	got := detectRedundantIndexes(idx)
	if len(got) != 1 || got[0].IndexName != "idx_a" || got[0].Kind != "duplicate" || got[0].CoveredBy != "uq_a" {
		t.Fatalf("duplicate must drop the non-unique peer, keep the unique: %s", names(got))
	}
}

func TestDetectDuplicateNonUniquePair(t *testing.T) {
	idx := []indexMeta{
		{Table: "t", Name: "idx_b", Columns: []string{"a", "b"}},
		{Table: "t", Name: "idx_a", Columns: []string{"a", "b"}},
	}
	got := detectRedundantIndexes(idx)
	// keeper is lexicographically-first name (idx_a); idx_b reported.
	if len(got) != 1 || got[0].IndexName != "idx_b" || got[0].CoveredBy != "idx_a" {
		t.Fatalf("wrong duplicate keeper: %s", names(got))
	}
}

func TestDetectPrefixRedundancy(t *testing.T) {
	idx := []indexMeta{
		{Table: "t", Name: "idx_a", Columns: []string{"a"}},         // redundant: prefix of idx_ab
		{Table: "t", Name: "idx_ab", Columns: []string{"a", "b"}},   // keeper
		{Table: "t", Name: "idx_bc", Columns: []string{"b", "c"}},   // independent, not a prefix of anything
	}
	got := detectRedundantIndexes(idx)
	if len(got) != 1 || got[0].IndexName != "idx_a" || got[0].Kind != "prefix" || got[0].CoveredBy != "idx_ab" {
		t.Fatalf("prefix redundancy misdetected: %s", names(got))
	}
}

func TestDetectPrefixNeverFlagsUnique(t *testing.T) {
	// A unique index on (a) is NOT redundant even though (a,b) covers the
	// lookup — dropping it would remove the uniqueness guarantee.
	idx := []indexMeta{
		{Table: "t", Name: "uq_a", Columns: []string{"a"}, Unique: true},
		{Table: "t", Name: "idx_ab", Columns: []string{"a", "b"}},
	}
	got := detectRedundantIndexes(idx)
	if len(got) != 0 {
		t.Fatalf("unique index must never be flagged redundant: %s", names(got))
	}
}

func TestDetectDifferentTablesAreIndependent(t *testing.T) {
	idx := []indexMeta{
		{Table: "t1", Name: "idx_a", Columns: []string{"a"}},
		{Table: "t2", Name: "idx_a2", Columns: []string{"a", "b"}},
	}
	if got := detectRedundantIndexes(idx); len(got) != 0 {
		t.Fatalf("indexes on different tables must not interact: %s", names(got))
	}
}

func TestDetectPrefixNotDoubleReportedAsDuplicate(t *testing.T) {
	// idx_a and idx_a2 are duplicates of each other AND both prefixes of idx_ab.
	// Each redundant index should appear exactly once.
	idx := []indexMeta{
		{Table: "t", Name: "idx_a", Columns: []string{"a"}},
		{Table: "t", Name: "idx_a2", Columns: []string{"a"}},
		{Table: "t", Name: "idx_ab", Columns: []string{"a", "b"}},
	}
	got := detectRedundantIndexes(idx)
	seen := map[string]int{}
	for _, r := range got {
		seen[r.IndexName]++
	}
	for name, n := range seen {
		if n != 1 {
			t.Fatalf("index %s reported %d times (want 1): %s", name, n, names(got))
		}
	}
	// idx_a2 duplicate of idx_a; idx_a2 prefix of idx_ab handled once. At least
	// one of the (a)-only indexes must survive as keeper.
	if len(got) < 1 {
		t.Fatalf("expected at least one redundant finding: %s", names(got))
	}
}
