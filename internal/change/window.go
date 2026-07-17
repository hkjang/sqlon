package change

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Window is a recurring maintenance window during which a change may execute.
// Times are minutes-of-day interpreted in UTC (the service clock is UTC); a
// window whose End is not after Start wraps past midnight. An empty Days list
// means every day.
type Window struct {
	Days  []string `json:"days,omitempty"` // mon,tue,wed,thu,fri,sat,sun (case-insensitive)
	Start string   `json:"start"`          // "HH:MM"
	End   string   `json:"end"`            // "HH:MM"
}

var weekdayNames = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

func parseHHMM(v string) (int, error) {
	parts := strings.Split(strings.TrimSpace(v), ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("time %q must be HH:MM", v)
	}
	h, herr := strconv.Atoi(parts[0])
	m, merr := strconv.Atoi(parts[1])
	if herr != nil || merr != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("time %q must be HH:MM within 00:00..23:59", v)
	}
	return h*60 + m, nil
}

// Validate checks the window is well-formed.
func (w Window) Validate() error {
	if _, err := parseHHMM(w.Start); err != nil {
		return fmt.Errorf("window start: %w", err)
	}
	if _, err := parseHHMM(w.End); err != nil {
		return fmt.Errorf("window end: %w", err)
	}
	for _, day := range w.Days {
		if _, ok := weekdayNames[strings.ToLower(strings.TrimSpace(day))]; !ok {
			return fmt.Errorf("window day %q is not a weekday (mon..sun)", day)
		}
	}
	return nil
}

// Contains reports whether t (in UTC) falls within this window.
func (w Window) Contains(t time.Time) bool {
	t = t.UTC()
	start, err := parseHHMM(w.Start)
	if err != nil {
		return false
	}
	end, err := parseHHMM(w.End)
	if err != nil {
		return false
	}
	minute := t.Hour()*60 + t.Minute()

	inClock := false
	if start <= end {
		inClock = minute >= start && minute < end
		if !inClock {
			return false
		}
		return w.dayAllowed(t.Weekday())
	}
	// Wraps past midnight: [start, 24:00) belongs to today, [00:00, end) to the
	// next day — so the day check applies to the window's *start* day.
	if minute >= start {
		return w.dayAllowed(t.Weekday())
	}
	if minute < end {
		return w.dayAllowed(previousWeekday(t.Weekday()))
	}
	return false
}

func (w Window) dayAllowed(day time.Weekday) bool {
	if len(w.Days) == 0 {
		return true
	}
	for _, name := range w.Days {
		if weekdayNames[strings.ToLower(strings.TrimSpace(name))] == day {
			return true
		}
	}
	return false
}

func previousWeekday(d time.Weekday) time.Weekday {
	return time.Weekday((int(d) + 6) % 7)
}

// MaintenanceGate reports whether execution is allowed now. A plan with no
// structured windows is always allowed. Emergency changes bypass the gate so
// incident response is never blocked by a maintenance schedule.
func (p *Plan) MaintenanceGate(now time.Time) (allowed bool, reason string) {
	if p.Risk == Emergency {
		return true, "emergency 변경은 유지보수 창 제약을 우회합니다"
	}
	if len(p.MaintenanceWindows) == 0 {
		return true, ""
	}
	for _, w := range p.MaintenanceWindows {
		if w.Contains(now) {
			return true, ""
		}
	}
	return false, "현재 시각이 승인된 유지보수 창에 포함되지 않습니다"
}
