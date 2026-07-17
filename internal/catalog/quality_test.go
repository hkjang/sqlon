package catalog

import "testing"

func TestQualityReportOnMetadb(t *testing.T) {
	c := loadTestCatalog(t) // data/metadb
	rep := c.QualityReport()
	if rep.TableCount == 0 {
		t.Fatal("expected tables")
	}
	if rep.OverallScore <= 0 || rep.OverallScore > 100 {
		t.Fatalf("overall score out of range: %v", rep.OverallScore)
	}
	if rep.OverallGrade == "" {
		t.Fatal("missing overall grade")
	}
	// every table graded, dimensions within range
	for _, tq := range rep.Tables {
		if tq.Grade == "" {
			t.Fatalf("%s: no grade", tq.Table)
		}
		for name, v := range map[string]float64{
			"completeness": tq.Dimensions.Completeness, "consistency": tq.Dimensions.Consistency,
			"relationship": tq.Dimensions.Relationship, "profiling": tq.Dimensions.Profiling,
			"security": tq.Dimensions.Security,
		} {
			if v < 0 || v > 100 {
				t.Fatalf("%s.%s out of range: %v", tq.Table, name, v)
			}
		}
	}
	// metadb has logical names + descriptions + PKs + relations, so it should
	// grade reasonably (not E)
	if rep.OverallGrade == "E" {
		t.Fatalf("well-formed metadb should not grade E, got %s (%v)", rep.OverallGrade, rep.OverallScore)
	}
	// aggregates present
	if len(rep.BySchema) == 0 {
		t.Fatal("expected per-schema aggregates")
	}
}

func TestGradeBoundaries(t *testing.T) {
	cases := map[float64]string{95: "A", 90: "A", 89.9: "B", 75: "B", 74: "C", 60: "C", 59: "D", 40: "D", 39: "E", 0: "E"}
	for score, want := range cases {
		if got := grade(score); got != want {
			t.Errorf("grade(%v) = %s, want %s", score, got, want)
		}
	}
}

func TestQualityGatePassesCleanCatalog(t *testing.T) {
	c := loadTestCatalog(t)
	res := c.QualityGate()
	// metadb is a clean, complete dataset — the gate should pass, and there
	// must be no block-severity violations
	for _, v := range res.Violations {
		if v.Severity == "block" {
			t.Fatalf("clean catalog produced a blocking violation: %+v", v)
		}
	}
	if !res.Pass {
		t.Fatalf("clean catalog must pass the gate: %+v", res.Violations)
	}
}

func TestQualityGateBlocksBrokenMetric(t *testing.T) {
	c := loadTestCatalog(t)
	// inject a metric referencing a non-existent table
	c.Metrics = append(c.Metrics, MetricDef{Name: "phantom", Expression: "COUNT(*)", Tables: []string{"public.no_such_table"}})
	res := c.QualityGate()
	if res.Pass {
		t.Fatal("a metric referencing a missing table must block the release")
	}
	found := false
	for _, v := range res.Violations {
		if v.Code == "METRIC_BROKEN" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected METRIC_BROKEN violation, got %+v", res.Violations)
	}
}

func TestQualityGateBlocksUnclassifiedPII(t *testing.T) {
	c := loadTestCatalog(t)
	// find an email-named column and force it unclassified
	patched := false
	for _, tbl := range c.Tables {
		for _, col := range tbl.Columns {
			if piiLikelyRE.MatchString(col.Name) {
				col.PII = false
				patched = true
			}
		}
	}
	if !patched {
		t.Skip("no PII-likely column in fixture")
	}
	res := c.QualityGate()
	found := false
	for _, v := range res.Violations {
		if v.Code == "PII_UNCLASSIFIED" {
			found = true
		}
	}
	if !found || res.Pass {
		t.Fatalf("unclassified PII must block the release, got pass=%v violations=%+v", res.Pass, res.Violations)
	}
}
