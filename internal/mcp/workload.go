package mcp

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Workload report (DBA co-pilot). Aggregates the query audit log (query-*.jsonl)
// over a window into an operational profile: volume, latency percentiles, error
// breakdown, hottest tables, slowest statements, and busiest hours. Read-only —
// it summarizes what already ran; it changes nothing.

// mcpWorkloadReport builds a workload profile from the audit log.
func (s *Server) mcpWorkloadReport(profile string, days, slowMs int) map[string]any {
	if days <= 0 {
		days = 7
	}
	if slowMs <= 0 {
		slowMs = 200
	}
	cat, catSource := s.catalogFor(profile)

	dir := filepath.Join(s.opDir(), "audit")
	cutoff := time.Now().AddDate(0, 0, -days)

	var latencies []int64
	total, success, slow := 0, 0, 0
	byTable := map[string]int{}
	byErr := map[string]int{}
	byTool := map[string]int{}
	byProfile := map[string]int{}
	byHour := make([]int, 24)
	type slowRec struct {
		SQL       string `json:"sql"`
		ElapsedMs int64  `json:"elapsed_ms"`
		Profile   string `json:"profile,omitempty"`
	}
	worst := map[string]*slowRec{} // by sql_hash → worst sample

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "query-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		if day := strings.TrimSuffix(strings.TrimPrefix(name, "query-"), ".jsonl"); len(day) == 8 {
			if t, err := time.Parse("20060102", day); err == nil && t.Before(cutoff.Truncate(24*time.Hour)) {
				continue
			}
		}
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
		for sc.Scan() {
			var rec struct {
				Tool  string `json:"tool"`
				Entry struct {
					SQLText   string `json:"sql_text"`
					SQLHash   string `json:"sql_hash"`
					ProfileID string `json:"db_profile_id"`
					ElapsedMs int64  `json:"elapsed_ms"`
					Success   bool   `json:"success"`
					ErrorCode string `json:"error_code"`
					StartedAt string `json:"started_at"`
				} `json:"entry"`
			}
			if json.Unmarshal(sc.Bytes(), &rec) != nil {
				continue
			}
			en := rec.Entry
			if en.SQLText == "" {
				continue
			}
			if profile != "" && !strings.EqualFold(en.ProfileID, profile) {
				continue
			}
			total++
			byProfile[en.ProfileID]++
			if rec.Tool != "" {
				byTool[rec.Tool]++
			}
			if t, err := time.Parse(time.RFC3339, en.StartedAt); err == nil {
				byHour[t.Hour()]++
			}
			if en.Success {
				success++
				latencies = append(latencies, en.ElapsedMs)
				if en.ElapsedMs >= int64(slowMs) {
					slow++
				}
				for _, fqn := range cat.SQLTables(en.SQLText) {
					byTable[fqn]++
				}
				key := en.SQLHash
				if key == "" {
					key = en.SQLText
				}
				if w := worst[key]; w == nil || en.ElapsedMs > w.ElapsedMs {
					worst[key] = &slowRec{SQL: truncateAdvisor(en.SQLText, 200), ElapsedMs: en.ElapsedMs, Profile: en.ProfileID}
				}
			} else {
				code := en.ErrorCode
				if code == "" {
					code = "unknown"
				}
				byErr[code]++
			}
		}
		_ = f.Close()
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	pct := func(p float64) int64 {
		if len(latencies) == 0 {
			return 0
		}
		idx := int(p * float64(len(latencies)-1))
		return latencies[idx]
	}
	var sum int64
	for _, v := range latencies {
		sum += v
	}
	avg := int64(0)
	if len(latencies) > 0 {
		avg = sum / int64(len(latencies))
	}

	errCount := total - success
	errRate := 0.0
	if total > 0 {
		errRate = float64(errCount) / float64(total)
	}

	topSlow := make([]slowRec, 0, len(worst))
	for _, w := range worst {
		topSlow = append(topSlow, *w)
	}
	sort.Slice(topSlow, func(i, j int) bool { return topSlow[i].ElapsedMs > topSlow[j].ElapsedMs })
	if len(topSlow) > 10 {
		topSlow = topSlow[:10]
	}

	peakHour, peakN := -1, 0
	for h, n := range byHour {
		if n > peakN {
			peakHour, peakN = h, n
		}
	}

	res := map[string]any{
		"profile":         profile,
		"window_days":     days,
		"slow_ms":         slowMs,
		"total_queries":   total,
		"success":         success,
		"errors":          errCount,
		"error_rate":      round2(errRate),
		"slow_queries":    slow,
		"latency_ms":      map[string]any{"avg": avg, "p50": pct(0.50), "p95": pct(0.95), "p99": pct(0.99), "max": pct(1.0)},
		"top_tables":      topN(byTable, 10),
		"top_errors":      topN(byErr, 10),
		"by_tool":         topN(byTool, 10),
		"by_profile":      topN(byProfile, 10),
		"top_slow":        topSlow,
		"peak_hour":       peakHour,
		"peak_hour_count": peakN,
		"catalog_source":  catSource,
		"note":            "감사 로그 기반 워크로드 요약입니다(읽기 전용). 인덱스 후보는 suggest_indexes, 개별 문장 진단은 lint_sql 를 함께 사용하세요.",
	}
	return res
}

type countItem struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}

// topN returns the highest-count entries of m, descending, capped at n.
func topN(m map[string]int, n int) []countItem {
	items := make([]countItem, 0, len(m))
	for k, v := range m {
		items = append(items, countItem{Key: k, Count: v})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Count != items[j].Count {
			return items[i].Count > items[j].Count
		}
		return items[i].Key < items[j].Key
	})
	if len(items) > n {
		items = items[:n]
	}
	return items
}

func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }
