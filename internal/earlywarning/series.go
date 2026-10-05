package earlywarning

import (
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// Point is one (unix seconds, value) sample, encoded as a two-element array
// to keep persisted series compact.
type Point struct {
	T int64
	V float64
}

func (p Point) MarshalJSON() ([]byte, error) { return json.Marshal([2]float64{float64(p.T), p.V}) }

func (p *Point) UnmarshalJSON(b []byte) error {
	var raw [2]float64
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	p.T, p.V = int64(raw[0]), raw[1]
	return nil
}

// Series is a downsampled time series: committed points are at least Step
// seconds apart and the newest point is a live tail that each sample inside
// the current step overwrites, so the latest value is always present. A
// sharp jump commits the tail instead of overwriting it (see jumped).
type Series struct {
	Step   int64   `json:"step"`
	Points []Point `json:"points"`
}

func newSeries(step time.Duration) *Series { return &Series{Step: int64(step / time.Second)} }

// Jumps at least this large are kept as their own point even inside a step,
// so downsampling never erases a bulk load, a TRUNCATE, or a WAL purge.
const (
	jumpFraction = 0.10
	jumpMinBytes = 64 << 20
)

func jumped(from, to float64) bool {
	d := math.Abs(to - from)
	return d >= jumpMinBytes && d >= jumpFraction*math.Abs(from)
}

// Add records a sample and drops points older than retention. Samples at or
// before the newest point are ignored.
func (s *Series) Add(t time.Time, v float64, retention time.Duration) {
	ts := t.Unix()
	n := len(s.Points)
	switch {
	case n > 0 && ts <= s.Points[n-1].T:
		return
	case n >= 2 && ts-s.Points[n-2].T < s.Step && !jumped(s.Points[n-1].V, v):
		s.Points[n-1] = Point{T: ts, V: v}
	default:
		s.Points = append(s.Points, Point{T: ts, V: v})
	}
	cutoff := t.Add(-retention).Unix()
	drop := 0
	for drop < len(s.Points)-1 && s.Points[drop].T < cutoff {
		drop++
	}
	if drop > 0 {
		s.Points = append(s.Points[:0], s.Points[drop:]...)
	}
}

// Last returns the newest point.
func (s *Series) Last() (Point, bool) {
	if s == nil || len(s.Points) == 0 {
		return Point{}, false
	}
	return s.Points[len(s.Points)-1], true
}

// At returns the point closest to t within tolerance.
func (s *Series) At(t time.Time, tolerance time.Duration) (Point, bool) {
	if s == nil {
		return Point{}, false
	}
	target, tol := t.Unix(), int64(tolerance/time.Second)
	best, bestDist := Point{}, int64(-1)
	for _, p := range s.Points {
		d := p.T - target
		if d < 0 {
			d = -d
		}
		if d <= tol && (bestDist < 0 || d < bestDist) {
			best, bestDist = p, d
		}
	}
	return best, bestDist >= 0
}

// EarliestBetween returns the oldest point with from <= T <= to.
func (s *Series) EarliestBetween(from, to time.Time) (Point, bool) {
	if s == nil {
		return Point{}, false
	}
	for _, p := range s.Points {
		if p.T >= from.Unix() && p.T <= to.Unix() {
			return p, true
		}
	}
	return Point{}, false
}

// MaxSince returns the largest value with T >= since.
func (s *Series) MaxSince(since time.Time) (Point, bool) {
	if s == nil {
		return Point{}, false
	}
	best, ok := Point{}, false
	for _, p := range s.Points {
		if p.T >= since.Unix() && (!ok || p.V > best.V) {
			best, ok = p, true
		}
	}
	return best, ok
}

type windowSpec struct {
	name      string
	window    time.Duration
	minSpan   time.Duration
	minPoints int
}

var (
	// The short window catches a surge (an archiver that stopped, a slot that
	// pins WAL) within hours; the long one is the trend a volume is sized by.
	shortWindow = windowSpec{name: "6h", window: 6 * time.Hour, minSpan: 2 * time.Hour, minPoints: 6}
	longWindow  = windowSpec{name: "7d", window: 7 * 24 * time.Hour, minSpan: 24 * time.Hour, minPoints: 12}
)

// Fit fits a least-squares line through the points inside the window
// ending at now. The fit is invalid when the window holds too few points or
// spans too little time to say anything about a trend.
func (s *Series) Fit(now time.Time, spec windowSpec) Fit {
	fit := Fit{Window: spec.name}
	if s == nil {
		fit.InvalidCause = "이력 없음"
		return fit
	}
	from := now.Add(-spec.window).Unix()
	var pts []Point
	for _, p := range s.Points {
		if p.T >= from && p.T <= now.Unix() {
			pts = append(pts, p)
		}
	}
	fit.Points = len(pts)
	if len(pts) >= 2 {
		fit.SpanHours = round1(float64(pts[len(pts)-1].T-pts[0].T) / 3600)
	}
	switch {
	case len(pts) < spec.minPoints:
		fit.InvalidCause = fmt.Sprintf("표본 %d개 (최소 %d개 필요)", len(pts), spec.minPoints)
		return fit
	case fit.SpanHours*float64(time.Hour) < float64(spec.minSpan):
		fit.InvalidCause = fmt.Sprintf("관측 구간 %.1f시간 (최소 %.0f시간 필요)", fit.SpanHours, spec.minSpan.Hours())
		return fit
	}
	t0 := float64(pts[0].T)
	var sx, sy, sxx, sxy float64
	n := float64(len(pts))
	for _, p := range pts {
		x := float64(p.T) - t0
		sx += x
		sy += p.V
		sxx += x * x
		sxy += x * p.V
	}
	den := n*sxx - sx*sx
	if den == 0 {
		fit.InvalidCause = "시간축 분산 없음"
		return fit
	}
	slope := (n*sxy - sx*sy) / den
	intercept := (sy - slope*sx) / n
	mean := sy / n
	var ssTot, ssRes float64
	for _, p := range pts {
		x := float64(p.T) - t0
		ssTot += (p.V - mean) * (p.V - mean)
		r := p.V - (intercept + slope*x)
		ssRes += r * r
	}
	r2 := 1.0
	if ssTot > 0 {
		r2 = math.Max(0, 1-ssRes/ssTot)
	}
	fit.BytesPerDay = math.Round(slope * 86400)
	fit.R2 = round3(r2)
	fit.Valid = true
	return fit
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
