package earlywarning

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// The daily report is a once-a-morning outlook to the default channel: how
// full each database is, how fast it grows, when it fills, and which
// databases SQLON cannot forecast. Alerts say "act now"; the report is what
// keeps a slow 60-day exhaustion from becoming tomorrow's alert.

const digestWindow = 2 * time.Hour

// ParseDigestTime validates an "HH:MM" daily report time ("" or "off"
// disables it).
func ParseDigestTime(v string) (string, error) {
	v = strings.TrimSpace(strings.ToLower(v))
	if v == "" || v == "off" {
		return "", nil
	}
	h, m, ok := strings.Cut(v, ":")
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if !ok || err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return "", fmt.Errorf("daily report time %q must be HH:MM or off", v)
	}
	return fmt.Sprintf("%02d:%02d", hh, mm), nil
}

// digestDue reports whether today's report should go out now: inside the
// two hours after the configured time, and not yet sent today.
func digestDue(at string, last string, now time.Time) (string, bool) {
	if at == "" {
		return "", false
	}
	local := now.In(displayLocation())
	today := local.Format("2006-01-02")
	if last == today {
		return today, false
	}
	hh, _ := strconv.Atoi(at[:2])
	mm, _ := strconv.Atoi(at[3:])
	start := time.Date(local.Year(), local.Month(), local.Day(), hh, mm, 0, 0, local.Location())
	return today, !local.Before(start) && local.Before(start.Add(digestWindow))
}

var weekdayKo = []string{"일", "월", "화", "수", "목", "금", "토"}

// worstForecast picks the forecast a person should look at first: the most
// severe, then the soonest to fill, then the fullest.
func worstForecast(fcs []Forecast) *Forecast {
	var best *Forecast
	score := func(f *Forecast) (int, float64, float64) {
		days := math.Inf(1)
		if f.DaysToFull != nil {
			days = *f.DaysToFull
		}
		return Rank(f.Status), -days, f.UsagePercent
	}
	for i := range fcs {
		f := &fcs[i]
		if best == nil {
			best = f
			continue
		}
		r1, d1, u1 := score(f)
		r2, d2, u2 := score(best)
		if r1 > r2 || r1 == r2 && (d1 > d2 || d1 == d2 && u1 > u2) {
			best = f
		}
	}
	return best
}

// FormatDigest renders the daily report.
func FormatDigest(b Board, now time.Time) string {
	local := now.In(displayLocation())
	var s strings.Builder
	fmt.Fprintf(&s, "#### 📊 SQLON 일일 용량 리포트 — %s (%s)\n", local.Format("2006-01-02"), weekdayKo[local.Weekday()])
	fmt.Fprintf(&s, "발생 중 경보: 긴급 %d · 경고 %d · 정보 %d — 감시 DB %d개\n", b.Summary.Critical, b.Summary.Warning, b.Summary.Info, b.Summary.Profiles)
	var noLimit, down []string
	for _, p := range b.Profiles {
		name := p.Name
		if name == "" {
			name = p.ProfileID
		}
		if p.ConsecutiveFailures > 0 {
			down = append(down, name)
		}
		f := worstForecast(p.Forecasts)
		if f == nil {
			fmt.Fprintf(&s, "\n%s **%s** — 용량 데이터 없음", statusIcon(p.Status), name)
			continue
		}
		line := fmt.Sprintf("\n%s **%s** — ", statusIcon(p.Status), name)
		if f.LimitBytes > 0 {
			line += fmt.Sprintf("%.1f%% (%s / %s)", f.UsagePercent, humanBytes(f.UsedBytes), humanBytes(f.LimitBytes))
		} else {
			line += humanBytes(f.UsedBytes) + " (한도 미선언)"
			noLimit = append(noLimit, name)
		}
		if g := f.GrowthLong; g != nil && g.Valid {
			line += fmt.Sprintf(" · 7일 추세 %s/일", humanBytes(g.BytesPerDay))
		} else if g := f.GrowthShort; g != nil && g.Valid {
			line += fmt.Sprintf(" · 6시간 추세 %s/일", humanBytes(g.BytesPerDay))
		}
		switch {
		case f.DaysToFull != nil && f.FullAt != nil:
			line += fmt.Sprintf(" · **%s 후 가득 참** (%s)", humanDays(*f.DaysToFull), f.FullAt.In(displayLocation()).Format("01-02"))
		case f.LimitBytes > 0:
			line += " · 증가 추세 없음"
		}
		if f.Scope == "volume" {
			line += " · 디스크 " + f.Name
		}
		s.WriteString(line)
	}
	if len(noLimit) > 0 {
		fmt.Fprintf(&s, "\n\n용량 한도가 없어 고갈 시점을 계산하지 못하는 DB: %s", strings.Join(noLimit, ", "))
	}
	if len(down) > 0 {
		fmt.Fprintf(&s, "\n최근 수집에 실패한 DB: %s", strings.Join(down, ", "))
	}
	for _, c := range append([]DeliveryStatus{b.Delivery}, b.Channels...) {
		if c.ConsecutiveFailures > 0 {
			fmt.Fprintf(&s, "\n⚠️ 알림 채널 %s 전달 실패 중: %s", c.Target, c.LastError)
		}
	}
	return s.String()
}

func statusIcon(status string) string {
	switch status {
	case SevCritical:
		return "🔴"
	case SevWarning:
		return "🟠"
	case "pending":
		return "⚪"
	}
	return "🟢"
}
