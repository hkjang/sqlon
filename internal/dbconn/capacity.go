package dbconn

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// CapacityConfig is the operator-declared storage envelope that early-warning
// forecasts run against. PostgreSQL and MySQL cannot report free disk space
// through SQL, so without a declared limit SQLON can measure how fast a
// database grows but cannot say when its volume fills.
type CapacityConfig struct {
	// StorageLimit is the size of the volume (or quota) that holds the data
	// directory and WAL — "500GiB", "2TB", "750G", or plain bytes. Decimal
	// units (GB, TB) are powers of 1000 and binary units (GiB, TiB, and the
	// bare G/T shorthand that df prints) are powers of 1024.
	StorageLimit string `json:"storage_limit,omitempty"`
	// Usage thresholds in percent of StorageLimit (defaults 80 / 90).
	WarnPercent     float64 `json:"warn_percent,omitempty"`
	CriticalPercent float64 `json:"critical_percent,omitempty"`
	// Projected days-until-full thresholds (defaults 14 / 3).
	WarnDays     float64 `json:"warn_days,omitempty"`
	CriticalDays float64 `json:"critical_days,omitempty"`
}

const (
	DefaultCapacityWarnPercent     = 80.0
	DefaultCapacityCriticalPercent = 90.0
	DefaultCapacityWarnDays        = 14.0
	DefaultCapacityCriticalDays    = 3.0
)

// LimitBytes parses StorageLimit. A nil config or an empty limit is 0, nil.
func (c *CapacityConfig) LimitBytes() (int64, error) {
	if c == nil || strings.TrimSpace(c.StorageLimit) == "" {
		return 0, nil
	}
	return ParseByteSize(c.StorageLimit)
}

// Thresholds returns the effective usage and days-to-full thresholds with
// defaults applied. It is safe on a nil config.
func (c *CapacityConfig) Thresholds() (warnPct, critPct, warnDays, critDays float64) {
	warnPct, critPct = DefaultCapacityWarnPercent, DefaultCapacityCriticalPercent
	warnDays, critDays = DefaultCapacityWarnDays, DefaultCapacityCriticalDays
	if c == nil {
		return
	}
	if c.WarnPercent > 0 {
		warnPct = c.WarnPercent
	}
	if c.CriticalPercent > 0 {
		critPct = c.CriticalPercent
	}
	if c.WarnDays > 0 {
		warnDays = c.WarnDays
	}
	if c.CriticalDays > 0 {
		critDays = c.CriticalDays
	}
	return
}

func (c *CapacityConfig) validate() error {
	if c == nil {
		return nil
	}
	if _, err := c.LimitBytes(); err != nil {
		return fmt.Errorf("capacity.storage_limit: %w", err)
	}
	for name, v := range map[string]float64{"warn_percent": c.WarnPercent, "critical_percent": c.CriticalPercent} {
		if v < 0 || v > 100 {
			return fmt.Errorf("capacity.%s must be between 0 and 100", name)
		}
	}
	for name, v := range map[string]float64{"warn_days": c.WarnDays, "critical_days": c.CriticalDays} {
		if v < 0 || v > 3650 {
			return fmt.Errorf("capacity.%s must be between 0 and 3650", name)
		}
	}
	warnPct, critPct, warnDays, critDays := c.Thresholds()
	if critPct < warnPct {
		return errors.New("capacity.critical_percent must not be below warn_percent")
	}
	if critDays > warnDays {
		return errors.New("capacity.critical_days must not exceed warn_days")
	}
	return nil
}

var byteUnits = map[string]float64{
	"": 1, "b": 1,
	"k": 1 << 10, "kb": 1e3, "kib": 1 << 10,
	"m": 1 << 20, "mb": 1e6, "mib": 1 << 20,
	"g": 1 << 30, "gb": 1e9, "gib": 1 << 30,
	"t": 1 << 40, "tb": 1e12, "tib": 1 << 40,
	"p": 1 << 50, "pb": 1e15, "pib": 1 << 50,
}

// ParseByteSize parses a human byte size such as "500GiB", "1.5 TB" or
// "1073741824".
func ParseByteSize(raw string) (int64, error) {
	s := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(raw), " ", ""))
	if s == "" {
		return 0, errors.New("empty size")
	}
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	number, unit := s[:i], s[i:]
	mult, ok := byteUnits[unit]
	if number == "" || !ok {
		return 0, fmt.Errorf("invalid size %q (use e.g. 500GiB, 2TB, or bytes)", raw)
	}
	v, err := strconv.ParseFloat(number, 64)
	if err != nil || v <= 0 || math.IsInf(v*mult, 0) || v*mult >= math.MaxInt64 {
		return 0, fmt.Errorf("invalid size %q", raw)
	}
	return int64(v * mult), nil
}
