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

	cat := s.cat()
	q := cat.QualityReport()
	gate := cat.QualityGate()
	pass := 0
	if gate.Pass {
		pass = 1
	}
	writeProductMetrics(&b, "sqlon", "", tools, snap, len(cat.Tables), len(cat.Relations), q.OverallScore, pass)
	// One-release compatibility for existing dashboards and alerts.
	writeProductMetrics(&b, "jamypg", "Deprecated alias; use sqlon_* metrics. ", tools, snap, len(cat.Tables), len(cat.Relations), q.OverallScore, pass)

	_, _ = w.Write([]byte(b.String()))

	// DB connector metrics (per-profile query counts/latency/circuit state).
	if s.DB != nil {
		_, _ = io.WriteString(w, s.DB.PrometheusText())
	}
}

func writeProductMetrics(b *strings.Builder, prefix, helpPrefix string, tools []string, snap map[string]callStat, tables, relations int, quality float64, gatePass int) {
	fmt.Fprintf(b, "# HELP %s_up %s1 if the server is serving.\n", prefix, helpPrefix)
	fmt.Fprintf(b, "# TYPE %s_up gauge\n%s_up 1\n", prefix, prefix)
	fmt.Fprintf(b, "# HELP %s_build_info %sBuild version label.\n", prefix, helpPrefix)
	fmt.Fprintf(b, "# TYPE %s_build_info gauge\n%s_build_info{version=%q} 1\n", prefix, prefix, Version)
	fmt.Fprintf(b, "# HELP %s_tool_calls_total %sMCP tool invocations by status.\n", prefix, helpPrefix)
	fmt.Fprintf(b, "# TYPE %s_tool_calls_total counter\n", prefix)
	for _, tool := range tools {
		st := snap[tool]
		fmt.Fprintf(b, "%s_tool_calls_total{tool=%q,status=\"ok\"} %d\n", prefix, tool, st.calls-st.errors)
		fmt.Fprintf(b, "%s_tool_calls_total{tool=%q,status=\"error\"} %d\n", prefix, tool, st.errors)
	}
	fmt.Fprintf(b, "# HELP %s_tool_duration_ms_sum %sSummed tool latency in ms.\n", prefix, helpPrefix)
	fmt.Fprintf(b, "# TYPE %s_tool_duration_ms_sum counter\n", prefix)
	for _, tool := range tools {
		fmt.Fprintf(b, "%s_tool_duration_ms_sum{tool=%q} %d\n", prefix, tool, snap[tool].durMillis)
	}
	fmt.Fprintf(b, "# HELP %s_catalog_tables %sNumber of compiled catalog tables.\n", prefix, helpPrefix)
	fmt.Fprintf(b, "# TYPE %s_catalog_tables gauge\n%s_catalog_tables %d\n", prefix, prefix, tables)
	fmt.Fprintf(b, "# HELP %s_catalog_relations %sNumber of compiled join relations.\n", prefix, helpPrefix)
	fmt.Fprintf(b, "# TYPE %s_catalog_relations gauge\n%s_catalog_relations %d\n", prefix, prefix, relations)
	fmt.Fprintf(b, "# HELP %s_metadata_quality_score %sOverall metadata quality score (0-100).\n", prefix, helpPrefix)
	fmt.Fprintf(b, "# TYPE %s_metadata_quality_score gauge\n%s_metadata_quality_score %.1f\n", prefix, prefix, quality)
	fmt.Fprintf(b, "# HELP %s_metadata_quality_gate_pass %s1 if the release quality gate passes.\n", prefix, helpPrefix)
	fmt.Fprintf(b, "# TYPE %s_metadata_quality_gate_pass gauge\n%s_metadata_quality_gate_pass %d\n", prefix, prefix, gatePass)
}
