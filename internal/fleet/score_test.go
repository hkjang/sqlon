package fleet

import "testing"

func TestScoreGradeBoundaries(t *testing.T) {
	cases := []struct {
		risk  int
		score int
		grade string
	}{
		{0, 100, "A"},
		{10, 90, "A"},
		{25, 75, "B"},
		{40, 60, "C"},
		{60, 40, "D"},
		{61, 39, "F"},
		{100, 0, "F"},
		{150, 0, "F"}, // clamp
	}
	for _, c := range cases {
		gotScore, gotGrade := scoreGrade(c.risk)
		if gotScore != c.score || gotGrade != c.grade {
			t.Errorf("scoreGrade(%d)=(%d,%s), want (%d,%s)", c.risk, gotScore, gotGrade, c.score, c.grade)
		}
	}
}

func TestFinalizeScoresAverageAndWorst(t *testing.T) {
	h := Health{Data: []Instance{
		{HealthScore: 100}, {HealthScore: 60}, {HealthScore: 20},
	}, Summary: newSummary()}
	finalizeScores(&h)
	if h.Summary.AverageScore != 60 {
		t.Fatalf("average want 60, got %d", h.Summary.AverageScore)
	}
	if h.Summary.WorstScore != 20 {
		t.Fatalf("worst want 20, got %d", h.Summary.WorstScore)
	}
}

func TestFinalizeScoresEmptyFleet(t *testing.T) {
	h := Health{Data: []Instance{}, Summary: newSummary()}
	finalizeScores(&h) // must not divide by zero
	if h.Summary.AverageScore != 0 || h.Summary.WorstScore != 0 {
		t.Fatalf("empty fleet should be 0/0, got %d/%d", h.Summary.AverageScore, h.Summary.WorstScore)
	}
}
