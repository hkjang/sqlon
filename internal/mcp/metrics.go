package mcp

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// Prometheus text-exposition metrics (improvement: 관측성). A dependency-free
// in-memory registry counts tool calls, errors, and latency, and /metrics
// renders them alongside catalog/quality gauges. No external client library.

type callStat struct {
	calls     int64
	errors    int64
	durMillis int64
}

type metricsRegistry struct {
	mu     sync.Mutex
	byTool map[string]*callStat
}

func newMetricsRegistry() *metricsRegistry {
	return &metricsRegistry{byTool: map[string]*callStat{}}
}

// recordToolMetric tallies one tool invocation. Safe for concurrent callers.
func (s *Server) recordToolMetric(tool string, durMillis int64, isErr bool) {
	if s.metrics == nil || tool == "" {
		return
	}
	s.metrics.mu.Lock()
	defer s.metrics.mu.Unlock()
	st := s.metrics.byTool[tool]
	if st == nil {
		st = &callStat{}
		s.metrics.byTool[tool] = st
	}
	st.calls++
	st.durMillis += durMillis
	if isErr {
		st.errors++
	}
}

func (s *Server) serveMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var b strings.Builder

	b.WriteString("# HELP jamypg_up 1 if the server is serving.\n")
	b.WriteString("# TYPE jamypg_up gauge\njamypg_up 1\n")

	b.WriteString("# HELP jamypg_build_info Build version label.\n")
	b.WriteString("# TYPE jamypg_build_info gauge\n")
	fmt.Fprintf(&b, "jamypg_build_info{version=%q} 1\n", Version)

	// tool-call counters
	s.metrics.mu.Lock()
	tools := make([]string, 0, len(s.metrics.byTool))
	for t := range s.metrics.byTool {
		tools = append(tools, t)
	}
	sort.Strings(tools)
	snap := make(map[string]callStat, len(tools))
	for _, t := range tools {
		snap[t] = *s.metrics.byTool[t]
	}
	s.metrics.mu.Unlock()

	b.WriteString("# HELP jamypg_tool_calls_total MCP tool invocations by status.\n")
	b.WriteString("# TYPE jamypg_tool_calls_total counter\n")
	for _, t := range tools {
		st := snap[t]
		ok := st.calls - st.errors
		fmt.Fprintf(&b, "jamypg_tool_calls_total{tool=%q,status=\"ok\"} %d\n", t, ok)
		fmt.Fprintf(&b, "jamypg_tool_calls_total{tool=%q,status=\"error\"} %d\n", t, st.errors)
	}
	b.WriteString("# HELP jamypg_tool_duration_ms_sum Summed tool latency in ms.\n")
	b.WriteString("# TYPE jamypg_tool_duration_ms_sum counter\n")
	for _, t := range tools {
		fmt.Fprintf(&b, "jamypg_tool_duration_ms_sum{tool=%q} %d\n", t, snap[t].durMillis)
	}

	// catalog gauges
	cat := s.cat()
	b.WriteString("# HELP jamypg_catalog_tables Number of compiled catalog tables.\n")
	b.WriteString("# TYPE jamypg_catalog_tables gauge\n")
	fmt.Fprintf(&b, "jamypg_catalog_tables %d\n", len(cat.Tables))
	b.WriteString("# HELP jamypg_catalog_relations Number of compiled join relations.\n")
	b.WriteString("# TYPE jamypg_catalog_relations gauge\n")
	fmt.Fprintf(&b, "jamypg_catalog_relations %d\n", len(cat.Relations))

	// metadata quality gauge (0..100) + release-gate pass
	q := cat.QualityReport()
	b.WriteString("# HELP jamypg_metadata_quality_score Overall metadata quality score (0-100).\n")
	b.WriteString("# TYPE jamypg_metadata_quality_score gauge\n")
	fmt.Fprintf(&b, "jamypg_metadata_quality_score %.1f\n", q.OverallScore)
	gate := cat.QualityGate()
	pass := 0
	if gate.Pass {
		pass = 1
	}
	b.WriteString("# HELP jamypg_metadata_quality_gate_pass 1 if the release quality gate passes.\n")
	b.WriteString("# TYPE jamypg_metadata_quality_gate_pass gauge\n")
	fmt.Fprintf(&b, "jamypg_metadata_quality_gate_pass %d\n", pass)

	_, _ = w.Write([]byte(b.String()))

	// DB connector metrics (per-profile query counts/latency/circuit state).
	if s.DB != nil {
		_, _ = io.WriteString(w, s.DB.PrometheusText())
	}
}
