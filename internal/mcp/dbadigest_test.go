package mcp

import "strings"

import "testing"

func TestDBADigestHeadline(t *testing.T) {
	// no traffic → neutral message
	if got := dbaDigestHeadline(0, 0, 0, 0, 0); !strings.Contains(got, "쿼리가 없습니다") {
		t.Fatalf("empty headline = %q", got)
	}
	// healthy: no errors, no slow, no candidates → ✅
	h := dbaDigestHeadline(100, 0, 0, 12, 0)
	if !strings.HasPrefix(h, "✅") {
		t.Fatalf("healthy headline should start with ✅: %q", h)
	}
	if !strings.Contains(h, "쿼리 100건") || !strings.Contains(h, "p95 12ms") {
		t.Fatalf("headline missing volume/latency: %q", h)
	}
	// attention: index candidates present → ⚠️ and mentions candidates
	w := dbaDigestHeadline(100, 0.2, 3, 800, 5)
	if !strings.HasPrefix(w, "⚠️") {
		t.Fatalf("headline with candidates should warn: %q", w)
	}
	if !strings.Contains(w, "오류율 20%") || !strings.Contains(w, "인덱스 후보 5건") || !strings.Contains(w, "느린쿼리 3건") {
		t.Fatalf("warn headline missing parts: %q", w)
	}
}
