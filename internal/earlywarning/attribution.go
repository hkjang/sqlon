package earlywarning

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"sqlon/internal/collector"
)

// Growth attribution answers the first question after "the disk fills in 3
// days": what is growing? It splits an asset's growth over the last week
// into the series SQLON already keeps — each large table, WAL/binlog, temp
// files, logs, and for a host volume the part outside the database.

type Contributor struct {
	Name  string  `json:"name"`
	Kind  string  `json:"kind"` // table | wal | temp | log | database | outside_db | other
	Bytes float64 `json:"bytes"`
	Share float64 `json:"share"` // of the asset's growth, 0..1
}

type Attribution struct {
	WindowHours  float64       `json:"window_hours"`
	GrowthBytes  float64       `json:"growth_bytes"`
	Contributors []Contributor `json:"contributors"`
}

const attributionWindow = 7 * 24 * time.Hour

// growthOver returns a series' growth inside the window. A series that
// first appears well after the window opened (a table created this week,
// e.g. a CREATE TABLE AS backup copy) grew from zero.
func growthOver(s *Series, from, now time.Time) (float64, bool) {
	first, ok := s.EarliestBetween(from, now)
	last, ok2 := s.Last()
	if !ok || !ok2 || last.T <= first.T {
		return 0, false
	}
	start := first.V
	if first.T == s.Points[0].T && first.T > from.Unix()+2*s.Step {
		start = 0 // the series began inside the window: a new object
	}
	return last.V - start, true
}

func attribute(ps *profileSeries, scope, name string, now time.Time) *Attribution {
	if ps == nil {
		return nil
	}
	asset := ps.Assets[assetKey(scope, name)]
	if asset == nil {
		return nil
	}
	from := now.Add(-attributionWindow)
	first, ok := asset.EarliestBetween(from, now)
	last, ok2 := asset.Last()
	if !ok || !ok2 || last.T-first.T < 3600 {
		return nil
	}
	growth := last.V - first.V
	if growth <= 0 {
		return nil
	}
	from = time.Unix(first.T, 0) // measure every part over the same span
	att := &Attribution{WindowHours: round1(float64(last.T-first.T) / 3600), GrowthBytes: growth}
	add := func(name, kind string, bytes float64) {
		if bytes > 0 {
			att.Contributors = append(att.Contributors, Contributor{Name: name, Kind: kind, Bytes: bytes})
		}
	}
	if scope == collector.ScopeVolume {
		if fp := ps.Assets[assetKey(collector.ScopeStorage, collector.FootprintName)]; fp != nil {
			if g, ok := growthOver(fp, from, now); ok {
				add("DB 점유량", "database", g)
				add("DB 밖 파일", "outside_db", growth-math.Max(g, 0))
			}
		}
	}
	for key, s := range ps.Assets {
		sc, n, _ := strings.Cut(key, ":")
		g, ok := growthOver(s, from, now)
		if !ok {
			continue
		}
		switch sc {
		case "table":
			add(n, "table", g)
		case collector.ScopeWAL:
			label := "WAL"
			if n == "binlog" {
				label = "binlog"
			}
			add(label, "wal", g)
		case collector.ScopeTemp:
			add("임시파일", "temp", g)
		case collector.ScopeLog:
			add("로그", "log", g)
		}
	}
	sort.SliceStable(att.Contributors, func(i, j int) bool {
		// for a volume, the database/outside split leads; then by size
		ri, rj := att.Contributors[i].Kind == "outside_db" || att.Contributors[i].Kind == "database", att.Contributors[j].Kind == "outside_db" || att.Contributors[j].Kind == "database"
		if ri != rj {
			return ri
		}
		return att.Contributors[i].Bytes > att.Contributors[j].Bytes
	})
	if len(att.Contributors) > 6 {
		att.Contributors = att.Contributors[:6]
	}
	for i := range att.Contributors {
		att.Contributors[i].Share = round3(math.Min(1, att.Contributors[i].Bytes/growth))
	}
	if len(att.Contributors) == 0 {
		return nil
	}
	return att
}

// text renders "최근 7.0일 +42.0 GiB 중 public.events +34.5 GiB(82%), …".
func (a *Attribution) text() string {
	if a == nil || len(a.Contributors) == 0 {
		return ""
	}
	span := fmt.Sprintf("%.0f시간", a.WindowHours)
	if a.WindowHours >= 48 {
		span = fmt.Sprintf("%.1f일", a.WindowHours/24)
	}
	var parts []string
	for i, c := range a.Contributors {
		if i == 4 {
			break
		}
		parts = append(parts, fmt.Sprintf("%s +%s(%.0f%%)", c.Name, humanBytes(c.Bytes), c.Share*100))
	}
	return fmt.Sprintf("최근 %s 증가 +%s 중 %s", span, humanBytes(a.GrowthBytes), strings.Join(parts, ", "))
}

// TopGrower names the largest single contributor that is not the
// database/outside split (for the daily report).
func (a *Attribution) TopGrower() *Contributor {
	if a == nil {
		return nil
	}
	for i := range a.Contributors {
		if k := a.Contributors[i].Kind; k != "database" {
			return &a.Contributors[i]
		}
	}
	return nil
}
