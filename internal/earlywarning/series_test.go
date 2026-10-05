package earlywarning

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

func TestSeriesDownsamplesWithLiveTail(t *testing.T) {
	s := newSeries(10 * time.Minute)
	for i := 0; i <= 30; i++ { // one sample per minute for 30 minutes
		s.Add(t0.Add(time.Duration(i)*time.Minute), float64(i), seriesRetention)
	}
	if len(s.Points) > 5 {
		t.Fatalf("30 one-minute samples at a 10-minute step kept %d points", len(s.Points))
	}
	last, _ := s.Last()
	if last.V != 30 || last.T != t0.Add(30*time.Minute).Unix() {
		t.Fatalf("the newest sample must always be present, got %+v", last)
	}
	for i := 1; i < len(s.Points)-1; i++ {
		if gap := s.Points[i].T - s.Points[i-1].T; gap < 9*60 {
			t.Fatalf("committed points closer than the step: %v", s.Points)
		}
	}
}

func TestSeriesDropsOutOfOrderAndOldSamples(t *testing.T) {
	s := newSeries(time.Hour)
	s.Add(t0, 1, 48*time.Hour)
	s.Add(t0.Add(-time.Minute), 99, 48*time.Hour)
	if len(s.Points) != 1 || s.Points[0].V != 1 {
		t.Fatalf("an older sample must be ignored: %+v", s.Points)
	}
	for h := 1; h <= 72; h++ {
		s.Add(t0.Add(time.Duration(h)*time.Hour), float64(h), 48*time.Hour)
	}
	if first := s.Points[0].T; first < t0.Add(24*time.Hour).Unix() {
		t.Fatalf("points older than retention survived: first=%d", first)
	}
}

func TestSeriesJSONIsCompact(t *testing.T) {
	s := newSeries(time.Hour)
	s.Add(t0, 1.5, seriesRetention)
	b, _ := json.Marshal(s)
	if want := fmt.Sprintf(`{"step":3600,"points":[[%d,1.5]]}`, t0.Unix()); string(b) != want {
		t.Fatalf("unexpected encoding %s", b)
	}
	var back Series
	if err := json.Unmarshal(b, &back); err != nil || back.Points[0].T != t0.Unix() || back.Points[0].V != 1.5 {
		t.Fatalf("round trip: %+v %v", back, err)
	}
}

func TestFitRecoversLinearGrowth(t *testing.T) {
	s := newSeries(10 * time.Minute)
	perDay := 50 * gib
	for m := 0; m <= 8*60; m += 10 {
		at := t0.Add(time.Duration(m) * time.Minute)
		s.Add(at, 100*gib+perDay*at.Sub(t0).Hours()/24, seriesRetention)
	}
	now := t0.Add(8 * time.Hour)
	fit := s.Fit(now, shortWindow)
	if !fit.Valid || math.Abs(fit.BytesPerDay-perDay)/perDay > 0.001 || fit.R2 < 0.999 {
		t.Fatalf("expected ~%v/day with R2≈1, got %+v", perDay, fit)
	}
	if long := s.Fit(now, longWindow); long.Valid {
		t.Fatalf("8 hours of history must not produce a 7-day fit: %+v", long)
	}
}

func TestFitNeedsSpanAndPoints(t *testing.T) {
	s := newSeries(time.Minute)
	for m := 0; m < 30; m++ {
		s.Add(t0.Add(time.Duration(m)*time.Minute), float64(m), seriesRetention)
	}
	fit := s.Fit(t0.Add(30*time.Minute), shortWindow)
	if fit.Valid || fit.InvalidCause == "" {
		t.Fatalf("30 minutes is below the 2h minimum span: %+v", fit)
	}
	var empty *Series
	if f := empty.Fit(t0, shortWindow); f.Valid {
		t.Fatalf("nil series fit must be invalid")
	}
}

func TestSeriesKeepsSharpJumpsInsideAStep(t *testing.T) {
	s := newSeries(time.Hour)
	s.Add(t0, 0, seriesRetention)
	for m := 1; m <= 5; m++ { // bulk load to 1.2GiB within the hour
		s.Add(t0.Add(time.Duration(m)*time.Minute), 1.2*gib*float64(m)/5, seriesRetention)
	}
	s.Add(t0.Add(6*time.Minute), 0, seriesRetention) // TRUNCATE
	s.Add(t0.Add(7*time.Minute), 0, seriesRetention)
	peak, ok := s.MaxSince(t0)
	if !ok || peak.V < 1.2*gib {
		t.Fatalf("the 1.2GiB peak before the truncate was erased by downsampling: %v", s.Points)
	}
	if last, _ := s.Last(); last.V != 0 {
		t.Fatalf("the latest value must still be current: %v", s.Points)
	}
	if len(s.Points) > 8 {
		t.Fatalf("jump commits must stay bounded: %d points", len(s.Points))
	}
}
