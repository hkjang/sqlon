package earlywarning

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"sqlon/internal/collector"
	"sqlon/internal/dbconn"
)

// Host disk reports close the blind spot SQL cannot see: everything else on
// the database's volume (dump backups, external logs, other programs) and the
// volume's real size. A small agent on the DB host (scripts/
// sqlon-disk-report.sh, run from cron) posts `df` results; they become
// "volume:<mount>" assets with the filesystem's own limit, forecast and
// alerted like any other storage asset.

// DiskVolume is one filesystem as `df` reports it.
type DiskVolume struct {
	Mount      string  `json:"mount"`
	Filesystem string  `json:"filesystem,omitempty"`
	TotalBytes float64 `json:"total_bytes"`
	UsedBytes  float64 `json:"used_bytes"`
	AvailBytes float64 `json:"avail_bytes"`
}

// HostDiskReport is the latest report received for a profile.
type HostDiskReport struct {
	Host       string       `json:"host,omitempty"`
	Volumes    []DiskVolume `json:"volumes"`
	ReportedAt time.Time    `json:"reported_at"`
}

const maxReportVolumes = 32

// Validate checks a report's shape; numbers come straight from df.
func (r HostDiskReport) Validate() error {
	if len(r.Volumes) == 0 || len(r.Volumes) > maxReportVolumes {
		return fmt.Errorf("volumes: 1..%d required", maxReportVolumes)
	}
	seen := map[string]bool{}
	for _, v := range r.Volumes {
		mount := strings.TrimSpace(v.Mount)
		switch {
		case mount == "" || len(mount) > 256:
			return errors.New("volume mount is required (max 256 chars)")
		case seen[mount]:
			return fmt.Errorf("volume %s reported twice", mount)
		case v.TotalBytes <= 0 || v.UsedBytes < 0 || v.AvailBytes < 0:
			return fmt.Errorf("volume %s: total_bytes must be > 0 and used/avail >= 0", mount)
		case v.UsedBytes > v.TotalBytes*1.01 || v.AvailBytes > v.TotalBytes*1.01:
			return fmt.Errorf("volume %s: used/avail exceed total", mount)
		}
		seen[mount] = true
	}
	return nil
}

// fillableBytes is what the database can still write into: used + avail.
// On ext4 this excludes the blocks reserved for root, which df's total
// includes and which PostgreSQL can never use.
func (v DiskVolume) fillableBytes() float64 {
	if v.UsedBytes+v.AvailBytes > 0 {
		return v.UsedBytes + v.AvailBytes
	}
	return v.TotalBytes
}

func (r HostDiskReport) capacity() []collector.Capacity {
	out := make([]collector.Capacity, 0, len(r.Volumes))
	for _, v := range r.Volumes {
		limit := v.fillableBytes()
		out = append(out, collector.Capacity{Scope: collector.ScopeVolume, Name: strings.TrimSpace(v.Mount), UsedBytes: v.UsedBytes, AllocatedBytes: v.TotalBytes, MaxBytes: limit, UsagePercent: round1(v.UsedBytes / limit * 100)})
	}
	return out
}

// ReportDisk stores a host disk report for a profile and records it into
// the profile's series at once (reports may arrive more often than the
// collection cycle). Alerts are evaluated on the next cycle.
func (e *Engine) ReportDisk(ctx context.Context, profileID string, report HostDiskReport) error {
	if err := report.Validate(); err != nil {
		return err
	}
	report.ReportedAt = e.now()
	for i := range report.Volumes {
		report.Volumes[i].Mount = strings.TrimSpace(report.Volumes[i].Mount)
	}
	e.ensureSeries(ctx, profileID, report.ReportedAt)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.st.HostDisk == nil {
		e.st.HostDisk = map[string]*HostDiskReport{}
	}
	e.st.HostDisk[profileID] = &report
	e.series[profileID].record(report.ReportedAt, report.capacity())
	return e.saveStateLocked()
}

// Volume usage and forecasts while the report is fresh, a "reporting
// stopped" warning once it is not (the agent dying is how this check goes
// blind).
const volumeAdvice = "볼륨을 무엇이 채우는지 보세요. DB 점유량과 차이가 크면 DB 밖 파일(덤프 백업·외부 로그·코어 파일·다른 프로그램)이 원인이니 DB 서버에서 du 로 찾아 정리하세요. DB가 원인이면 예방 점검(WAL·슬롯·아카이브)과 급증 테이블을 확인하고, 추세가 정상이라면 고갈 전에 볼륨을 증설하세요."

// hostDiskConditions evaluates the latest report. dbBytes is the database's
// own footprint (0 when unknown); the difference to the volume is what fills
// it from outside PostgreSQL.
func hostDiskConditions(p dbconn.Profile, report *HostDiskReport, ps *profileSeries, now time.Time, staleAfter time.Duration, dbBytes float64) ([]Condition, []Forecast) {
	age := now.Sub(report.ReportedAt)
	if age > staleAfter {
		host := report.Host
		if host == "" {
			host = "DB 서버"
		}
		return []Condition{{
			Key: RuleHostDiskStale, Check: CheckHostDiskAgent, Rule: RuleHostDiskStale, Severity: SevWarning, Object: host,
			Title:          fmt.Sprintf("호스트 디스크 보고 중단: %s 마지막 보고 %s 전", host, humanDays(age.Hours()/24)),
			Detail:         "DB 서버의 디스크 보고 에이전트(sqlon-disk-report.sh)가 멈췄습니다. 그동안 PostgreSQL 밖의 파일로 디스크가 차는 것은 보이지 않습니다.",
			Recommendation: "DB 서버의 cron/systemd 타이머와 SQLON 접속(토큰·네트워크)을 확인하세요.",
			Value:          age.Seconds(), Threshold: staleAfter.Seconds(),
		}}, nil
	}
	snap := collector.Snapshot{Capacity: report.capacity()}
	fcs := buildForecasts(p, snap, ps, now)
	conds := capacityConditions(p, snap, fcs)
	host := prefixed(" · 호스트 ", report.Host)
	for i := range conds {
		conds[i].Recommendation = volumeAdvice
		if dbBytes > 0 {
			for _, v := range report.Volumes {
				if conds[i].Object == assetKey(collector.ScopeVolume, v.Mount) && v.UsedBytes > dbBytes {
					conds[i].Detail += fmt.Sprintf(" · DB 점유량 %s, DB 밖 파일 약 %s", humanBytes(dbBytes), humanBytes(v.UsedBytes-dbBytes))
				}
			}
		}
		conds[i].Detail += host
	}
	return conds, fcs
}
